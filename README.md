# Fyne Terminal Slowness

For <https://github.com/fyne-io/terminal/issues/124>, spun out of
<https://github.com/fyne-io/terminal/pull/121#issuecomment-3184055092>

## Instructions

- Make sure you have docker available
- `docker pull debian:stable-slim`
- `go run .`
- Click the `Run!` button

PR fyne-io/terminal#121 _helps_, but is not a panacea. To see the impact:

- Uncomment the `replace` directive in `go.mod`
- Run `go mod tidy`
- Run the demo app again
