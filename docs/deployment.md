# Deployment

## Runtime requirements

Webdesktop runs inside an active KDE Plasma Wayland session. It requires:

- `xdg-desktop-portal` and `xdg-desktop-portal-kde`
- PipeWire
- GStreamer core plus the PipeWire, base, good, bad, and ugly plugins
- libei
- `pipewire-pulse` when desktop audio is enabled

The built-in profiles use `vp8enc`, `x264enc`, and `vah264enc`. The VA-API H.264
profile requires a working VA-API driver; the VP8 and software H.264 profiles do
not.

## Build and run

Build the Nix package:

```bash
nix build
```

Create a private configuration file and start the service:

```bash
install -Dm600 webdesktop.example.yaml \
  "$HOME/.config/webdesktop/config.yaml"
./result/bin/webdesktop serve \
  --config "$HOME/.config/webdesktop/config.yaml"
```

The service exposes:

- `GET /`, the embedded browser client
- `GET /healthz`
- `GET /api/config`
- `GET /api/status`
- `GET /webrtc`, the default signaling WebSocket

The health endpoint starts before portal authorization finishes.

## Portal authorization

The packaged application ID is `io.github.tarik02.webdesktop`. Webdesktop
registers this identity before calling the portal. The package also installs a
matching desktop entry.

On first launch, Plasma asks which monitor to share and whether the session may
be restored. Keep the restore option enabled if the service should restart
without asking again.

Webdesktop creates one portal session for screen capture and optional input. It
uses ScreenCast for the PipeWire stream and RemoteDesktop with `ConnectToEIS`
for pointer and keyboard events. Closing the portal session stops media and
input together.

For a trusted unattended installation, KDE can authorize the application ID
for the first launch:

```bash
flatpak permission-set \
  kde-authorized remote-desktop \
  io.github.tarik02.webdesktop yes
systemctl --user restart webdesktop.service
```

Wait until `/api/status` reports `"ready": true`, then remove the bootstrap
permission:

```bash
flatpak permission-remove \
  kde-authorized remote-desktop \
  io.github.tarik02.webdesktop
systemctl --user restart webdesktop.service
```

Later starts use the portal restore token. Webdesktop does not change the KDE
permission store itself.

The restore state is stored at
`$XDG_STATE_HOME/webdesktop/portal-restore.json`, or
`~/.local/state/webdesktop/portal-restore.json` when `XDG_STATE_HOME` is
unset. The directory uses mode 0700 and the file uses mode 0600.

Portal restore tokens are single-use. Webdesktop keeps the previous token until
the portal returns its replacement, then writes the new state atomically.
Changing the monitor, pointer, or keyboard request invalidates the stored state
and opens the consent flow again.

Webdesktop cannot capture or control the login screen or a locked session. It
does not provide remote unlock. Set `input.enabled: false` for view-only
ScreenCast operation.

## systemd user service

Install the package into the user profile:

```bash
nix profile install path:.#
package=$(nix path-info path:.#)
install -Dm600 "$package/share/webdesktop/config.example.yaml" \
  "$HOME/.config/webdesktop/config.yaml"
systemctl --user enable --now \
  "$package/lib/systemd/user/webdesktop.service"
```

The unit belongs to `graphical-session.target`. It stops with the graphical
session and does not restart the shared portal. A denied portal request or
invalid configuration leaves the unit failed instead of reopening the consent
dialog in a loop.

Disable the service with:

```bash
systemctl --user disable --now webdesktop.service
```

## Desktop audio

Audio is disabled by default. It captures a PulseAudio monitor through
PipeWire's PulseAudio-compatible server and encodes stereo Opus at 48 kHz.

The default device is `@DEFAULT_MONITOR@`. An explicit device must end in
`.monitor`; microphone sources are rejected. List available monitor sources
with:

```bash
pactl list short sources
```

Audio starts only after portal authorization succeeds. Losing the selected
monitor source stops the shared desktop session.

## Network and security

Webdesktop authentication is disabled by default. Native password login and
bearer tokens can be enabled independently:

```bash
install -d -m 0700 "$HOME/.config/webdesktop"
printf 'choose-a-unique-password' > "$HOME/.config/webdesktop/password"
openssl rand -base64 32 > "$HOME/.config/webdesktop/bearer-token"
chmod 0600 \
  "$HOME/.config/webdesktop/password" \
  "$HOME/.config/webdesktop/bearer-token"
```

```yaml
auth:
  trusted_proxy_cidrs: []
  login:
    enabled: true
    password_file: /home/user/.config/webdesktop/password
  bearer:
    enabled: true
    token_file: /home/user/.config/webdesktop/bearer-token
  session:
    ttl: 24h
    secure_cookie: true
```

Replace `/home/user` with the service user's absolute home path. The embedded
client accepts either configured credential and exchanges it for an in-memory
browser session. API and non-browser WebSocket clients can instead send
`Authorization: Bearer <token>`. Browser WebSocket APIs cannot set that header,
so the embedded client uses its `HttpOnly` session cookie for signaling.

Authentication protects `/api/config`, `/api/status`, and the configured
WebSocket signaling path. The SPA files, `/healthz`, and authentication session
endpoints remain public so the login screen and health probes can load. Each
client address gets five login attempts immediately, followed by one attempt
every three seconds. Logout, session expiry, and service restart close or
invalidate browser-session peers. None of them changes either credential file.

Webdesktop does not terminate TLS. Set `auth.session.secure_cookie: true` behind
HTTPS. A password, bearer token, or session cookie sent over plain HTTP can be
read by a network observer. Keep the default `127.0.0.1:8080` listener unless a
trusted HTTPS reverse proxy, VPN, or SSH tunnel protects the connection.
Restrict any directly bound address with a firewall. Peers with active input
access can control the unlocked desktop. Enable `input.locking` to restrict
control to one peer at a time.

An SSH tunnel keeps the HTTP and signaling listener private:

```bash
ssh -N -L 8080:127.0.0.1:8080 host
```

Then open `http://127.0.0.1:8080/`.

For direct network access, configure a trusted HTTPS reverse proxy or bind a
LAN address and restrict access with a firewall. A fixed WebRTC UDP range can
be configured on both the host and the service:

```yaml
webrtc:
  udp_port_min: 60000
  udp_port_max: 61000
```

When both values are zero, Pion uses the system ephemeral range.

Host ICE candidates usually work on the same machine or a reachable LAN.
Configure STUN or TURN for NAT traversal. TURN URLs require
`webrtc.ice_username` and `webrtc.ice_credential`.

With an empty `webrtc.allowed_origins` list, browser WebSockets must use the
same host as the HTTP request. Exact `http://` and `https://` origins may be
listed. Authentication cannot be enabled while this list contains `*` because
that would weaken browser session protection.

There is no clipboard, file transfer, gamepad, touch, or remote unlock.
