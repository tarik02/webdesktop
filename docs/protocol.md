# Protocol

Protocol version 1 uses a WebSocket for signaling and two client-created WebRTC
data channels:

- `control`, reliable and ordered
- `input`, reliable and ordered

One WebSocket owns one peer connection. It accepts one offer and does not
renegotiate.

## Signaling

Signaling messages are UTF-8 JSON text. The client must send an offer within 10
seconds of the WebSocket upgrade. The server pings every 5 seconds and requires
pongs within 15 seconds.

The client offer must include:

- a recv-only video transceiver
- a reliable ordered `control` data channel
- a reliable ordered `input` data channel when remote input is needed
- an active Opus audio media section when server audio is enabled

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

Clients may send candidates before or after the offer. The server queues early
candidates until it installs the remote description. Ignore the browser's
final `icecandidate` event when `event.candidate` is `null`.

Structured error:

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

The peer limit uses error code `peer_limit`.

When tracing is enabled, the embedded client may send bounded diagnostics:

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

Valid levels are `debug`, `info`, `warn`, and `error`. Client logs cannot
contain SDP or ICE candidates.

## Control channel

Control messages are UTF-8 JSON text up to 16 KiB. Each request has a
caller-selected ID.

### Video quality

```json
{
  "version": 1,
  "id": "quality-42",
  "type": "video.quality.set",
  "quality": {
    "codec": "h264",
    "width": 1920,
    "height": 1080,
    "framerate": 60,
    "bitrate_kbps": 10000
  }
}
```

Quality fields are optional and merge with the current settings. At least one
field is required.

Successful response:

```json
{
  "version": 1,
  "id": "quality-42",
  "type": "video.quality.set.result",
  "ok": true,
  "quality": {
    "codec": "h264",
    "width": 1920,
    "height": 1080,
    "framerate": 60,
    "bitrate_kbps": 10000
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

Quality is global because all peers share one encoder. A codec change closes
peers using the old codec and requires a new SDP exchange.

### Input lease

Only one peer can own input.

Acquire:

```json
{
  "version": 1,
  "id": "input-1",
  "type": "input.acquire"
}
```

Successful response:

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

Release:

```json
{
  "version": 1,
  "id": "input-2",
  "type": "input.release"
}
```

The result type is `input.release.result`. Another peer receives `input_busy`.
Other acquisition errors include `input_disabled`, `input_not_ready`,
`input_channel_required`, and unauthorized pointer or keyboard classes.

Closing either data channel, closing the peer, portal shutdown, overload, or
service shutdown releases the lease and any held keys or buttons.

## Input channel

Input messages are UTF-8 JSON text up to 4 KiB. Each message needs a sequence
greater than zero and larger than the previous valid sequence on that channel.

### Absolute pointer motion

```json
{
  "version": 1,
  "sequence": 1,
  "type": "input.pointer.motion.absolute",
  "x": 0.5,
  "y": 0.25
}
```

`x` and `y` are normalized values from 0 through 1.

### Relative pointer motion

```json
{
  "version": 1,
  "sequence": 2,
  "type": "input.pointer.motion.relative",
  "dx": 4.5,
  "dy": -2
}
```

### Pointer button

```json
{
  "version": 1,
  "sequence": 3,
  "type": "input.pointer.button",
  "button": "primary",
  "pressed": true
}
```

`button` may be `primary`, `middle`, `secondary`, `back`, or `forward`.

### Scroll

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

Send a later zero-delta message with the matching stop field set to `true` when
the gesture ends.

### Keyboard

```json
{
  "version": 1,
  "sequence": 5,
  "type": "input.keyboard.key",
  "keycode": 30,
  "pressed": true
}
```

`keycode` is a Linux evdev code from 1 through 767. Browser clients must map
`KeyboardEvent.code` to evdev codes.

Successful input events have no response. Errors include the decoded sequence:

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

The input worker coalesces adjacent motion and continuous scroll events when
ordering remains intact. It never drops key, button, or scroll-stop
transitions. An overload returns `input_overloaded`, releases held state, and
closes the input channel.

## Validation

The server rejects unknown fields, missing required fields, `null` in required
fields, multiple JSON values, binary messages, invalid UTF-8, oversized
messages, and unordered or partial-reliable data channels.
