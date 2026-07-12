# webdesktop

`webdesktop` streams an existing KDE Plasma Wayland desktop over WebRTC. It
uses xdg-desktop-portal ScreenCast for monitor capture, owns the portal
PipeWire remote for the lifetime of one persistent capture pipeline, and feeds
the newest captured frame to an independent encoder pipeline. VP8 uses a
software encoder. H.264 uses GStreamer's VA encoder. Remote pointer and keyboard
events use the xdg-desktop-portal RemoteDesktop interface, `ConnectToEIS`, and
the system libei library. Optional desktop audio uses `pulsesrc` through
PipeWire's PulseAudio-compatible server and software Opus encoding.

The service includes an embedded production SPA, WebSocket signaling, and a
versioned data-channel protocol for video quality and exclusive input control.
It has no remote unlock, built-in authentication, or TLS termination. Audio is
disabled by default.

## Commands

```text
webdesktop serve [flags]
webdesktop version
```

The default HTTP address is `127.0.0.1:8080`. The running service exposes:

- `GET /healthz`
- `GET /api/config`
- `GET /api/status`
- `GET /webrtc`, upgraded to the signaling WebSocket by default
- `GET /`, the embedded SPA with history fallback

The health endpoint starts while desktop capture authorization is pending.

## Plasma portal authorization

The packaged application ID is `io.github.tarik02.webdesktop`. Before its first
portal call, the service registers that identity on its D-Bus connection
through `org.freedesktop.host.portal.Registry`. The package installs the
matching `io.github.tarik02.webdesktop.desktop` entry. Its `Exec` command starts
the same packaged binary used by the systemd service.

Input is enabled by default. `webdesktop serve` creates a RemoteDesktop portal
session, calls ScreenCast `SelectSources` for one monitor, calls RemoteDesktop
`SelectDevices` for the configured pointer and keyboard classes, and calls
RemoteDesktop `Start` once. The Start response supplies both the selected
devices and ScreenCast stream. The service then opens the PipeWire remote and
calls `ConnectToEIS`. Media and input use that one authorized portal session.
Closing it stops both.

The service requests portal persistence until explicitly revoked. The normal
bootstrap on KDE Plasma 6.7.1 is one approval in the active unlocked session.
Keep "Allow restoring on future sessions" checked and select Approve.

An operator can instead bootstrap a trusted unattended installation with KDE's
application-specific remote-desktop permission:

```bash
flatpak permission-set \
  kde-authorized remote-desktop \
  io.github.tarik02.webdesktop yes
systemctl --user restart webdesktop.service
```

Wait until `/api/status` reports `ready: true` and the restore state exists,
then remove the bootstrap permission and restart:

```bash
flatpak permission-remove \
  kde-authorized remote-desktop \
  io.github.tarik02.webdesktop
systemctl --user restart webdesktop.service
```

This grants only the packaged application ID and leaves the portal in the
capture and input path. It does not disable consent globally or call KWin's
private screenshot or EIS interfaces. Removing the permission after the first
successful launch proves later unattended starts use the portal restore token.
The webdesktop process never changes KDE's permission store itself.

Either bootstrap path makes the portal return a single-use restore token.
Webdesktop stores it at
`$XDG_STATE_HOME/webdesktop/portal-restore.json`, or
`~/.local/state/webdesktop/portal-restore.json` when `XDG_STATE_HOME` is unset.
The state file and directory use modes 0600 and 0700.

Each later launch keeps the old token until the portal returns, then atomically
writes the replacement token. An interrupted launch can therefore retry the
old token if KDE had not consumed it yet. The stored state includes the stable
application ID, monitor sharing, and the requested pointer and keyboard
capabilities. Changing those values discards the old token and opens the normal
consent flow. If KDE cannot restore a token, it prompts normally.

KWin's `X-KDE-Wayland-Interfaces` and
`X-KDE-DBUS-Restricted-Interfaces` desktop fields are not required for this
application. Webdesktop does not connect to KWin's restricted protocols or
D-Bus screenshot API directly. The KDE portal owns those compositor-facing
permissions.

Set `input.enabled: false` for the ScreenCast-only CreateSession,
SelectSources, Start, and OpenPipeWireRemote flow. Webdesktop cannot authorize
capture at the login or lock screen and does not provide unattended remote
unlock. Input works only while the authorized Plasma session is active and
unlocked. If the user denies or cancels the portal dialog, the service exits
and closes signaling and all peers.

Desktop audio is gated by the same authorization. The Pulse monitor is not
opened until the portal Start flow succeeds and optional EIS setup completes.
While consent is pending, health and signaling remain available but the audio
track is silent. Denial or portal setup failure prevents audio capture from
starting.

## WebRTC behavior

Each peer registers only the codec selected when that connection starts. VP8
frames use the VP8 RTP payloader. H.264 uses constrained-baseline Annex-B
access units from `vah264enc` and packetization mode 1. WebRTC signaling uses
the libwebrtc-compatible constrained-baseline identifier
`profile-level-id=42e02a`, Level 4.2, for the lifetime of every peer
connection. Browser offers such as `42e01f` are accepted when they include
`level-asymmetry-allowed=1`; the answer advertises `42e02a`. Offers at Level
4.2 or higher do not need level asymmetry.

The GStreamer caps force constrained-baseline Level 4.2. The encoder writes
`42c02a` in the SPS. `42c02a` and the negotiated `42e02a` both identify
constrained-baseline Level 4.2 bitstreams; the extra `42e0` constraint flag is
the canonical form used by libwebrtc SDP and lets Pion match browser offers.

All peers share one encoded video stream and, when enabled, one encoded audio
stream. The persistent PipeWire pipeline copies each raw buffer into a
capture appsink that keeps only its newest sample. An application-owned
single-sample slot feeds each independent encoder through a one-buffer leaky
appsrc. Encoder slowdown therefore drops obsolete raw frames without blocking
PipeWire or accumulating latency. Encoded samples use blocking, unbuffered
handoffs through media fanout and each peer writer. There is no per-peer video
queue or packet pacer. A peer waiting for its first decodable frame ignores
inter-frames until the requested keyframe arrives. Connecting a viewer does
not create another encoder pipeline. Each WebSocket owns one peer connection,
and the default maximum is two peers.

Bitrate-only changes update the active encoder while it is PLAYING. H.264
updates its requested CPB size and target bitrate together. `vah264enc` clamps
the CPB to its HRD-compliant minimum and reports the effective value in trace
snapshots. Resolution, frame-rate, and codec changes build a new encoder
pipeline against the same latest-frame slot. The candidate receives the
current frame immediately, starts encoding, and waits for an IDR before the
service activates it and retires the previous encoder. PipeWire capture
remains linked and PLAYING throughout the change. A candidate that fails or
does not produce an IDR within five seconds is discarded without replacing
the active encoder. Bitrate, resolution, and frame-rate changes keep the
existing peer when its codec remains compatible. Codec changes require a new
SDP exchange.

RTP writes are immediate. Pion retains recent RTP for NACK retransmission. VP8
uses a short rate-control buffer, bounded intra frames, the fastest realtime
CPU setting, and eight encoder threads by default. H.264 uses VA CBR rate
control with GStreamer's HRD-compliant CPB, no B-frames, one reference frame,
four slices, disabled CABAC and macroblock bitrate control, and
constrained-baseline output.

`pipewiresrc` copies portal buffers and uses a keepalive interval derived from
the configured frame rate. When KWin's portal stream is damage-driven, the
source resends its latest buffer instead of leaving the encoder and RTP stream
idle. `videorate` remains drop-only and caps each encoder branch at the
configured rate. The capture appsink, latest-frame slot, and encoder appsrc all
discard obsolete raw frames. The encoded appsink is bounded and non-leaky so
it does not discard part of an encoded reference chain.

Video RTP timing follows the monotonic production gap between encoded samples.
Before packetizing the current access unit, the track skips exactly that
elapsed RTP time and gives the packetizer no additional nominal frame advance.
GStreamer PTS remains available for diagnostics, but PTS duplicates and the
reset from each replacement pipeline cannot distort the RTP clock. Audio RTP
continues to advance from each Opus sample duration.

The service reads RTCP from every video and audio sender. PLI, FIR, and a newly
connected peer request a keyframe from the active GStreamer video encoder. The
encoder receives an upstream force-key-unit event with headers requested,
which works for both VP8 and H.264. Pion's sender-report interceptor maps each
track's RTP clock to NTP time. The tracks use the same WebRTC media stream ID.
This gives browsers the standard A/V clock correlation, but capture latency
between the independent pipelines is not calibrated for perfect lip sync.

## Optional desktop audio

Audio requires PipeWire with the `pipewire-pulse` compatibility daemon and the
GStreamer `pulsesrc` and `opusenc` elements. The default device is
`@DEFAULT_MONITOR@`, the PulseAudio protocol name for the monitor of the
current default sink. A configured explicit source must end in `.monitor`.
`webdesktop` rejects other device names instead of falling back to a
microphone. Use `pactl list short sources` in the graphical user session to
find an explicit monitor source.

The pipeline converts and resamples to stereo S16LE at 48 kHz, then encodes
20 ms Opus frames at the configured bitrate. Capture callbacks only enqueue
into a bounded channel. The service watches the resolved Pulse source for the
full pipeline lifetime. Moving an explicit monitor stream elsewhere, resolving
`@DEFAULT_MONITOR@` to a non-monitor source, source disappearance, PulseAudio
failure, plugin failure, or encoder failure terminates the application and
tears down the portal session and peers. With `audio.enabled: false`, no audio
device is opened and the video/input behavior is unchanged.

## Signaling protocol

Signaling uses valid UTF-8 WebSocket text messages with JSON protocol version
1. Required fields are presence-aware. Missing fields and `null` values are
different errors. Unknown fields, unknown nested fields, multiple JSON values,
binary messages, invalid UTF-8, and messages larger than 128 KiB are rejected.
One socket accepts one offer and does not perform renegotiation.

The client has 10 seconds after the WebSocket upgrade to send its offer.
The server sends a ping every 5 seconds after upgrade and requires pongs within
15 seconds after the offer. Initial pongs do not extend the offer deadline.
Timeouts release the peer slot. Shutdown sends a bounded WebSocket close frame
before closing the socket.

The client must create a recv-only video transceiver and a reliable ordered
data channel named `control` before creating its offer. When audio is enabled,
the offer must contain an active `recvonly` or `sendrecv` audio media section
with Opus at 48 kHz and two channels. Missing, rejected, inactive, send-only,
or incompatible audio sections fail as `invalid_offer` before Pion installs
the remote description. The registered Opus payload type is 111. When audio is
disabled, no audio codec or track is registered and existing video-only
clients keep working. A client that wants input must also create one reliable
ordered data channel named `input`. Creating a data channel ensures that the
offer contains the SCTP media section. The server rejects other labels,
duplicate channels, and either channel when configured for unordered or
partial-reliable delivery.

Client offer:

```json
{
  "version": 1,
  "type": "offer",
  "sdp": "v=0\r\n..."
}
```

Server answer:

```json
{
  "version": 1,
  "type": "answer",
  "sdp": "v=0\r\n..."
}
```

ICE candidate in either direction:

```json
{
  "version": 1,
  "type": "ice-candidate",
  "candidate": {
    "candidate": "candidate:...",
    "sdpMid": "0",
    "sdpMLineIndex": 0,
    "usernameFragment": "..."
  }
}
```

Clients may send candidates before or after the offer. The server queues
pre-offer candidates until it installs the remote description. Server
candidates are held until the answer has been written. Browser clients should
ignore the final `icecandidate` event where `event.candidate` is `null`.

When `tracing.enabled` is true, the embedded client sends bounded structured
diagnostics over the same signaling socket:

```json
{
  "version": 1,
  "type": "client-log",
  "level": "debug",
  "event": "performance.snapshot",
  "details": {
    "fps": "30.0",
    "bitrate_bps": "3864000",
    "rtt_ms": "4.1"
  }
}
```

Levels are `debug`, `info`, `warn`, or `error`. Event names are limited to 128
bytes. Details are string values with at most 32 entries, 64 bytes per key, and
512 bytes per value. The embedded client never sends SDP, full ICE candidates,
key values, pointer coordinates, or per-frame and per-motion events. The server
rejects `client-log` when tracing is disabled.

Structured signaling error:

```json
{
  "version": 1,
  "type": "error",
  "error": {
    "code": "invalid_offer",
    "message": "..."
  }
}
```

The peer limit is reported as `peer_limit` on an upgraded WebSocket before it
is closed.

## Control data channel

The client-created `control` channel accepts UTF-8 text messages up to 16 KiB.
Protocol version 1 handles video quality and the exclusive input lease.

A request has a caller-selected ID:

```json
{
  "version": 1,
  "id": "quality-42",
  "type": "video.quality.set",
  "quality": {
    "width": 1600,
    "height": 900,
    "framerate": 30,
    "bitrate_kbps": 3500
  }
}
```

Quality fields are optional and merge with the current settings. At least one
field must be present. A successful response returns the effective full
quality:

```json
{
  "version": 1,
  "id": "quality-42",
  "type": "video.quality.set.result",
  "ok": true,
  "quality": {
    "codec": "vp8",
    "width": 1600,
    "height": 900,
    "framerate": 30,
    "bitrate_kbps": 3500
  }
}
```

Errors preserve the request ID:

```json
{
  "version": 1,
  "id": "quality-42",
  "type": "error",
  "ok": false,
  "error": {
    "code": "quality_update_failed",
    "message": "..."
  }
}
```

`codec` accepts `vp8` or `h264`. A codec update starts a replacement encoder
branch and switches after its first IDR. The browser client then reconnects
automatically because the new codec needs a new SDP offer and answer. Other
peers using the old codec are closed. Unknown message types and fields are
rejected.

H.264 quality must remain inside Level 4.2 for the full peer lifetime:

- no more than 263 rounded macroblocks in either frame dimension
- no more than 8704 macroblocks per frame
- no more than 522240 macroblocks per second
- no more than 50000 Kbit/s

Macroblock dimensions round width and height up to multiples of 16. For
example, 1920x1080 at 60 fps uses 489600 macroblocks per second and fits. A
7680x240 frame exceeds the 263-macroblock width limit even though its total
macroblock count fits. Startup and control updates reject such dimensions
before changing the active pipeline.
An incompatible control update returns `h264_level_incompatible` without
changing the stream.

Quality is global because every viewer receives the same encoded stream. A
quality update from one peer changes the stream for every peer. Bitrate-only
updates retune the active encoder. H.264 updates its requested CPB size before
its target bitrate, and `vah264enc` applies its HRD minimum. Resolution and
frame-rate changes use an IDR-gated replacement encoder branch. The persistent
PipeWire capture pipeline remains active and feeds both branches during the
short overlap. A same-codec update does not create another peer.

VP8 bitrate accepts any whole Kbit/s value from 100 through 2147483, the
largest target that fits the encoder's signed 32-bit bits-per-second property.
H.264 bitrate accepts 100 through 50000 Kbit/s under the negotiated Level 4.2
limit.

Only one peer can own input because every viewer shares the same desktop. A
connected peer with an open `input` channel acquires the lease through
`control`:

```json
{
  "version": 1,
  "id": "input-1",
  "type": "input.acquire"
}
```

Successful acquisition reports the authorized configured classes:

```json
{
  "version": 1,
  "id": "input-1",
  "type": "input.acquire.result",
  "ok": true,
  "input": {
    "pointer": true,
    "keyboard": true
  }
}
```

Another peer receives an `input_busy` error. Acquisition can also return
`input_disabled`, `input_pointer_unauthorized`,
`input_keyboard_unauthorized`, `input_not_ready`,
`input_channel_required`, or `peer_not_connected`.

The owner releases the lease with:

```json
{
  "version": 1,
  "id": "input-2",
  "type": "input.release"
}
```

The result type is `input.release.result`. Closing either data channel, closing
the peer, portal closure, overload, and service shutdown also release the
lease. EIS setup or runtime transport failure releases the lease and terminates
the shared desktop session instead of degrading to view-only operation. The
service emits releases for all held keys and buttons while their EIS devices
remain available.

## Input data channel

The client-created `input` channel accepts UTF-8 JSON text messages up to
4 KiB. Every message requires protocol version 1, a sequence greater than zero,
and a sequence strictly larger than the previous valid input message on that
channel. Unknown fields, fields from another event type, missing fields, `null`,
multiple JSON values, invalid UTF-8, binary messages, and non-finite values are
rejected.

Normalized absolute motion:

```json
{
  "version": 1,
  "sequence": 1,
  "type": "input.pointer.motion.absolute",
  "x": 0.5,
  "y": 0.25
}
```

`x` and `y` are in `[0,1]`. The service maps them through the active libei
absolute region paired with the ScreenCast `mapping_id`. If no mapping ID is
available, it maps through the active region layout.

Relative motion:

```json
{
  "version": 1,
  "sequence": 2,
  "type": "input.pointer.motion.relative",
  "dx": 4.5,
  "dy": -2
}
```

Pointer button:

```json
{
  "version": 1,
  "sequence": 3,
  "type": "input.pointer.button",
  "button": "primary",
  "pressed": true
}
```

`button` is `primary`, `middle`, `secondary`, `back`, or `forward`.

Continuous scroll and explicit libei axis stops:

```json
{
  "version": 1,
  "sequence": 4,
  "type": "input.pointer.scroll",
  "horizontal": 0,
  "vertical": 12.5,
  "stop_horizontal": false,
  "stop_vertical": false
}
```

Send a later message with a zero delta and the corresponding stop field set to
`true` when the browser gesture ends. Positive horizontal values move right;
positive vertical values move down.

Keyboard transition:

```json
{
  "version": 1,
  "sequence": 5,
  "type": "input.keyboard.key",
  "keycode": 30,
  "pressed": true
}
```

`keycode` is a Linux evdev keycode from 1 through 767. A custom browser
frontend must map `KeyboardEvent.code` to Linux evdev codes. Browser `keyCode`
and locale-dependent text are not accepted.

Successful input events have no response. Errors use the input channel and
include the sequence when it was decoded:

```json
{
  "version": 1,
  "sequence": 5,
  "type": "error",
  "ok": false,
  "error": {
    "code": "input_not_owned",
    "message": "peer does not own input"
  }
}
```

The worker always coalesces adjacent absolute motion, relative motion, and
continuous scroll where ordering remains unchanged. This prevents pointer
motion from building a stale queue. It never drops key, button, or scroll-stop
transitions. Overload that would lose transition ordering returns
`input_overloaded`, releases held state, revokes the lease, and closes the
input channel.

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

WebRTC settings include:

- `webrtc.signaling_path`
- `webrtc.max_peers`
- `webrtc.ice_servers`
- `webrtc.ice_username` and `webrtc.ice_credential`
- `webrtc.udp_port_min` and `webrtc.udp_port_max`
- `webrtc.allowed_origins`

Input settings include only implemented behavior:

- `input.enabled`
- `input.pointer`
- `input.keyboard`
- `input.queue_size`

Audio settings are static:

- `audio.enabled`
- `audio.device`
- `audio.bitrate_kbps`

Audio has no runtime quality command. The codec, 48 kHz sample rate, stereo
channel count, and 20 ms frame duration are fixed.

## Tracing

Tracing is off by default. Enable it with configuration, the
`WEBDESKTOP_TRACING_ENABLED` environment variable, or
`--tracing-enabled=true`:

```yaml
logging:
  level: debug
  format: json

tracing:
  enabled: true
```

The server then logs WebRTC, ICE, signaling, data-channel, queue, drop, write,
input, keyframe, NACK, requested encoder bitrate, effective H.264 bitrate and
CPB, and RTCP receiver-report state. Each active peer writes one snapshot every
five seconds. The browser sends connection, media, input lease, quality,
cleanup, error, and performance events through its signaling socket. Browser
trace output also appears in the ephemeral browser console.

Follow the packaged service logs on Polygon:

```bash
ssh polygon '
  export XDG_RUNTIME_DIR=/run/user/$(id -u)
  export DBUS_SESSION_BUS_ADDRESS=unix:path=$XDG_RUNTIME_DIR/bus
  journalctl --user -u webdesktop.service -f -o cat
'
```

Client entries use logger name `webrtc.client`, include the server `peer_id`,
and store browser fields under `client_details`. Disable tracing after the
problem is captured if the extra five-second snapshots are no longer useful.

The remote-desktop binary defaults to pointer and keyboard control. Embedding
deployments can set `input.enabled: false` to keep view-only ScreenCast
behavior.

Matching environment variables use names such as
`WEBDESKTOP_WEBRTC_MAX_PEERS` and `WEBDESKTOP_WEBRTC_UDP_PORT_MIN`. Run
`webdesktop serve --help` for matching flags. Repeated ICE server and allowed
origin values use the repeatable `--webrtc-ice-server` and
`--webrtc-allowed-origin` flags.

The implemented source is `monitor`. Cursor mode can be `embedded` or `hidden`.
VP8 is the default codec. H.264 uses `vah264enc` with constrained-baseline
Level 4.2 byte-stream output and a `42c02a` SPS. WebRTC negotiates the
compatible libwebrtc SDP form `42e02a`. Startup rejects H.264 quality outside
the limits listed above. Width and height are applied after color conversion,
frame-rate normalization, and scaling.

## Network and security

`webdesktop` has no authentication and no TLS. The loopback listen address is
the safe default. An embedding application or reverse proxy must authenticate
and protect signaling and both data channels before exposing them beyond the
local machine. A peer that acquires input can control the active unlocked
desktop with the portal-authorized pointer and keyboard classes.

There is no clipboard, file transfer, gamepad, touch, or remote unlock. The
portal and Plasma lock screen remain the authority. Input cannot unlock a
locked session.

If the UDP port minimum and maximum are both zero, Pion uses the system
ephemeral range. Otherwise both values are required. No public STUN server is
configured by default. ICE server URLs may use `stun`, `stuns`, `turn`, or
`turns`. The service parses the complete URI, including port and
`transport=udp` or `transport=tcp`, during startup with Pion's STUN/TURN URI
parser. Invalid schemes, hosts, ports, queries, and transports fail startup.
TURN URLs require the configured username and credential. The project does not
deploy or manage a TURN server.

Host candidates usually work on the same machine or reachable LAN. NAT,
firewalls, container networking, and reverse proxies can prevent direct ICE
connectivity. Configure reachable STUN or TURN infrastructure and allow the
selected UDP range when access crosses those boundaries.

With an empty allowed-origin list, browser WebSockets must use the same host as
the HTTP request. Clients without an `Origin` header are allowed. Configured
origins are exact `http://host[:port]` or `https://host[:port]` values. `*`
allows every origin and should be used only behind another trusted control.
Origin checks do not authenticate a user.

The Polygon production service currently binds every LAN interface on TCP 8080
and pins WebRTC ICE to UDP 60000 through 61000. The service config and NixOS
firewall must use the same UDP range:

```nix
networking.firewall.allowedTCPPorts = [ 8080 ];
networking.firewall.allowedUDPPortRanges = [
  {
    from = 60000;
    to = 61000;
  }
];
```

```yaml
webrtc:
  udp_port_min: 60000
  udp_port_max: 61000
```

Then open `http://polygon.lan:8080/`. Webdesktop has no authentication or TLS,
so do not forward this port from the router or expose it to an untrusted
network.

An SSH tunnel remains the safer option when direct LAN access is not wanted:

```bash
ssh -N -L 8080:127.0.0.1:8080 polygon
```

Then open `http://127.0.0.1:8080/`.

To bind every Polygon interface intentionally, set
`server.listen_address: 0.0.0.0:8080` in the production config and restart the
user service. Webdesktop has no authentication or TLS, so every host that can
reach that port can view and control the desktop.

The `webrtc` Go package exposes a media interface, `Service.Run`,
`Service.Close`, and a Gin `Handler`. An embedding application can mount that
handler on its own router after its authentication and authorization
middleware. The standalone binary mounts it at the configured signaling path
and keeps `GET /healthz`.

## Build and run

Build the package:

```bash
nix build path:.#
```

The package build fetches dependencies from `web/pnpm-lock.yaml` with a fixed
Nix hash, runs frontend format, lint, typecheck, and production build checks,
then copies `web/dist` into the Go source used by `go:embed`.

Run the binary without installing it:

```bash
nix run path:.# -- version
nix run path:.# -- serve --config ./webdesktop.example.yaml
```

The second command opens the normal Plasma portal consent dialog.

## systemd user service

Install the package into the user profile, copy the packaged example, and
enable the packaged unit:

```bash
nix profile install path:.#
package=$(nix path-info path:.#)
install -Dm600 "$package/share/webdesktop/config.example.yaml" \
  "$HOME/.config/webdesktop/config.yaml"
systemctl --user enable --now \
  "$package/lib/systemd/user/webdesktop.service"
```

Edit `~/.config/webdesktop/config.yaml` before starting the service if the
defaults are not suitable. The unit passes that exact path to the binary.

The unit uses `WantedBy=`, `After=`, `Requisite=`, and `PartOf=` for
`graphical-session.target`. It stops with the graphical session and cannot
start successfully when that target is inactive. D-Bus, PipeWire, and
pipewire-pulse are reached through the active user session and their normal
socket or D-Bus activation.

The unit orders itself after xdg-desktop-portal and KDE's portal backend. It
does not restart the shared portal, so starting webdesktop cannot invalidate
capture sessions owned by Sunshine, KRDP, or other desktop applications.

The unit has no restart policy. A denied portal request or invalid
configuration stays failed instead of repeatedly reopening the consent dialog.
`TimeoutStopSec=15s` leaves time for peer, HTTP, GStreamer, libei, and portal
cleanup after SIGTERM.

The package also installs:

- `$package/share/applications/io.github.tarik02.webdesktop.desktop`
- `$package/share/webdesktop/config.example.yaml`

Webdesktop creates its private user state directory when the portal returns the
first restore token.

Disable it with:

```bash
systemctl --user disable --now webdesktop.service
```

## Development

Enter the development environment:

```bash
nix develop
```

Build and vet the Go packages:

```bash
go build ./...
go vet ./...
```

Build the frontend before running Go commands directly because the embedded
package expects `web/dist`:

```bash
cd web
pnpm format:check
pnpm lint
pnpm typecheck
pnpm build
```

`nix flake check path:.` builds the frontend and package, runs `go vet ./...`,
checks the embedded assets, and validates the packaged systemd unit and desktop
entry.

The primary test harness is the focused Polygon browser E2E:

```bash
e2e/polygon.sh
```

It requires the packaged service and an existing portal restore token. It uses
an ephemeral agent-browser session through a temporary SSH tunnel and a
fullscreen GTK4 target forced onto Wayland. The target drives compositor-paced
full-surface motion, logs real pointer and keyboard input, and acknowledges a
deterministic color change only after KWin reports presentation. The harness
checks advancing WebRTC video, optional audio, H.264 1080p60 at 8,000 and
10,000 Kbit/s, same-peer bitrate cutovers, idle/change latency, quality and
codec changes, disconnect cleanup, and unattended restoration after a service
restart. It deletes its browser, tunnel, target, and monitoring process on
exit.

The longer codec gate requires Firefox in the Polygon user profile:

```bash
nix profile add --profile ~/.nix-profile nixpkgs#firefox
e2e/polygon-youtube.sh
```

It resolves an anonymous progressive Big Buck Bunny media URL from YouTube
with `yt-dlp`, verifies the source is hosted by `googlevideo.com`, and plays it
in Firefox with a temporary Selenium profile in the real Plasma session. This
avoids YouTube's page-level automation cutoff while still exercising a real
YouTube stream. The gate checks Firefox's media time and the remote browser's
decoded-frame count. VP8 and H.264 must each play for at least one minute
without a ten-second freeze, and each connection must render its first frame
within five seconds. The script removes the temporary Firefox profile,
Aperture browser, and test files, then restarts the production service from
its configured VP8 settings.
