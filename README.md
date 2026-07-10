# webdesktop

`webdesktop` captures an existing KDE Plasma Wayland desktop for a future
WebRTC transport. It uses the xdg-desktop-portal ScreenCast flow to obtain one
monitor stream, passes the portal PipeWire remote to a dynamic GStreamer
pipeline, and emits software-encoded VP8 or H.264 frames through a Go media API.

WebRTC signaling, Pion peer connections, input, audio, remote unlock, and a
frontend are not implemented.

## Commands

```text
webdesktop serve [flags]
webdesktop version
```

The running service exposes `GET /healthz`. The health server starts while
desktop capture authorization is pending.

## Plasma portal authorization

`webdesktop serve` requests monitor capture from the desktop portal. KDE Plasma
shows its normal screen-sharing dialog in the active graphical session. A user
must be present in the active, unlocked session to select and authorize the
monitor.

`webdesktop` does not request persistent portal permission and cannot authorize
capture at the login or lock screen. It does not provide unattended remote
unlock. If the user denies or cancels the portal dialog, the service exits.

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
WEBDESKTOP_VIDEO_SOURCE
WEBDESKTOP_VIDEO_CURSOR_MODE
WEBDESKTOP_VIDEO_CODEC
WEBDESKTOP_VIDEO_WIDTH
WEBDESKTOP_VIDEO_HEIGHT
WEBDESKTOP_VIDEO_FRAMERATE
WEBDESKTOP_VIDEO_BITRATE_KBPS
WEBDESKTOP_VIDEO_TUNING_THREADS
WEBDESKTOP_VIDEO_TUNING_KEYFRAME_INTERVAL
WEBDESKTOP_VIDEO_TUNING_VP8_CPU_USED
WEBDESKTOP_VIDEO_TUNING_H264_SPEED_PRESET
```

Run `webdesktop serve --help` for matching command flags.

The implemented source is `monitor`. Cursor mode can be `embedded` or `hidden`.
VP8 is the default codec. H.264 uses the software `x264enc` encoder with
constrained-baseline byte-stream output. Width and height are applied after
software color conversion, frame-rate normalization, and scaling.

The `media.Service` Go API exposes encoded samples and `UpdateQuality`.
Bitrate-only changes update the live encoder. Codec, resolution, or frame-rate
changes stop and rebuild the pipeline against the same portal session. Stage 2
does not expose this control over HTTP, WebSocket, or a WebRTC data channel.

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
