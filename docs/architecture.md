# Architecture

Webdesktop is Wayland-only. KDE's portal authorizes the session, KWin exposes a
PipeWire stream, GStreamer captures and encodes it, and Pion sends the encoded
samples to browsers.

```text
xdg-desktop-portal
  -> PipeWire remote
  -> pipewiresrc
  -> newest raw frame
  -> configured encoder profile
  -> Pion RTP
  -> WebRTC peers
```

Pointer motion travels through an unordered zero-retransmit WebRTC data channel.
Buttons, scrolling, and keyboard events use a separate reliable ordered channel
so lost motion cannot delay current input. The bundled desktop service connects
that controller to the portal's `ConnectToEIS` file descriptor through libei;
embedded applications can provide another input sender.

The input protocol carries both physical keyboard transitions and committed
UTF-8 text. Physical transitions support shortcuts and navigation. Text events
preserve the client's active input method. Backends opt into text delivery by
implementing `input.KeyboardTextSender`. The bundled EIS sender uses libei's
text capability when the compositor offers it. On Plasma versions without that
capability, it sends Unicode keysyms through KWin's authorized fake-input
protocol.

Clipboard content uses a separate reliable data channel and the Clipboard portal
attached to the same RemoteDesktop session. An active input session gates
clipboard access. Optional input locking limits control and clipboard access to
one peer.

## Video capture

The portal PipeWire connection stays open for the service lifetime. Capture
and encoding use separate GStreamer pipelines so an encoder change does not
rebuild or pause the portal stream.

`pipewiresrc` negotiates DMA-BUF before system memory and retains four to eight
capture buffers so the compositor can keep exporting frames without forced
copies. DMA-BUF-backed samples keep their memory through the latest-frame slot
and encoder appsrc. The capture appsink keeps one sample. An application-owned
single-frame slot feeds a one-buffer leaky encoder appsrc. If encoding slows down, each layer
drops obsolete raw frames instead of blocking PipeWire or accumulating stale
video. The portal capture session stays open while the service is idle, but raw
frames only enter the encoder while at least one WebRTC peer is registered.

`pipewiresrc` uses a frame keepalive based on the configured frame rate. This
resends the latest buffer when the compositor provides damage-driven updates,
so an idle desktop does not stop the RTP timeline. Each encoder branch
timestamps its input from its own monotonic pipeline clock, so a PipeWire
timestamp regression cannot pause `videorate`. `videorate` caps each encoder
branch without manufacturing queued duplicate frames.

Encoded samples use blocking handoff. Encoded reference chains are not dropped
between the encoder and peer writers. Pion's GCC interceptor uses transport-wide
congestion feedback, while RTP is emitted without its shared FIFO pacer so Opus
cannot wait behind a large video access unit. The shared encoder uses the lowest
active peer estimate, capped by the bitrate selected by the user. Encoder updates
use a five-percent or 250 Kbit/s hysteresis to avoid forcing VA-API rate-control
reconfiguration for every small GCC estimate change.

## Encoder profiles and quality changes

Encoder profiles come from runtime configuration. Each profile contains named
quality options, its GStreamer pipeline template, live bitrate properties,
quality limits, RTP codec capability, packetizer selection, RTCP feedback, and
SDP constraints. Each option is a full resolution, frame rate, and bitrate
tuple. Adding an encoder for an existing supported RTP payloader does not
require a codec branch in the service.

The built-in VP8 profile uses `vp8enc` with a short rate-control buffer, bounded
intra frames, and a realtime CPU setting. The software H.264 profile uses
`x264enc` with the ultrafast zero-latency preset. The VA-API H.264 profile uses
`vah264enc`. Its constrained-baseline and High variants use one slice,
macroblock rate control, and a medium quality/speed target. The High variant is
offered separately so clients without an advertised H.264 High capability do
not select it.

All peers share one encoded stream. Clients select an advertised profile and
option as a base, then may override resolution, frame rate, and bitrate within
that profile's limits. A bitrate-only change updates the active encoder using
the profile's configured properties. The VA-API profile updates its CPB request
and target bitrate together.

Resolution and frame-rate changes create a candidate encoder branch against
the current latest-frame slot. The service switches only after the candidate
produces an IDR, then retires the old branch. A failed candidate leaves the
active stream unchanged.

A profile change with different codec metadata needs a new SDP offer and answer.
The embedded client reconnects after the new encoder becomes active. Profiles
with identical codec metadata switch without reconnecting.

Profiles configure any display orientation correction through
`frontend_transform`. The encoder keeps the captured orientation unchanged;
the browser transforms the video and remaps absolute pointer coordinates.

## RTP timing and recovery

Video RTP timing follows the monotonic production gap between encoded access
units. GStreamer PTS remains available for diagnostics but does not control the
RTP clock across encoder replacements.

The service reads RTCP from every sender. Transport-cc reports drive pacing and
encoder bitrate. PLI, FIR, and a newly connected peer request a keyframe from
the active encoder. Pion keeps recent RTP packets for
NACK retransmission and emits sender reports that map RTP clocks to NTP time.

A new peer ignores inter-frames until it receives a decodable keyframe.

## Audio

Optional audio uses `pulsesrc` against a PipeWire PulseAudio monitor, converts
to stereo S16LE at 48 kHz, and encodes 20 ms Opus frames. Audio and video share
the same WebRTC media stream ID. Their independent capture pipelines are not
calibrated for sample-accurate lip sync.

The embedded client requests a 10 ms browser jitter-buffer target for both
receivers. Applying the same target to audio and video keeps lip sync from
forcing the video receiver back to Chromium's larger default queue.

## Embedding

The `webrtc` Go package exposes a standard `net/http` signaling handler and
uses interfaces for media, audio, input, and clipboard integration. Importing
it does not require the portal, GStreamer, or libei packages. Applications can
mount it behind their own authentication middleware and use a custom frontend.

See [Embedding the WebRTC transport](embedding.md) for the contracts and
lifecycle options.

## Implementation references

The pipeline design was informed by:

- [Sunshine's PipeWire capture path](https://github.com/LizardByte/Sunshine/blob/c78b9827867b5aff80e7319d222b81e1d2cfd122/src/platform/linux/pipewire.cpp),
  especially newest-buffer handling.
- [Neko's GStreamer capture pipelines](https://github.com/m1k1o/neko/blob/d74052bb844c43a0cc3c2386d083f7505dc483a2/server/internal/config/capture_pipeline.go)
  and [direct encoded-sample handoff](https://github.com/m1k1o/neko/blob/d74052bb844c43a0cc3c2386d083f7505dc483a2/server/internal/webrtc/track.go).
- [Selkies' GStreamer WebRTC implementation](https://github.com/selkies-project/selkies/blob/7a80d7eea94f7ff5e754407a18364f4008d8b0fd/src/selkies_gstreamer/gstwebrtc_app.py),
  especially H.264 settings and live bitrate changes.
- [KDE KRDP/KPipeWire's VideoStream](https://github.com/KDE/krdp/blob/7396f77f44e3e4515a1d6182ef4ad4f267f8e986/src/VideoStream.cpp)
  for native Wayland PipeWire capture.

Webdesktop implements these ideas in Go and does not vendor source from those
projects. Each reference remains under its upstream license.
