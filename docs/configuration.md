# Configuration

Webdesktop accepts YAML, TOML, or JSON:

```bash
webdesktop serve --config ./webdesktop.yaml
```

Resolution order, highest priority first:

1. command flags
2. `WEBDESKTOP_*` environment variables
3. the explicit configuration file
4. built-in defaults

Environment names follow the configuration path. For example,
`webrtc.max_peers` becomes `WEBDESKTOP_WEBRTC_MAX_PEERS`. Run
`webdesktop serve --help` for the matching flags.

The complete default file is [webdesktop.example.yaml](../webdesktop.example.yaml).

## Server and logging

| Setting | Default | Notes |
| --- | --- | --- |
| `server.listen_address` | `127.0.0.1:8080` | HTTP, SPA, and signaling listener |
| `server.shutdown_timeout` | `10s` | Positive Go duration |
| `logging.level` | `info` | Zap log level |
| `logging.format` | `json` | `json` or `console` |
| `tracing.enabled` | `false` | Bounded server and browser diagnostics |

## Authentication

| Setting | Default | Notes |
| --- | --- | --- |
| `auth.trusted_proxy_cidrs` | `[]` | Reverse proxies trusted to append client addresses for login rate limiting |
| `auth.login.enabled` | `false` | Enable the embedded password login |
| `auth.login.password_file` | empty | Owner-only file containing the password |
| `auth.bearer.enabled` | `false` | Accept `Authorization: Bearer` and token login |
| `auth.bearer.token_file` | empty | Owner-only file containing one bearer token |
| `auth.session.ttl` | `24h` | Lifetime of in-memory browser sessions |
| `auth.session.secure_cookie` | `false` | Send browser sessions only over HTTPS |

Each mechanism is independent. When both are enabled, either the password or
bearer token can create a browser session, and a valid bearer header can access
protected HTTP and WebSocket routes directly. When both are disabled,
webdesktop retains its unauthenticated behavior.

Credential files must be regular owner-only files, contain one UTF-8 line, and
have no group or other permission bits. Symlinks are rejected. Passwords must
be at least 8 bytes; bearer tokens must be at least 32 bytes. One trailing
newline is ignored. Webdesktop reads the files at startup and stores only
SHA-256 digests in memory.

Browser sessions are random, `HttpOnly`, `SameSite=Strict` cookies. They are
revocable through the UI but are not persisted, so restarting the service logs
browsers out. Logout and session expiry close WebSocket peers authorized by
that session. Set `auth.session.secure_cookie` when the public URL uses HTTPS.
Authentication over plain HTTP does not protect credentials from network
observers.

Login rate limiting uses the TCP peer address by default. If a reverse proxy
must preserve separate client buckets, list only that proxy's CIDRs in
`auth.trusted_proxy_cidrs`. Webdesktop then reads `X-Forwarded-For` from
right to left and ignores values beyond the first untrusted hop. Never list a
client network or an all-address CIDR.

## Video

| Setting | Default | Notes |
| --- | --- | --- |
| `video.source` | `monitor` | Only monitor capture is implemented |
| `video.cursor_mode` | `embedded` | `embedded` or `hidden` |
| `video.profile` | `vp8` | Selected key from `video.profiles` |
| `video.option` | `balanced` | Selected named option from the active profile |
| `video.tuning.threads` | `8` | Encoder threads, 1 through 64 |
| `video.tuning.keyframe_interval` | `60` | Frames, 1 through 600 |
| `video.tuning.vp8_cpu_used` | `16` | VP8 speed setting, 0 through 16 |
| `video.profiles` | three built-ins | Encoder pipeline, bitrate updates, codec metadata, and limits |

The built-in profiles are:

- `vp8`, software VP8 with `vp8enc`
- `h264-software`, software H.264 with `x264enc`
- `h264-vaapi`, VA-API H.264 with `vah264enc`

Defining `video.profiles` replaces the built-in profile map.

VP8 remains the default. Both H.264 profiles produce constrained-baseline Level
4.2 byte streams and use the same WebRTC codec metadata. Their quality must stay
within all of these limits:

- 263 rounded macroblocks in either dimension
- 8704 macroblocks per frame
- 522240 macroblocks per second
- 50000 Kbit/s

Macroblock dimensions round width and height up to multiples of 16.
1920x1080 at 60 fps fits Level 4.2.

Clients select a profile and named option as a base, then may override its
resolution, frame rate, and bitrate. The server checks the effective values
against generic bounds and the profile's limits. A bitrate-only change updates
the active encoder through the profile's configured properties. Other changes
start a replacement encoder and switch after its first keyframe. Switching
between the two H.264 profiles keeps the existing peer connection because their
codec metadata is identical. Switching between H.264 and VP8 requires a new SDP
exchange, so the embedded client reconnects.

### Encoder profile definitions

Each `video.profiles` entry contains:

| Field | Purpose |
| --- | --- |
| `label` | Browser UI label |
| `default_option` | Option used when a client changes profile without naming an option |
| `frontend_transform` | Browser transform: `none`, `flip-horizontal`, `flip-vertical`, or `rotate-180` |
| `options` | Named complete quality tuples with `label`, `width`, `height`, `framerate`, and `bitrate_kbps` |
| `pipeline` | GStreamer pipeline fragment between `videorate` and `appsink` |
| `encoder_element` | Named element used for encoder tracing |
| `bitrate` | Ordered live property updates with `element`, `property`, `type`, and templated `value` |
| `codec` | Codec ID, MIME type, RTP clock and payload, payloader, RTCP feedback, and SDP metadata |
| `limits` | Optional bitrate and macroblock limits; zero disables a limit |

Pipeline and bitrate values use Go template syntax. The available values are
`.Width`, `.Height`, `.Framerate`, `.BitrateKbps`, `.Threads`,
`.KeyframeInterval`, and `.VP8CPUUsed`. `mul` multiplies two integers,
`ceilDiv` divides and rounds up, and `element` prefixes an element name for the
current encoder branch. For example:

```yaml
pipeline: >-
  x264enc name={{ element "encoder" }}
  bitrate={{ .BitrateKbps }} !
  video/x-h264,stream-format=byte-stream,alignment=au
bitrate:
  - element: encoder
    property: bitrate
    type: uint
    value: "{{ .BitrateKbps }}"
```

`codec.payloader` supports `vp8` and `h264`. `codec.sdp.offer_fmtp` maps FMTP
parameter names to regular expressions required in a browser offer.
`codec.sdp.answer_fmtp` replaces matching answer parameters. Profiles that use
the same codec ID must have identical RTP and SDP metadata so their encoded
streams can share active peer connections.

## Audio

| Setting | Default | Notes |
| --- | --- | --- |
| `audio.enabled` | `false` | Enables desktop audio |
| `audio.device` | `@DEFAULT_MONITOR@` | Must be the default or end in `.monitor` |
| `audio.bitrate_kbps` | `128` | 6 through 510 |

Audio uses stereo Opus at 48 kHz with 20 ms frames. It has no runtime quality
command.

## Input

| Setting | Default | Notes |
| --- | --- | --- |
| `input.enabled` | `true` | Requests portal RemoteDesktop access |
| `input.locking` | `false` | Restricts input and clipboard control to one peer |
| `input.pointer` | `true` | Enables pointer events |
| `input.keyboard` | `true` | Enables keyboard events |
| `input.queue_size` | `256` | Events per peer, 16 through 4096 |

At least one input class must be enabled when input is active. Peers control
input independently by default. Set `input.locking: true` or pass
`--input-locking` to allow only one peer at a time.

## Clipboard

| Setting | Default | Notes |
| --- | --- | --- |
| `clipboard.enabled` | `true` | Synchronizes text, HTML, and supported image formats |

Clipboard access uses the Wayland Clipboard portal and requires `input.enabled` and
`input.keyboard`.
Each peer with an active input session receives desktop clipboard content and
may replace it. With input locking enabled, this is limited to the peer holding
the input lock. Transfers are limited to 32 MiB.

## WebRTC

| Setting | Default | Notes |
| --- | --- | --- |
| `webrtc.signaling_path` | `/webrtc` | Clean absolute path below `/` |
| `webrtc.max_peers` | `2` | 1 through 64 |
| `webrtc.replace_existing_peer` | `false` | Requires `max_peers: 1`; a new signaling connection closes and replaces the current peer |
| `webrtc.ice_servers` | `[]` | STUN or TURN URLs |
| `webrtc.ice_username` | empty | Required with TURN |
| `webrtc.ice_credential` | empty | Required with TURN |
| `webrtc.udp_port_min` | `0` | Zero uses the system range |
| `webrtc.udp_port_max` | `0` | Must be set with the minimum |
| `webrtc.allowed_origins` | `[]` | Empty means same-host browser requests |

ICE URLs may use `stun`, `stuns`, `turn`, or `turns`. TURN transports may be
selected with `transport=udp` or `transport=tcp`.

## Tracing

Enable bounded transport diagnostics:

```yaml
logging:
  level: debug
  format: json

tracing:
  enabled: true
```

The server logs signaling, ICE, RTCP, queue state, drops, encoder settings,
write durations, keyframes, input state, and one peer snapshot every five
seconds. The embedded browser sends matching connection and performance
events through the signaling socket.

Tracing never includes SDP, full ICE candidates, key values, pointer
coordinates, or per-frame input events.
