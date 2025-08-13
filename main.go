package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	"github.com/docker/docker/api/types"
	dockerContainer "github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
	"github.com/fyne-io/terminal"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sys/unix"
)

func main() {
	ctx, stop := context.WithCancel(context.Background())
	defer stop()

	a := app.NewWithID("com.github.mgabeler-lee-6rs.fyne-terminal-slow")

	sigCtx, sigCancel := signal.NotifyContext(ctx, os.Interrupt, os.Kill, syscall.SIGTERM)
	defer sigCancel()

	s := &AppState{
		ctx: sigCtx,
		app: a,
	}
	s.createMainWindow()

	s.mainWindow.Show()
	s.app.Run()
}

type AppState struct {
	ctx        context.Context
	app        fyne.App
	mainWindow fyne.Window
	terminal   *terminal.Terminal
	termSize   *termSizeTracker
}

func (s *AppState) createMainWindow() {
	w := s.app.NewWindow("Slow Terminal Demo")
	s.mainWindow = w

	content := container.NewBorder(
		// top
		widget.NewButtonWithIcon("Run!", theme.DownloadIcon(), s.run),
		nil, // bottom
		nil, // left
		nil, // right
		// center
		newTerminal(s),
	)

	w.SetContent(content)
	w.SetMaster()
	w.Resize(fyne.NewSize(1280, 720))
	w.CenterOnScreen()
}

func newTerminal(s *AppState) fyne.CanvasObject {
	t := terminal.New()

	s.terminal = t
	s.termSize = newTermSizeTracker(t)
	return t
}

type termSizeTracker struct {
	ch         chan terminal.Config
	mu         sync.Mutex
	rows, cols uint
}

func newTermSizeTracker(t *terminal.Terminal) *termSizeTracker {
	tracker := &termSizeTracker{
		ch: make(chan terminal.Config, 1),
	}
	go func() {
		for cfg := range tracker.ch {
			tracker.mu.Lock()
			tracker.rows, tracker.cols = cfg.Rows, cfg.Columns
			tracker.mu.Unlock()
		}
	}()
	t.AddListener(tracker.ch)
	return tracker
}

func (t *termSizeTracker) LastSize() (rows uint, cols uint) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.rows, t.cols
}

func (s *AppState) run() {
	go s.reallyRun()
}

func (s *AppState) reallyRun() {
	getTermSize := func() (uint, uint, error) {
		r, c := s.termSize.LastSize()
		if r == 0 || c == 0 {
			return 25, 80, errors.New("terminal size unknown")
		}
		return r, c, nil
	}

	// two pipes, one for reading from the terminal, one for writing to it
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	go func() {
		must(s.terminal.RunWithConnection(stdinW, stdoutR))
	}()

	defer stdinR.Close()

	_, _ = fmt.Fprint(stdoutW, "\033[H\033[2J\033[3J") // clear the screen
	_, _ = fmt.Fprint(stdoutW, "Asked to do the thing\r\n")

	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	dc, err := newRawDockerClient()
	if err != nil {
		panic(err)
	}

	defer dc.Close()

	err = dockerRun(ctx, dc, getTermSize, stdinR, stdoutW)
	if err != nil {
		fyne.Do(func() {
			dialog.NewError(fmt.Errorf("run failed: %w", err), s.mainWindow).Show()
		})
		return
	}
}

func newRawDockerClient() (*client.Client, error) {
	return client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
}

func dockerRun(
	ctx context.Context,
	dc *client.Client,
	getTermSize func() (rows, cols uint, err error),
	stdin io.Reader,
	stdout io.Writer,
) (finalErr error) {
	config := &dockerContainer.Config{
		StdinOnce:    true,
		OpenStdin:    true,
		AttachStdout: true,
		AttachStderr: true,
		Tty:          true,
		Cmd: []string{
			// The real app runs a container that does a bunch of stuff and emits a
			// lot of output. Here we just spew out some convenient text to replicate the scale
			// of output it would generate.
			"/bin/sh", "-c",
			"apt-get update ; apt-get -y install lz4 ; lz4cat /var/lib/apt/lists/*_Packages.lz4",
		},
		Image: "debian:stable-slim",
	}
	mounts := []mount.Mount{
		// real app does some stuff here
	}
	hostConfig := &dockerContainer.HostConfig{
		Mounts:     mounts,
		Privileged: true,
		AutoRemove: true,
	}

	return runContainer(ctx, dc, config, hostConfig, getTermSize, stdin, stdout)
}

func runContainer(
	ctx context.Context,
	dc *client.Client,
	cfg *dockerContainer.Config,
	hostCfg *dockerContainer.HostConfig,
	getTermSize func() (rows, cols uint, err error),
	stdin io.Reader,
	stdout io.Writer,
) (finalErr error) {
	cfg.AttachStdout = true
	cfg.AttachStderr = true
	cfg.Tty = true
	cfg.Env = append(cfg.Env, "TERM=xterm-256color")

	created, err := dc.ContainerCreate(
		ctx,
		cfg,
		hostCfg,
		nil,
		nil,
		"",
	)
	if err != nil {
		return fmt.Errorf("failed to create %s container: %w", cfg.Image, err)
	}

	deleted := false
	deleteContainer := func() error {
		deleted = true
		// don't let context cancellation prevent us from deleting the container
		err := dc.ContainerRemove(context.Background(), created.ID, dockerContainer.RemoveOptions{Force: true})
		if err != nil {
			return fmt.Errorf("failed to remove %s container: %w", cfg.Image, err)
		}
		return nil
	}
	defer func() {
		if !deleted {
			err := deleteContainer()
			if err != nil {
				finalErr = errors.Join(finalErr, err)
			}
		}
	}()

	// attach before starting so we get all the info
	attachOpts := dockerContainer.AttachOptions{
		Stream: true,
		Stdout: true,
		Stderr: true,
	}
	attached, err := dc.ContainerAttach(ctx, created.ID, attachOpts)
	if err != nil {
		// TODO: delete the container
		return fmt.Errorf("unable to attach to %s container: %w", cfg.Image, err)
	}

	eg, egCtx := errgroup.WithContext(ctx)
	// run IO concurrent with waiter
	eg.Go(func() error {
		defer attached.Close()
		if err := interactiveTTY(egCtx, attached, getTermSize,
			func(ctx context.Context, r dockerContainer.ResizeOptions) error {
				return dc.ContainerResize(ctx, created.ID, r)
			},
			func(ctx context.Context, s os.Signal) error {
				return dc.ContainerKill(ctx, created.ID, unix.SignalName(s.(unix.Signal)))
			},
			stdin, stdout,
		); err != nil {
			return fmt.Errorf("failed doing io to %s container: %w", cfg.Image, err)
		}
		return nil
	})
	// waiter needs to start before we start the container
	waiting := make(chan struct{})
	// shouldn't report wait errors until we've had a chance to report start errors
	started := make(chan struct{})
	ended := make(chan struct{})
	exitCode := -1
	eg.Go(func() error {
		defer close(ended)
		// container should stop on its own, wait for it and then remove it
		onStopped, onErr := dc.ContainerWait(ctx, created.ID, dockerContainer.WaitConditionRemoved)
		close(waiting)
		<-started
		select {
		case <-egCtx.Done():
			return egCtx.Err()
		case stopped := <-onStopped:
			// we used autoremove so the container is gone now
			deleted = true
			exitCode = int(stopped.StatusCode)
			if stopped.Error != nil {
				return fmt.Errorf(
					"failed waiting for %s container to stop: %s (%d)",
					cfg.Image,
					stopped.Error.Message,
					stopped.StatusCode,
				)
			} else {
				// stopped gracefully (though maybe with a non-zero exit code)
				return nil
			}
		case err := <-onErr:
			return fmt.Errorf("failed waiting for %s container to stop/delete: %w", cfg.Image, err)
		}
	})
	eg.Go(func() error {
		defer close(started)
		// don't start container until the waiter is started, so the waiter is sure
		// to see what happens
		select {
		case <-egCtx.Done():
			return egCtx.Err()
		case <-waiting:
			// continue with start
		}
		err = dc.ContainerStart(ctx, created.ID, dockerContainer.StartOptions{})
		if err != nil {
			return fmt.Errorf("unable to start %s container: %w", cfg.Image, err)
		}
		return nil
	})
	eg.Go(func() error {
		// watch for context cancellation and terminate the container if so
		select {
		case <-ended:
			// don't kill it if it ends on its own
			return nil
		case <-egCtx.Done():
			return deleteContainer()
		}
	})

	err = eg.Wait()

	if exitCode != 0 {
		err = errors.Join(err, fmt.Errorf("container returned non-zero exit code %d", exitCode))
	}

	_, _ = fmt.Fprintf(stdout, "\r\n\r\nContainer exited with code %d\r\n", exitCode)

	return err
}

func interactiveTTY(
	ctx context.Context,
	attached types.HijackedResponse,
	getTermSize func() (rows, cols uint, err error),
	resizer func(context.Context, dockerContainer.ResizeOptions) error,
	signaller func(context.Context, os.Signal) error,
	stdin io.Reader,
	stdout io.Writer,
) error {
	// compare to:
	// https://github.com/docker/cli/blob/master/cli/command/container/run.go
	// https://github.com/docker/cli/blob/master/cli/command/container/hijack.go
	// https://github.com/docker/cli/blob/master/vendor/github.com/moby/term/term.go

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	eg, egCtx := errgroup.WithContext(ctx)

	/*
		// 1. put tty in raw mode
		inState, err := term.MakeRaw(stdinFd)
		if err != nil {
			return fmt.Errorf("unable to put tty in raw mode: %w", err)
		}
		restoreIn := func() error {
			if inState == nil {
				return nil
			}
			if err := term.Restore(stdinFd, inState); err != nil {
				return err
			}
			inState = nil
			return nil
		}
		//nolint:errcheck
		defer restoreIn()
		outState, err := term.MakeRaw(stdoutFd)
		if err != nil {
			return fmt.Errorf("unable to put tty in raw mode: %w", err)
		}
		restoreOut := func() error {
			if outState == nil {
				return nil
			}
			if err := term.Restore(stdoutFd, outState); err != nil {
				return err
			}
			outState = nil
			return nil
		}
		//nolint:errcheck
		defer restoreOut()
	*/

	// 2. setup signal forwarder
	// 3. setup tty size forwarder
	signals := make(chan os.Signal, 16 /* arbitrary */)
	signal.Notify(signals)
	defer signal.Stop(signals)
	resizeTty := func() error {
		if w, h, err := getTermSize(); err != nil {
			return err
		} else {
			return resizer(egCtx, dockerContainer.ResizeOptions{Width: w, Height: h})
		}
	}
	eg.Go(func() error {
		// resize won't work at first, need to retry it until it succeeds
		resizeRetry := time.NewTicker(100 * time.Millisecond)
		tryResize := func() {
			if err := resizeTty(); err == nil {
				resizeRetry.Stop()
			}
		}
		tryResize()
		for {
			select {
			case <-egCtx.Done():
				return nil
			case <-resizeRetry.C:
				tryResize()
			case s := <-signals:
				// runtime uses SIGURG for scheduling
				if s == unix.SIGCHLD || s == unix.SIGPIPE || s == unix.SIGURG {
					continue
				}
				if err := signaller(egCtx, s); err != nil {
					return fmt.Errorf("failed to forward signal %v: %w", s, err)
				}
				if s == unix.SIGWINCH {
					if err := resizeTty(); err != nil {
						return err
					}
				}
			}
		}
	})

	// 4. start routine to copy raw input to attached.Conn
	eg.Go(func() error {
		// we can't wait on the input because we can't interrupt the read from stdin
		errCh := make(chan error, 1)
		// TODO: this is only safe when the parent process isn't going to do
		// anything else, otherwise this may eat at least one byte from stdin after
		// the container is terminated
		go func() {
			// obeying context cancellation here is hard, because TTY fds don't support
			// deadlines
			_, err := io.Copy(attached.Conn, stdin)
			if errors.Is(err, net.ErrClosed) {
				// ignore this, just means the connection was closed (container stopped)
				// while we were doing i/o
				err = nil
			}
			if cwErr := attached.CloseWrite(); cwErr != nil && !errors.Is(cwErr, net.ErrClosed) {
				err = errors.Join(err, cwErr)
			}
			errCh <- err
		}()
		select {
		case err := <-errCh:
			return err
		case <-egCtx.Done():
			return nil
		}
	})

	// 5. start routine to copy raw output from attached.Conn
	eg.Go(func() error {
		// when output ends, everything else should end too
		defer cancel()
		// obeying context cancellation here is hard, because TTY fds don't support
		// deadlines
		_, err := io.Copy(stdout, attached.Reader)
		if errors.Is(err, net.ErrClosed) {
			// ignore this, just means the connection was closed (container stopped)
			// while we were doing i/o
			err = nil
		}
		// if output ends no point in accepting more input, close everything
		attached.Close()
		return err
	})

	// 6. wait
	err := eg.Wait()

	/*
		// 7. restore everything
		if rErr := restoreOut(); rErr != nil {
			err = errors.Join(err, rErr)
		}
		if rErr := restoreIn(); rErr != nil {
			err = errors.Join(err, rErr)
		}
	*/

	return err
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
