#!/usr/bin/env bash
set -euo pipefail

polygon_host=${POLYGON_HOST:-polygon}
aperture_url=${APERTURE_URL:-https://aperture.tarik02.me}
aperture_token_file=${APERTURE_TOKEN_FILE:-$HOME/.config/aperture-token}
youtube_url=${YOUTUBE_URL:-https://www.youtube.com/watch?v=YE7VzlLtp-4}
watch_intervals=${WEBDESKTOP_YOUTUBE_INTERVALS:-14}
watch_interval_seconds=${WEBDESKTOP_YOUTUBE_INTERVAL_SECONDS:-5}
minimum_playback_seconds=${WEBDESKTOP_YOUTUBE_MIN_ADVANCE_SECONDS:-60}
minimum_viewer_frames=${WEBDESKTOP_YOUTUBE_MIN_VIEWER_FRAMES:-$((minimum_playback_seconds * 10))}
keep_temp=${WEBDESKTOP_YOUTUBE_KEEP_TEMP:-false}
browser_session="webdesktop-youtube-$$"
local_tmp=$(mktemp -d)
remote_tmp=
aperture_session_id=
service_touched=false

browser() {
	nix shell nixpkgs#agent-browser -c agent-browser --session "$browser_session" "$@"
}

wait_polygon_ready() {
	for _ in $(seq 1 80); do
		if ssh "$polygon_host" \
			"curl -fsS http://127.0.0.1:8080/api/status | jq -e '.ready == true' >/dev/null" \
			2>/dev/null; then
			return
		fi
		sleep 0.25
	done
	echo "webdesktop did not become ready on Polygon" >&2
	exit 1
}

remote_read() {
	ssh "$polygon_host" bash -s -- "$remote_tmp" "$1" <<'EOF'
set -euo pipefail
cat "$1/$2"
EOF
}

cleanup() {
	browser close >/dev/null 2>&1 || true
	if [[ -n "$aperture_session_id" && -f "$aperture_token_file" ]]; then
		aperture_token=$(<"$aperture_token_file")
		curl -fsS -X DELETE \
			-H "Authorization: Bearer $aperture_token" \
			"$aperture_url/api/sessions/$aperture_session_id" \
			>/dev/null 2>&1 || true
	fi
	if [[ -n "$remote_tmp" ]]; then
		ssh "$polygon_host" bash -s -- "$remote_tmp" "$keep_temp" <<'EOF' >/dev/null 2>&1 || true
remote_tmp=$1
keep_temp=$2
touch "$remote_tmp/youtube-stop"
if [[ -s "$remote_tmp/target.pid" ]]; then
  target_pid=$(<"$remote_tmp/target.pid")
  for _ in $(seq 1 50); do
    if ! kill -0 "$target_pid" 2>/dev/null; then
      break
    fi
    sleep 0.1
  done
  kill "$target_pid" 2>/dev/null || true
  for _ in $(seq 1 20); do
    if ! kill -0 "$target_pid" 2>/dev/null; then
      break
    fi
    sleep 0.1
  done
  kill -KILL "$target_pid" 2>/dev/null || true
fi
if [[ "$keep_temp" != true ]]; then
  rm -rf "$remote_tmp"
fi
EOF
	fi
	if [[ "$service_touched" == true ]]; then
		ssh "$polygon_host" bash -s <<'EOF' >/dev/null 2>&1 || true
uid=$(id -u)
export XDG_RUNTIME_DIR=/run/user/$uid
export DBUS_SESSION_BUS_ADDRESS=unix:path=$XDG_RUNTIME_DIR/bus
systemctl --user restart webdesktop.service
for _ in $(seq 1 80); do
  if curl -fsS http://127.0.0.1:8080/api/status |
    jq -e '.ready == true' >/dev/null 2>&1; then
    break
  fi
  sleep 0.25
done
EOF
	fi
	rm -rf "$local_tmp"
}
trap cleanup EXIT

if [[ ! -f "$aperture_token_file" ]]; then
	echo "Aperture token is missing: $aperture_token_file" >&2
	exit 1
fi
if ! ssh "$polygon_host" test -x '$HOME/.nix-profile/bin/firefox'; then
	echo "Firefox is not installed in the Polygon user profile" >&2
	exit 1
fi
preflight_status=$(ssh "$polygon_host" curl -fsS http://127.0.0.1:8080/api/status)
if [[ $(jq -r '.active_peers' <<<"$preflight_status") != 0 ]]; then
	echo "Polygon YouTube gate requires zero initial peers: $preflight_status" >&2
	exit 1
fi

service_touched=true
ssh "$polygon_host" bash -s <<'EOF'
set -euo pipefail
uid=$(id -u)
export XDG_RUNTIME_DIR=/run/user/$uid
export DBUS_SESSION_BUS_ADDRESS=unix:path=$XDG_RUNTIME_DIR/bus
systemctl --user restart webdesktop.service
EOF
wait_polygon_ready

initial_quality=$(ssh "$polygon_host" curl -fsS http://127.0.0.1:8080/api/status)
if [[ $(jq -r '.video.codec' <<<"$initial_quality") != vp8 ]] ||
	[[ $(jq -r '.video.width' <<<"$initial_quality") != 1920 ]] ||
	[[ $(jq -r '.video.height' <<<"$initial_quality") != 1080 ]] ||
	[[ $(jq -r '.video.framerate' <<<"$initial_quality") != 30 ]] ||
	[[ $(jq -r '.video.bitrate_kbps' <<<"$initial_quality") != 4000 ]]; then
	echo "Polygon production video configuration is unexpected: $initial_quality" >&2
	exit 1
fi

remote_tmp=$(ssh "$polygon_host" 'mktemp -d /tmp/webdesktop-youtube.XXXXXX')
scp -q e2e/youtube_target.py "$polygon_host:$remote_tmp/youtube_target.py"
youtube_url_base64=$(printf '%s' "$youtube_url" | base64 -w0)
ssh "$polygon_host" bash -s -- "$remote_tmp" "$youtube_url_base64" <<'EOF'
set -euo pipefail
remote_tmp=$1
youtube_url=$(printf '%s' "$2" | base64 -d)
media_url_path=$remote_tmp/youtube-media-url
uid=$(id -u)
export XDG_RUNTIME_DIR=/run/user/$uid
export DBUS_SESSION_BUS_ADDRESS=unix:path=$XDG_RUNTIME_DIR/bus
export DISPLAY=:0
export WAYLAND_DISPLAY=wayland-0
export MOZ_ENABLE_WAYLAND=1
umask 077
nix shell nixpkgs#yt-dlp -c yt-dlp \
  --no-playlist \
  --no-warnings \
  --get-url \
  -f 'best[ext=mp4][height<=720][acodec!=none]/best[height<=720][acodec!=none]' \
  "$youtube_url" >"$media_url_path"
test "$(wc -l <"$media_url_path")" -eq 1
printf -v target_command 'exec python %q %q %q %q' \
  "$remote_tmp/youtube_target.py" \
  "$remote_tmp" \
  "$HOME/.nix-profile/bin/firefox" \
  "$media_url_path"
nohup nix-shell \
  -p geckodriver 'python3.withPackages (ps: [ ps.selenium ])' \
  --run "$target_command" \
  >"$remote_tmp/target.log" 2>&1 &
printf '%s\n' "$!" >"$remote_tmp/target.pid"
EOF

for _ in $(seq 1 90); do
	target_state=$(remote_read youtube-status.json 2>/dev/null || true)
	if [[ -n "$target_state" ]] &&
		[[ $(jq -r '.found // false' <<<"$target_state") == true ]] &&
		awk "BEGIN { exit !(($(jq -r '.current_time // 0' <<<"$target_state")) > 1) }"; then
		break
	fi
	sleep 0.5
done
if [[ -z "${target_state:-}" ]] ||
	[[ $(jq -r '.found // false' <<<"$target_state") != true ]] ||
	! awk "BEGIN { exit !(($(jq -r '.current_time // 0' <<<"$target_state")) > 1) }"; then
	remote_read target.log >&2
	echo "Firefox did not start YouTube playback" >&2
	exit 1
fi
if ! awk "BEGIN { exit !(($(jq -r '.duration // 0' <<<"$target_state")) > 120) }"; then
	echo "YouTube target is too short: $target_state" >&2
	exit 1
fi
if [[ $(jq -r '.source_host // ""' <<<"$target_state") != *.googlevideo.com ]]; then
	echo "YouTube target did not resolve to Google Video: $target_state" >&2
	exit 1
fi

aperture_token=$(<"$aperture_token_file")
session_response=$local_tmp/aperture-session.json
curl -fsS -X POST \
	-H "Authorization: Bearer $aperture_token" \
	-H 'Content-Type: application/json' \
	-d '{
      "label": "webdesktop YouTube codec gate",
      "baseSnapshotName": "bb",
      "browser": {
        "channel": "chromium",
        "args": []
      },
      "tags": {
        "purpose": "webdesktop-youtube-codec-gate"
      }
    }' \
	"$aperture_url/api/sessions" >"$session_response"
aperture_session_id=$(jq -er '.session.id' "$session_response")
cdp_url=$(jq -er '.cdpUrl' "$session_response")
cdp_token=$(jq -er '.cdpToken' "$session_response")
cdp_version=$local_tmp/cdp-version.json
cdp_ready=false
for _ in $(seq 1 80); do
	if curl -fsS "$cdp_url/$cdp_token/json/version" >"$cdp_version" 2>/dev/null; then
		cdp_ready=true
		break
	fi
	sleep 0.25
done
if [[ "$cdp_ready" != true ]]; then
	echo "Aperture CDP endpoint did not become ready" >&2
	exit 1
fi
browser connect "$(jq -er '.webSocketDebuggerUrl' "$cdp_version")" >/dev/null

connection_started_ms=$(date +%s%3N)
browser open http://polygon.lan:8080/ >/dev/null
browser set viewport 1440 1000 >/dev/null
browser wait --fn \
	'document.querySelector("[data-testid=connection-status]")?.getAttribute("data-phase") === "connected"' \
	>/dev/null
browser wait --fn \
	'document.querySelector("[data-testid=remote-video]")?.readyState >= 2 && document.querySelector("[data-testid=remote-video]")?.videoWidth > 0' \
	>/dev/null
vp8_first_frame_ms=$(($(date +%s%3N) - connection_started_ms))
if ((vp8_first_frame_ms > 5000)); then
	echo "VP8 first frame took ${vp8_first_frame_ms}ms" >&2
	exit 1
fi

viewer_stats() {
	browser eval --stdin <<'EOF'
(() => {
  const video = document.querySelector("[data-testid=remote-video]");
  if (!(video instanceof HTMLVideoElement)) throw new Error("remote video missing");
  const quality = video.getVideoPlaybackQuality();
  return {
    total_frames: quality.totalVideoFrames,
    dropped_frames: quality.droppedVideoFrames,
    current_time: video.currentTime,
    ready_state: video.readyState,
    width: video.videoWidth,
    height: video.videoHeight,
    paused: video.paused,
  };
})()
EOF
}

target_stats() {
	remote_read youtube-status.json
}

watch_codec() {
	local codec=$1
	local viewer_before target_before viewer_after target_after
	local previous_viewer_frames previous_target_time viewer_stalls=0 target_stalls=0

	viewer_before=$(viewer_stats)
	target_before=$(target_stats)
	previous_viewer_frames=$(jq -er '.total_frames' <<<"$viewer_before")
	previous_target_time=$(jq -er '.current_time' <<<"$target_before")

	for interval in $(seq 1 "$watch_intervals"); do
		sleep "$watch_interval_seconds"
		viewer_after=$(viewer_stats)
		target_after=$(target_stats)
		viewer_frames=$(jq -er '.total_frames' <<<"$viewer_after")
		target_time=$(jq -er '.current_time' <<<"$target_after")

		if [[ $(jq -r '.ready_state' <<<"$viewer_after") -lt 2 ]] ||
			[[ $(jq -r '.width' <<<"$viewer_after") -eq 0 ]] ||
			[[ $(jq -r '.height' <<<"$viewer_after") -eq 0 ]]; then
			echo "$codec remote video became unavailable: $viewer_after" >&2
			exit 1
		fi
		if ((viewer_frames <= previous_viewer_frames)); then
			((viewer_stalls += 1))
		else
			viewer_stalls=0
		fi
		if awk "BEGIN { exit !($target_time <= $previous_target_time) }"; then
			((target_stalls += 1))
		else
			target_stalls=0
		fi
		if ((viewer_stalls >= 2)); then
			echo "$codec remote video froze for at least $((watch_interval_seconds * 2)) seconds" >&2
			exit 1
		fi
		if ((target_stalls >= 2)); then
			echo "Firefox state: $target_after" >&2
			remote_read target.log >&2 || true
			remote_read geckodriver.log >&2 || true
			echo "Firefox YouTube playback froze for at least $((watch_interval_seconds * 2)) seconds" >&2
			exit 1
		fi

		printf '%s interval=%d viewer_frames=%d target_time=%.3f\n' \
			"$codec" \
			"$interval" \
			"$viewer_frames" \
			"$target_time"
		previous_viewer_frames=$viewer_frames
		previous_target_time=$target_time
	done

	viewer_delta=$((\
		$(jq -er '.total_frames' <<<"$viewer_after") - \
		$(jq -er '.total_frames' <<<"$viewer_before")))
	target_delta=$(awk "BEGIN { print $(jq -er '.current_time' <<<"$target_after") - $(jq -er '.current_time' <<<"$target_before") }")
	if ! awk "BEGIN { exit !($target_delta >= $minimum_playback_seconds) }"; then
		echo "$codec YouTube playback advanced only ${target_delta}s" >&2
		exit 1
	fi
	if ((viewer_delta < minimum_viewer_frames)); then
		echo "$codec remote video decoded only $viewer_delta frames" >&2
		exit 1
	fi

	printf '%s playback passed: target_advance=%ss viewer_frames=%d\n' \
		"$codec" \
		"$target_delta" \
		"$viewer_delta"
}

watch_codec vp8

browser click '[data-testid="quality-trigger"]' >/dev/null
browser wait '#quality-codec' >/dev/null
browser select '#quality-codec' h264 >/dev/null
h264_started_ms=$(date +%s%3N)
browser find role button click --name "Apply quality" >/dev/null
for _ in $(seq 1 40); do
	applied_codec=$(ssh "$polygon_host" \
		"curl -fsS http://127.0.0.1:8080/api/status | jq -r '.video.codec'")
	if [[ "$applied_codec" == h264 ]]; then
		break
	fi
	sleep 0.1
done
if [[ "$applied_codec" != h264 ]]; then
	echo "H.264 quality change was not applied" >&2
	exit 1
fi
browser wait --fn \
	'document.querySelector("[data-testid=connection-status]")?.getAttribute("data-phase") === "connected"' \
	>/dev/null
browser eval --stdin >/dev/null <<'EOF'
(async () => {
  await new Promise((resolve, reject) => {
    const video = document.querySelector("[data-testid=remote-video]");
    if (!(video instanceof HTMLVideoElement)) {
      reject(new Error("remote video missing after H.264 reconnect"));
      return;
    }
    const timeout = setTimeout(() => reject(new Error("H.264 first frame timeout")), 5000);
    video.requestVideoFrameCallback(() => {
      clearTimeout(timeout);
      resolve();
    });
  });
  return true;
})()
EOF
h264_first_frame_ms=$(($(date +%s%3N) - h264_started_ms))
if ((h264_first_frame_ms > 5000)); then
	echo "H.264 first frame took ${h264_first_frame_ms}ms" >&2
	exit 1
fi

watch_codec h264

echo "webdesktop YouTube codec gate passed: VP8 first-frame=${vp8_first_frame_ms}ms H.264 first-frame=${h264_first_frame_ms}ms playback=$((watch_intervals * watch_interval_seconds))s-per-codec"
