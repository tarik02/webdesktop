# Embedding the WebRTC transport

The `webrtc` package can be embedded without using webdesktop's portal capture,
GStreamer media service, libei input sender, clipboard controller, Gin
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

Applications can pass an `*input.Controller` directly as the WebRTC
`InputController`. Implement `input.Sender` for the host input API and attach it
with the available pointer and keyboard authorization. The controller retains
lease locking, bounded queueing and motion coalescing, pressed key and button
tracking, overload revocation, and cleanup. The bundled application uses
`input/eis.Sender`; another host can use a compositor control socket instead.
Implement the optional `input.KeyboardTextSender` interface to receive committed
UTF-8 text events. The bundled EIS sender implements it when the EIS server
offers libei's text capability or KWin's Unicode keysym protocol is available.
Without backend support, physical keyboard events still work, but a text event
returns `input.ErrNotReady` and revokes the input lease.

Applications can also reuse `clipboard.Controller` by implementing its
`clipboard.Backend` contract and calling `Attach`. The controller retains
selection subscriptions, generation checks, MIME normalization, reads, writes,
and latest-content tracking, while WebRTC handles transfer framing and limits.

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
