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

## Video

| Setting | Default | Notes |
| --- | --- | --- |
| `video.source` | `monitor` | Only monitor capture is implemented |
| `video.cursor_mode` | `embedded` | `embedded` or `hidden` |
| `video.codec` | `vp8` | `vp8` or `h264` |
| `video.width` | `1920` | Even value from 320 through 7680 |
| `video.height` | `1080` | Even value from 240 through 4320 |
| `video.framerate` | `30` | 1 through 120 |
| `video.bitrate_kbps` | `4000` | At least 100 |
| `video.tuning.threads` | `8` | VP8 threads, 1 through 64 |
| `video.tuning.keyframe_interval` | `60` | Frames, 1 through 600 |
| `video.tuning.vp8_cpu_used` | `16` | VP8 speed setting, 0 through 16 |

H.264 uses constrained-baseline Level 4.2. Its quality must stay within all of
these limits:

- 263 rounded macroblocks in either dimension
- 8704 macroblocks per frame
- 522240 macroblocks per second
- 50000 Kbit/s

Macroblock dimensions round width and height up to multiples of 16.
1920x1080 at 60 fps fits Level 4.2.

Bitrate changes update the active encoder. Resolution and frame-rate changes
start a replacement encoder and switch after its first keyframe. Codec changes
require a new SDP exchange, so the embedded client reconnects.

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
| `input.pointer` | `true` | Enables pointer events |
| `input.keyboard` | `true` | Enables keyboard events |
| `input.queue_size` | `256` | 16 through 4096 |

At least one input class must be enabled when input is active. Only one peer
can hold the input lease.

## WebRTC

| Setting | Default | Notes |
| --- | --- | --- |
| `webrtc.signaling_path` | `/webrtc` | Clean absolute path below `/` |
| `webrtc.max_peers` | `2` | 1 through 64 |
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
