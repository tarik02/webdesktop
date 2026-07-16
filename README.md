# webdesktop

`webdesktop` streams an existing KDE Plasma Wayland session over WebRTC. It
captures through xdg-desktop-portal and PipeWire, encodes with GStreamer, and
uses Pion for WebRTC. There is no X11 capture path or fallback.

## Features

- Runtime-configurable VP8, software H.264, and VA-API H.264 encoder profiles
- Low-latency newest-frame pipeline with bounded queues
- Server-defined quality presets with client-side resolution, frame-rate, and bitrate overrides
- Optional Opus desktop audio
- Portal-authorized pointer and keyboard input through libei
- Bidirectional rich clipboard synchronization through the Clipboard portal
- Embedded browser client and WebSocket signaling
- Persistent portal restore tokens for unattended service restarts

## Requirements

- KDE Plasma on Wayland
- xdg-desktop-portal with the KDE backend and Clipboard portal v1
- PipeWire
- GStreamer with the PipeWire, base, good, bad, and ugly plugins
- libei

The Nix package supplies the userspace dependencies. The VA-API H.264 profile
also needs a working VA-API driver. Desktop audio needs `pipewire-pulse`.

## Run

```bash
nix build
install -Dm600 webdesktop.example.yaml \
  "$HOME/.config/webdesktop/config.yaml"
./result/bin/webdesktop serve \
  --config "$HOME/.config/webdesktop/config.yaml"
```

The first launch opens the Plasma portal prompt. Select a monitor and allow the
session to be restored. Then open `http://127.0.0.1:8080/`.

The service listens on loopback by default. It has no built-in authentication
or TLS, so do not expose it directly to an untrusted network.

## Documentation

- [Deployment and portal setup](docs/deployment.md)
- [Configuration](docs/configuration.md)
- [Media and WebRTC architecture](docs/architecture.md)
- [Embedding the WebRTC transport](docs/embedding.md)
- [Signaling and data-channel protocol](docs/protocol.md)
- [Development and releases](docs/development.md)

## License

Webdesktop is licensed under the [MIT License](LICENSE). See
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md) for bundled third-party
notices.
