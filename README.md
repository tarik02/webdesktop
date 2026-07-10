# webdesktop

`webdesktop` streams an existing KDE Plasma Wayland desktop over WebRTC. It
uses xdg-desktop-portal ScreenCast for monitor capture, owns the portal
PipeWire remote for the lifetime of one shared GStreamer pipeline, and encodes
video in software as VP8 or H.264. Remote pointer and keyboard events use the
xdg-desktop-portal RemoteDesktop interface, `ConnectToEIS`, and the system
libei library.

The service includes WebSocket signaling and a versioned data-channel protocol
for video quality and exclusive input control. It has no frontend, audio,
remote unlock, built-in authentication, TLS termination, or systemd unit.

## Commands

```text
webdesktop serve [flags]
webdesktop version
```

The default HTTP address is `127.0.0.1:8080`. The running service exposes:

- `GET /healthz`
- `GET /webrtc`, upgraded to the signaling WebSocket by default

The health endpoint starts while desktop capture authorization is pending.

## Plasma portal authorization

Input is enabled by default. `webdesktop serve` creates a RemoteDesktop portal
session, calls ScreenCast `SelectSources` for one monitor, calls RemoteDesktop
`SelectDevices` for the configured pointer and keyboard classes, and calls
RemoteDesktop `Start` once. The Start response supplies both the selected
devices and ScreenCast stream. The service then opens the PipeWire remote and
calls `ConnectToEIS`. Media and input use that one authorized portal session.
Closing it stops both.

KDE Plasma shows its normal remote-desktop consent dialog in the active
graphical session. A user must be present in the active, unlocked session to
select the monitor and authorize input. Set `input.enabled: false` for the
ScreenCast-only CreateSession, SelectSources, Start, and OpenPipeWireRemote
flow.

`webdesktop` does not request persistent portal permission and cannot authorize
capture at the login or lock screen. It does not provide unattended remote
unlock. Input works only while the authorized Plasma session is active and
unlocked. If the user denies or cancels the portal dialog, the service exits
and closes signaling and all peers.

## WebRTC behavior

The configured video codec is the only codec registered with Pion. VP8 frames
use the VP8 RTP payloader. H.264 uses constrained-baseline Annex-B access units
from `x264enc` and packetization mode 1. WebRTC signaling uses the
libwebrtc-compatible constrained-baseline identifier
`profile-level-id=42e028`, Level 4.0, for the lifetime of every peer
connection. Browser offers such as `42e01f` are accepted when they include
`level-asymmetry-allowed=1`; the answer advertises `42e028`. Offers at Level
4.0 or higher do not need level asymmetry.

The GStreamer caps force constrained-baseline Level 4.0. x264 writes
`42c028` in the SPS. `42c028` and the negotiated `42e028` both identify
constrained-baseline Level 4.0 bitstreams; the extra `42e0` constraint flag is
the canonical form used by libwebrtc SDP and lets Pion match browser offers.

All peers share one encoded GStreamer stream. Each peer has its own RTP
packetizer, bounded eight-sample queue, and writer. A slow TURN/TCP or TURN/TLS
peer can drop only its own queued samples. It cannot block capture, another
peer, or service shutdown. Connecting a viewer does not create another
GStreamer pipeline. Each WebSocket owns one peer connection, and the default
maximum is two peers.

Per-peer RTP timing follows encoded sample PTS gaps, so queue drops preserve
elapsed media time. A PTS regression or a jump over 10 seconds is treated as a
pipeline discontinuity and advances by the encoded sample duration instead.

The service reads RTCP from every video sender. PLI, FIR, and a newly connected
peer request a keyframe from the active GStreamer encoder. The encoder receives
an upstream force-key-unit event with headers requested, which works for both
VP8 and H.264.

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

The client must create a recv-only video transceiver and a reliable ordered data
channel named `control` before creating its offer. A client that wants input
must also create one reliable ordered data channel named `input`. Creating a
data channel ensures that the offer contains the SCTP media section. The server
rejects other labels, duplicate channels, and either channel when configured
for unordered or partial-reliable delivery.

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

`codec` is accepted only so the service can return the specific
`codec_static` error. Changing codec requires SDP renegotiation and is not
implemented. Unknown message types and fields are rejected.

H.264 quality must remain inside Level 4.0 for the full peer lifetime:

- no more than 256 rounded macroblocks in either frame dimension
- no more than 8192 macroblocks per frame
- no more than 245760 macroblocks per second
- no more than 20000 Kbit/s

Macroblock dimensions round width and height up to multiples of 16. For
example, 1920x1080 at 30 fps uses 244800 macroblocks per second and fits, while
1920x1080 at 60 fps does not. A 7680x240 frame exceeds the 256-macroblock
width limit even though its total macroblock count fits. Startup and control
updates reject such dimensions before changing the active pipeline.
An incompatible control update returns `h264_level_incompatible` without
changing the stream.

Quality is global because every viewer receives the same encoded stream. A
quality update from one peer changes the stream for every peer. Bitrate-only
updates modify the live encoder. Resolution or frame-rate changes rebuild the
single pipeline against the same portal session.

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

When its bounded queue is full, the worker coalesces adjacent absolute motion,
relative motion, and continuous scroll where ordering remains unchanged. It
never drops key, button, or scroll-stop transitions. Overload that would lose
transition ordering returns `input_overloaded`, releases held state, revokes
the lease, and closes the input channel.

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

The remote-desktop binary defaults to pointer and keyboard control. Embedding
deployments can set `input.enabled: false` to keep view-only ScreenCast
behavior.

Matching environment variables use names such as
`WEBDESKTOP_WEBRTC_MAX_PEERS` and `WEBDESKTOP_WEBRTC_UDP_PORT_MIN`. Run
`webdesktop serve --help` for matching flags. Repeated ICE server and allowed
origin values use the repeatable `--webrtc-ice-server` and
`--webrtc-allowed-origin` flags.

The implemented source is `monitor`. Cursor mode can be `embedded` or `hidden`.
VP8 is the default codec. H.264 uses software `x264enc` with
constrained-baseline Level 4.0 byte-stream output and a `42c028` SPS. WebRTC
negotiates the compatible libwebrtc SDP form `42e028`. Startup rejects H.264
quality outside the limits listed above. Width and height are applied after
software color conversion, frame-rate normalization, and scaling.

## Network and security

`webdesktop` has no authentication and no TLS. The loopback listen address is
the safe default. An embedding application or reverse proxy must authenticate
and protect signaling and both data channels before exposing them beyond the
local machine. A peer that acquires input can control the active unlocked
desktop with the portal-authorized pointer and keyboard classes.

There is no clipboard, file transfer, gamepad, touch, audio, or remote unlock.
The portal and Plasma lock screen remain the authority. Input cannot unlock a
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

The `webrtc` Go package exposes a media interface, `Service.Run`,
`Service.Close`, and a Gin `Handler`. An embedding application can mount that
handler on its own router after its authentication and authorization
middleware. The standalone binary mounts it at the configured signaling path
and keeps `GET /healthz`.

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
