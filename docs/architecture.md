# Architecture

Webdesktop is Wayland-only. KDE's portal authorizes the session, KWin exposes a
PipeWire stream, GStreamer captures and encodes it, and Pion sends the encoded
samples to browsers.

```text
xdg-desktop-portal
  -> PipeWire remote
  -> pipewiresrc
  -> newest raw frame
  -> VP8 or VA H.264 encoder
  -> Pion RTP
  -> WebRTC peers
```

Pointer and keyboard events travel in the other direction through a WebRTC data
channel, the portal's `ConnectToEIS` file descriptor, and libei.

Clipboard content uses a separate reliable data channel and the Clipboard portal
attached to the same RemoteDesktop session. The input lease also gates clipboard
access, so only the active controller receives or replaces clipboard content.

## Video capture

The portal PipeWire connection stays open for the service lifetime. Capture
and encoding use separate GStreamer pipelines so an encoder change does not
rebuild or pause the portal stream.

The capture appsink keeps one sample. An application-owned single-frame slot
feeds a one-buffer leaky encoder appsrc. If encoding slows down, each layer
drops obsolete raw frames instead of blocking PipeWire or accumulating stale
video. The portal capture session stays open while the service is idle, but raw
frames only enter the encoder while at least one WebRTC peer is registered.

`pipewiresrc` uses a frame keepalive based on the configured frame rate. This
resends the latest buffer when the compositor provides damage-driven updates,
so an idle desktop does not stop the RTP timeline. `videorate` caps each
encoder branch without manufacturing queued duplicate frames.

Encoded samples use blocking handoff. Encoded reference chains are not dropped
between the encoder and peer writers. There is no per-peer video queue or
packet pacer.

## Encoders and quality changes

VP8 uses `vp8enc` with a short rate-control buffer, bounded intra frames, and a
realtime CPU setting.

H.264 uses `vah264enc` in constrained-baseline Level 4.2 byte-stream mode. It
uses VA CBR control, no B-frames, one reference frame, four slices, disabled
CABAC, and disabled macroblock bitrate control. SDP advertises the
libwebrtc-compatible `42e02a` profile-level identifier.

All peers share one encoded stream. A bitrate-only change updates the active
encoder while it is playing. H.264 updates its CPB request and target bitrate
together.

Resolution and frame-rate changes create a candidate encoder branch against
the current latest-frame slot. The service switches only after the candidate
produces an IDR, then retires the old branch. A failed candidate leaves the
active stream unchanged.

A codec change needs a new SDP offer and answer. The embedded client reconnects
after the new encoder becomes active.

## RTP timing and recovery

Video RTP timing follows the monotonic production gap between encoded access
units. GStreamer PTS remains available for diagnostics but does not control the
RTP clock across encoder replacements.

The service reads RTCP from every sender. PLI, FIR, and a newly connected peer
request a keyframe from the active encoder. Pion keeps recent RTP packets for
NACK retransmission and emits sender reports that map RTP clocks to NTP time.

A new peer ignores inter-frames until it receives a decodable keyframe.

## Audio

Optional audio uses `pulsesrc` against a PipeWire PulseAudio monitor, converts
to stereo S16LE at 48 kHz, and encodes 20 ms Opus frames. Audio and video share
the same WebRTC media stream ID. Their independent capture pipelines are not
calibrated for sample-accurate lip sync.

## Embedding

The `webrtc` Go package exposes its media interface, `Service.Run`,
`Service.Close`, and a Gin `Handler`. Another application can mount the handler
behind its own authentication and authorization middleware.

## Implementation references

The pipeline design was informed by:

- [Sunshine's PipeWire capture path](https://github.com/LizardByte/Sunshine/blob/c78b9827867b5aff80e7319d222b81e1d2cfd122/src/platform/linux/pipewire.cpp),
  especially newest-buffer handling.
- [Neko's GStreamer capture pipelines](https://github.com/m1k1o/neko/blob/d74052bb844c43a0cc3c2386d083f7505dc483a2/server/internal/config/capture_pipeline.go)
  and [direct encoded-sample handoff](https://github.com/m1k1o/neko/blob/d74052bb844c43a0cc3c2386d083f7505dc483a2/server/internal/webrtc/track.go).
- [Selkies' GStreamer WebRTC implementation](https://github.com/selkies-project/selkies/blob/7a80d7eea94f7ff5e754407a18364f4008d8b0fd/src/selkies_gstreamer/gstwebrtc_app.py),
  especially VA H.264 settings and live bitrate changes.
- [KDE KRDP/KPipeWire's VideoStream](https://github.com/KDE/krdp/blob/7396f77f44e3e4515a1d6182ef4ad4f267f8e986/src/VideoStream.cpp)
  for native Wayland PipeWire capture.

Webdesktop implements these ideas in Go and does not vendor source from those
projects. Each reference remains under its upstream license.
