# webdesktop

`webdesktop` is the service scaffold for sharing an existing KDE Plasma
Wayland desktop over WebRTC. Stage 1 provides the binary, static configuration,
structured logging, and HTTP lifecycle. Desktop capture, WebRTC signaling,
input, audio, and a frontend are not implemented yet.

## Commands

```text
webdesktop serve [flags]
webdesktop version
```

The running service exposes `GET /healthz`.

## Configuration

Copy `webdesktop.example.yaml` and pass it explicitly:

```bash
webdesktop serve --config ./webdesktop.yaml
```

Configuration precedence is:

1. command flags
2. `WEBDESKTOP_*` environment variables
3. the explicit config file
4. built-in defaults

Available environment variables are:

```text
WEBDESKTOP_SERVER_LISTEN_ADDRESS
WEBDESKTOP_SERVER_SHUTDOWN_TIMEOUT
WEBDESKTOP_LOGGING_LEVEL
WEBDESKTOP_LOGGING_FORMAT
```

Run `webdesktop serve --help` for matching command flags.

## Development and build

Enter the development environment:

```bash
nix develop
```

Build the Go packages or the Nix package:

```bash
go build ./...
nix build .#
```
