# Embedding the WebRTC transport

The `webrtc` package can be embedded without using webdesktop's portal capture,
GStreamer media service, libei input controller, clipboard controller, Gin
server, or bundled frontend.

Create a `webrtc.Service` with application-owned implementations of these
contracts:

- `MediaSource` supplies encoded VP8 or H.264 access units, quality metadata,
  live quality updates, and keyframe requests.
- `AudioSource` supplies encoded Opus frames when audio is enabled.
- `InputController` owns peer leases and receives validated input events.
- `ClipboardController` synchronizes clipboard content when enabled.
- `Observer` optionally receives peer open, connection-state, and close events.

The media source returns complete encoded access units rather than RTP packets.
The transport performs RTP packetization, timing, fan-out, RTCP handling, and
retransmission for every peer.

Applications that already expose a PipeWire node can reuse webdesktop's media
service without opening a portal session:

```go
source, err := media.NewPipeWireTargetSource("weston.pipewire")
if err != nil {
	return err
}
go mediaService.Run(ctx, source)
```

The target source uses the process PipeWire remote and sets the supplied value
as `pipewiresrc.target-object`. `media.PortalSource` keeps the existing portal
capture path available to the bundled application.

`InputController` and `ClipboardController` may be `nil`. The service then uses
disabled implementations and rejects the corresponding protocol requests.
`AudioSource` may be `nil` only when `Config.AudioEnabled` is false.

`Service.Handler` returns a standard `net/http.Handler`, so applications can
mount signaling behind their own authentication and routing middleware:

```go
service, err := webrtc.New(
	webrtc.Config{
		MaxPeers:            1,
		ReplaceExistingPeer: true,
	},
	mediaSource,
	nil,
	inputController,
	nil,
	logger,
)
if err != nil {
	return err
}

mux.Handle("/sessions/example/webrtc", authMiddleware(service.Handler()))
go service.Run(ctx)
```

When `ReplaceExistingPeer` is enabled, `MaxPeers` must be `1`. A new signaling
connection closes the current peer and waits for its resources to be released
before creating the replacement. This fits session supervisors where the most
recent viewer owns the session.

The signaling, control, input, and clipboard messages remain defined in
[`protocol.md`](protocol.md). A custom frontend can implement that protocol
without using the bundled web application.
