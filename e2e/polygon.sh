#!/usr/bin/env bash
set -euo pipefail

polygon_host=${POLYGON_HOST:-polygon}
local_port=${WEBDESKTOP_E2E_LOCAL_PORT:-18080}
target_local_port=${WEBDESKTOP_E2E_TARGET_LOCAL_PORT:-$((20000 + $$ % 10000))}
target_remote_port=${WEBDESKTOP_E2E_TARGET_REMOTE_PORT:-$((30000 + $$ % 10000))}
idle_seconds=${WEBDESKTOP_E2E_IDLE_SECONDS:-45}
sustained_seconds=${WEBDESKTOP_E2E_SUSTAINED_SECONDS:-12}
latency_cycle_count=${WEBDESKTOP_E2E_LATENCY_CYCLES:-6}
local_polygon=false
if [[ $(hostname) == "$polygon_host" ]]; then
	local_polygon=true
	local_port=8080
	target_local_port=$target_remote_port
fi
browser_session="webdesktop-e2e-$$"
remote_tmp=
script_dir=$(dirname "$(readlink -f "$0")")
browser_executable=${AGENT_BROWSER_EXECUTABLE_PATH:-}
browser_command=()
browser_headed=false
xvfb_pid=
openbox_pid=

if ! [[ "$idle_seconds" =~ ^[1-9][0-9]*$ ]] ||
	! [[ "$sustained_seconds" =~ ^[1-9][0-9]*$ ]] ||
	! [[ "$latency_cycle_count" =~ ^[1-9][0-9]*$ ]]; then
	echo "Polygon E2E duration overrides must be positive whole seconds" >&2
	exit 1
fi

local_tmp=$(mktemp -d)
ssh_control=$local_tmp/ssh-control

polygon_exec() {
	if [[ "$local_polygon" == true ]]; then
		"$@"
	else
		command ssh "$polygon_host" "$@"
	fi
}

progress() {
	printf 'webdesktop Polygon E2E: %s\n' "$*" >&2
}

cleanup() {
	local exit_status=$?
	local peers=
	trap - EXIT
	set +e

	browser close >/dev/null 2>&1
	for _ in $(seq 1 20); do
		if [[ "$local_polygon" == true ]]; then
			peers=$(
				curl -fsS http://127.0.0.1:8080/api/status 2>/dev/null |
					jq -r '.active_peers' 2>/dev/null
			)
		else
			peers=$(
				ssh "$polygon_host" \
					"curl -fsS http://127.0.0.1:8080/api/status | jq -r '.active_peers'" \
					2>/dev/null
			)
		fi
		if [[ "$peers" == 0 ]]; then
			break
		fi
		sleep 0.25
	done
	if ((exit_status != 0)) || [[ "$peers" != 0 ]]; then
		polygon_exec systemctl --user restart webdesktop.service >/dev/null 2>&1
		for _ in $(seq 1 40); do
			if [[ "$local_polygon" == true ]]; then
				peers=$(
					curl -fsS http://127.0.0.1:8080/api/status 2>/dev/null |
						jq -r 'select(.ready == true) | .active_peers' 2>/dev/null
				)
			else
				peers=$(
					ssh "$polygon_host" \
						"curl -fsS http://127.0.0.1:8080/api/status | jq -r 'select(.ready == true) | .active_peers'" \
						2>/dev/null
				)
			fi
			if [[ "$peers" == 0 ]]; then
				break
			fi
			sleep 0.25
		done
	fi
	if [[ -n "$remote_tmp" ]]; then
		polygon_exec bash -s -- "$remote_tmp" <<'EOF' >/dev/null 2>&1 || true
      remote_tmp=$1
      for file in "$remote_tmp/target.pid" "$remote_tmp/monitor.pid"; do
        if [[ -s "$file" ]]; then
          pid=$(<"$file")
          kill "$pid" 2>/dev/null || true
          for _ in $(seq 1 20); do
            if ! kill -0 "$pid" 2>/dev/null; then
              break
            fi
            sleep 0.1
          done
          kill -KILL "$pid" 2>/dev/null || true
        fi
      done
      rm -rf "$remote_tmp"
EOF
	fi
	if [[ "$local_polygon" != true ]]; then
		ssh -S "$ssh_control" -O exit "$polygon_host" >/dev/null 2>&1 || true
	fi
	if [[ -n "$openbox_pid" ]]; then
		kill "$openbox_pid" 2>/dev/null || true
		wait "$openbox_pid" 2>/dev/null || true
	fi
	if [[ -n "$xvfb_pid" ]]; then
		kill "$xvfb_pid" 2>/dev/null || true
		wait "$xvfb_pid" 2>/dev/null || true
	fi
	rm -rf "$local_tmp"
	exit "$exit_status"
}
trap cleanup EXIT

if command -v agent-browser >/dev/null 2>&1; then
	browser_command=(agent-browser)
elif command -v nix >/dev/null 2>&1; then
	agent_browser_store_path=$(
		nix build --no-link --print-out-paths nixpkgs#agent-browser
	)
	browser_command=("$agent_browser_store_path/bin/agent-browser")
else
	browser_command=(
		mise x node@22.18.0 pnpm@11.9.0 --
		pnpm dlx agent-browser@0.31.1
	)
fi

browser() {
	local -a browser_options=(--session "$browser_session")
	if [[ -n "$browser_executable" && "$1" == "open" ]]; then
		browser_options+=(--executable-path "$browser_executable")
		if [[ "$browser_headed" == true ]]; then
			browser_options+=(--headed)
		else
			browser_options+=(--headed false)
		fi
	fi

	"${browser_command[@]}" "${browser_options[@]}" "$@"
}

if [[ -z "$browser_executable" ]] && command -v nix >/dev/null 2>&1; then
	chromium_store_path=$(
		nix build --no-link --print-out-paths nixpkgs#chromium
	)
	browser_executable=$chromium_store_path/bin/chromium
fi

wait_polygon_ready() {
	for _ in $(seq 1 60); do
		if [[ "$local_polygon" == true ]]; then
			if curl -fsS http://127.0.0.1:8080/api/status |
				jq -e '.ready == true' >/dev/null; then
				return
			fi
		elif ssh "$polygon_host" \
			"curl -fsS http://127.0.0.1:8080/api/status | jq -e '.ready == true' >/dev/null" \
			2>/dev/null; then
			return
		fi
		sleep 0.5
	done
	echo "webdesktop did not become ready on Polygon" >&2
	exit 1
}

open_quality() {
	if printf '%s\n' \
		'document.getElementById("quality-bitrate") instanceof HTMLInputElement' |
		browser eval --stdin |
		jq -e '. == true' >/dev/null 2>&1; then
		return
	fi
	browser click '[data-testid="quality-trigger"]' >/dev/null || return
	browser wait '#quality-bitrate' >/dev/null || return
}

apply_h264_bitrate() {
	local bitrate=$1
	if [[ "$bitrate" != 8000 && "$bitrate" != 10000 ]]; then
		echo "unsupported H.264 regression bitrate: $bitrate" >&2
		return 1
	fi

	open_quality || return
	printf '%s\n' "
(() => {
  const input = document.getElementById('quality-bitrate');
  if (!(input instanceof HTMLInputElement)) throw new Error('quality-bitrate missing');
  const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')?.set;
  if (!setter) throw new Error('input value setter missing');
  setter.call(input, '$bitrate');
  input.dispatchEvent(new Event('input', { bubbles: true }));
  input.dispatchEvent(new Event('change', { bubbles: true }));
  return true;
})()
" | browser eval --stdin >/dev/null || return

	local started_ms
	started_ms=$(date +%s%3N)
	browser find role button click --name "Apply quality" >/dev/null || return
	local status=
	for _ in $(seq 1 100); do
		status=$(curl -fsS "http://127.0.0.1:$local_port/api/status") || return
		if [[ $(jq -r '.video.bitrate_kbps' <<<"$status") == "$bitrate" ]]; then
			break
		fi
		sleep 0.1
	done
	if [[ $(jq -r '.video.codec' <<<"$status") != h264 ]] ||
		[[ $(jq -r '.video.width' <<<"$status") != 1920 ]] ||
		[[ $(jq -r '.video.height' <<<"$status") != 1080 ]] ||
		[[ $(jq -r '.video.framerate' <<<"$status") != 60 ]] ||
		[[ $(jq -r '.video.bitrate_kbps' <<<"$status") != "$bitrate" ]]; then
		echo "H.264 bitrate change was not applied: $status" >&2
		return 1
	fi
	echo "$(($(date +%s%3N) - started_ms))"
}

measure_sustained_video() {
	local target_before
	target_before=$(curl -fsS "http://127.0.0.1:$target_local_port/status")
	local browser_result
	browser_result=$(
		printf '%s\n' "
(async () => {
  const video = document.querySelector('[data-testid=remote-video]');
  if (!(video instanceof HTMLVideoElement)) throw new Error('remote video missing');
  const canvas = document.createElement('canvas');
  canvas.width = 16;
  canvas.height = 16;
  const context = canvas.getContext('2d', { willReadFrequently: true });
  if (!context) throw new Error('canvas context missing');
  const before = video.getVideoPlaybackQuality();
  let visualSamples = 0;
  let visualChanges = 0;
  let previousVisualHash = null;
  let callbackID = 0;
  const inspectFrame = () => {
    context.drawImage(video, 0, 0, 16, 16);
    const data = context.getImageData(0, 0, 16, 16).data;
    let hash = 2166136261;
    for (let index = 0; index < data.length; index += 16) {
      hash = Math.imul(hash ^ (data[index] >> 4), 16777619);
      hash = Math.imul(hash ^ (data[index + 1] >> 4), 16777619);
      hash = Math.imul(hash ^ (data[index + 2] >> 4), 16777619);
    }
    if (previousVisualHash !== null && hash !== previousVisualHash) {
      visualChanges += 1;
    }
    previousVisualHash = hash;
    visualSamples += 1;
    callbackID = video.requestVideoFrameCallback(inspectFrame);
  };
  callbackID = video.requestVideoFrameCallback(inspectFrame);
  const started = performance.now();
  await new Promise((resolve) => setTimeout(resolve, $((sustained_seconds * 1000))));
  video.cancelVideoFrameCallback(callbackID);
  const elapsed = (performance.now() - started) / 1000;
  const after = video.getVideoPlaybackQuality();
  return {
    elapsed_seconds: elapsed,
    frames: after.totalVideoFrames - before.totalVideoFrames,
    dropped: after.droppedVideoFrames - before.droppedVideoFrames,
    fps: (after.totalVideoFrames - before.totalVideoFrames) / elapsed,
    width: video.videoWidth,
    height: video.videoHeight,
    visual_samples: visualSamples,
    visual_changes: visualChanges,
  };
})()
" | browser eval --stdin
	)
	local target_after
	target_after=$(curl -fsS "http://127.0.0.1:$target_local_port/status")
	jq -nc \
		--argjson browser "$browser_result" \
		--argjson before "$target_before" \
		--argjson after "$target_after" \
		'{
			browser: $browser,
			target_presented_frames: ($after.presented_frame_count - $before.presented_frame_count),
			target_unpresented_frames: ($after.unpresented_frame_count - $before.unpresented_frame_count),
			target_fps: (($after.presented_frame_count - $before.presented_frame_count) / $browser.elapsed_seconds)
		}'
}

measure_visible_change() {
	local sequence=$1
	local pattern=$2
	local baseline_pattern=${3:-}
	if ! [[ "$sequence" =~ ^[0-9]+$ ]] ||
		[[ "$pattern" != red && "$pattern" != green ]] ||
		[[ -n "$baseline_pattern" &&
			"$baseline_pattern" != red &&
			"$baseline_pattern" != green ]]; then
		echo "invalid visible-change request" >&2
		return 1
	fi

	printf '%s\n' "
(async () => {
  const video = document.querySelector('[data-testid=remote-video]');
  if (!(video instanceof HTMLVideoElement)) throw new Error('remote video missing');
  const canvas = document.createElement('canvas');
  canvas.width = 16;
  canvas.height = 16;
  const context = canvas.getContext('2d', { willReadFrequently: true });
  if (!context) throw new Error('canvas context missing');
  const sample = () => {
    context.drawImage(video, 0, 0, 16, 16);
    const data = context.getImageData(0, 0, 16, 16).data;
    let red = 0;
    let green = 0;
    let blue = 0;
    for (let index = 0; index < data.length; index += 4) {
      red += data[index];
      green += data[index + 1];
      blue += data[index + 2];
    }
    const pixels = data.length / 4;
    return [red / pixels, green / pixels, blue / pixels];
  };
  const matches = (color, pattern) =>
    pattern === 'red'
      ? color[0] > color[1] + 60 && color[0] > color[2] + 60
      : color[1] > color[0] + 60 && color[1] > color[2] + 60;
  const expectedBaselinePattern = '$baseline_pattern';
  const baselineSyncStarted = performance.now();
  let baselineSyncCallbacks = 0;
  if (expectedBaselinePattern) {
    await new Promise((resolve, reject) => {
      let callbackID = 0;
      let consecutiveMatches = 0;
      const timeout = setTimeout(() => {
        video.cancelVideoFrameCallback(callbackID);
        reject(
          new Error(
            'baseline color sync timeout: expected=' +
              expectedBaselinePattern +
              ' sampled=' +
              JSON.stringify(sample()),
          ),
        );
      }, 5000);
      const inspectFrame = () => {
        baselineSyncCallbacks += 1;
        if (matches(sample(), expectedBaselinePattern)) {
          consecutiveMatches += 1;
        } else {
          consecutiveMatches = 0;
        }
        if (consecutiveMatches >= 3) {
          clearTimeout(timeout);
          resolve();
          return;
        }
        callbackID = video.requestVideoFrameCallback(inspectFrame);
      };
      callbackID = video.requestVideoFrameCallback(inspectFrame);
    });
  }
  const baselineSyncMs = performance.now() - baselineSyncStarted;
  const baseline = sample();
  const expectedPattern = '$pattern';
  const framesBefore = video.getVideoPlaybackQuality().totalVideoFrames;
  let callbacks = 0;
  let polls = 0;
  let consecutiveMatches = 0;
  let maxDifference = 0;
  const requestStarted = performance.now();
  const detected = new Promise((resolve, reject) => {
    let settled = false;
    let callbackID = 0;
    const interval = setInterval(() => {
      polls += 1;
      inspect('timer');
    }, 5);
    const timeout = setTimeout(() => {
      settled = true;
      clearInterval(interval);
      video.cancelVideoFrameCallback(callbackID);
      reject(new Error('visible change timeout'));
    }, 10000);
    const inspect = (detectedBy, metadata) => {
      if (settled) return true;
      const elapsed = performance.now() - requestStarted;
      if (elapsed >= 10000) {
        settled = true;
        clearTimeout(timeout);
        clearInterval(interval);
        video.cancelVideoFrameCallback(callbackID);
        reject(
          new Error(
            'visible change timeout: elapsed_ms=' +
              elapsed.toFixed(1) +
              ' expected=' +
              expectedPattern +
              ' baseline=' +
              JSON.stringify(baseline) +
              ' sampled=' +
              JSON.stringify(sample()) +
              ' max_difference=' +
              maxDifference.toFixed(1),
          ),
        );
        return true;
      }
      const color = sample();
      const difference = color.reduce(
        (total, value, index) => total + Math.abs(value - baseline[index]),
        0,
      );
      maxDifference = Math.max(maxDifference, difference);
      const expectedColor = matches(color, expectedPattern);
      if (difference >= 80 && expectedColor) {
        consecutiveMatches += 1;
      } else {
        consecutiveMatches = 0;
      }
      if (consecutiveMatches >= 2) {
        settled = true;
        clearTimeout(timeout);
        clearInterval(interval);
        video.cancelVideoFrameCallback(callbackID);
        resolve({
          detectedAt: performance.now(),
          detectedBy,
          color,
          difference,
          mediaTime: metadata?.mediaTime ?? video.currentTime,
          frames: video.getVideoPlaybackQuality().totalVideoFrames,
        });
        return true;
      }
      return false;
    };
    const inspectFrame = (_now, metadata) => {
      callbacks += 1;
      if (!inspect('video-frame-callback', metadata)) {
        callbackID = video.requestVideoFrameCallback(inspectFrame);
      }
    };
    callbackID = video.requestVideoFrameCallback(inspectFrame);
  });
  const responsePromise = fetch(
    'http://127.0.0.1:$target_local_port/trigger?sequence=$sequence&pattern=$pattern',
    { cache: 'no-store' },
  ).then(async (response) => {
    const target = await response.json();
    if (!response.ok) {
      throw new Error(
        'target trigger failed: ' + response.status + ' ' + JSON.stringify(target),
      );
    }
    return { responseAt: performance.now(), target };
  });
  const values = await Promise.all([detected, responsePromise]);
  const detection = values[0];
  const response = values[1];
  return {
    baseline_sync_ms: baselineSyncMs,
    baseline_sync_callbacks: baselineSyncCallbacks,
    latency_ms: detection.detectedAt - requestStarted,
    trigger_response_ms: response.responseAt - requestStarted,
    after_response_ms: detection.detectedAt - response.responseAt,
    target_present_ms:
      (response.target.presentation_feedback_monotonic_ns -
        response.target.command_received_monotonic_ns) /
      1e6,
    baseline,
    color: detection.color,
    difference: detection.difference,
    max_difference: maxDifference,
    callbacks,
    polls,
    detected_by: detection.detectedBy,
    frames_during_detection: detection.frames - framesBefore,
    media_time: detection.mediaTime,
    target: response.target,
  };
})()
" | browser eval --stdin
}

if ! command -v nix >/dev/null 2>&1; then
	echo "Polygon E2E requires Nix for its isolated headed browser" >&2
	exit 1
fi
xorg_store_path=$(nix build --no-link --print-out-paths nixpkgs#xorg.xorgserver)
openbox_store_path=$(nix build --no-link --print-out-paths nixpkgs#openbox)
display_file=$local_tmp/display
exec {display_fd}>"$display_file"
"$xorg_store_path/bin/Xvfb" -displayfd "$display_fd" \
	-screen 0 1440x1000x24 -nolisten tcp \
	>"$local_tmp/xvfb.log" 2>&1 &
xvfb_pid=$!
exec {display_fd}>&-
for _ in $(seq 1 40); do
	if [[ -s "$display_file" ]]; then
		break
	fi
	sleep 0.05
done
if [[ ! -s "$display_file" ]]; then
	echo "Xvfb did not allocate a display" >&2
	exit 1
fi
display_number=$(<"$display_file")
export DISPLAY=:$display_number
"$openbox_store_path/bin/openbox" >"$local_tmp/openbox.log" 2>&1 &
openbox_pid=$!
sleep 0.5

wait_polygon_ready
initial_status=$(polygon_exec curl -fsS http://127.0.0.1:8080/api/status)
if [[ $(jq -r '.active_peers' <<<"$initial_status") != 0 ]]; then
	echo "Polygon E2E requires zero initial peers: $initial_status" >&2
	exit 1
fi
progress "restart service at configured VP8 baseline"
polygon_exec systemctl --user restart webdesktop.service
wait_polygon_ready
initial_status=$(polygon_exec curl -fsS http://127.0.0.1:8080/api/status)
if [[ $(jq -r '.video.codec' <<<"$initial_status") != vp8 ]] ||
	[[ $(jq -r '.video.width' <<<"$initial_status") != 1920 ]] ||
	[[ $(jq -r '.video.height' <<<"$initial_status") != 1080 ]] ||
	[[ $(jq -r '.video.framerate' <<<"$initial_status") != 30 ]] ||
	[[ $(jq -r '.video.bitrate_kbps' <<<"$initial_status") != 4000 ]]; then
	echo "Polygon service did not restart at the configured VP8 baseline: $initial_status" >&2
	exit 1
fi
if [[ "$local_polygon" == true ]]; then
	tracing_enabled=$(
		curl -fsS http://127.0.0.1:8080/api/config |
			jq -r '.tracing.enabled'
	)
else
	tracing_enabled=$(
		ssh "$polygon_host" \
			"curl -fsS http://127.0.0.1:8080/api/config | jq -r '.tracing.enabled'"
	)
fi
if [[ "$tracing_enabled" != true ]]; then
	echo "Polygon tracing is not enabled" >&2
	exit 1
fi
trace_started_epoch=$(polygon_exec date +%s)
if [[ "$local_polygon" != true ]]; then
	ssh -M -S "$ssh_control" -fNT \
		-o ExitOnForwardFailure=yes \
		-L "127.0.0.1:$local_port:127.0.0.1:8080" \
		-L "127.0.0.1:$target_local_port:127.0.0.1:$target_remote_port" \
		"$polygon_host"
fi
remote_tmp=$(polygon_exec mktemp -d /tmp/webdesktop-e2e.XXXXXX)
if [[ "$local_polygon" == true ]]; then
	cp "$script_dir/wayland_target.py" "$remote_tmp/wayland_target.py"
else
	scp -o "ControlPath=$ssh_control" \
		"$script_dir/wayland_target.py" \
		"$polygon_host:$remote_tmp/wayland_target.py"
fi
polygon_exec bash -s -- "$remote_tmp" "$target_remote_port" <<'REMOTE'
remote_tmp=$1
target_port=$2
cat >"$remote_tmp/start-targets.sh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
remote_tmp=$1
target_port=$2
uid=$(id -u)
export XDG_RUNTIME_DIR=/run/user/$uid
export DBUS_SESSION_BUS_ADDRESS=unix:path=$XDG_RUNTIME_DIR/bus
export WAYLAND_DISPLAY=wayland-0
export GDK_BACKEND=wayland
unset DISPLAY XAUTHORITY

target_command=$(printf \
  'exec python %q --listen-address 127.0.0.1 --port %q --ready-file %q' \
  "$remote_tmp/wayland_target.py" \
  "$target_port" \
  "$remote_tmp/target.ready")
nohup stdbuf -oL -eL nix-shell \
  -p gtk4 gobject-introspection \
  'python3.withPackages (ps: [ ps.pygobject3 ])' \
  --run "$target_command" \
  >"$remote_tmp/target.log" 2>&1 &
echo $! >"$remote_tmp/target.pid"
EOF
chmod 0700 "$remote_tmp/start-targets.sh"
REMOTE
polygon_exec bash -s -- "$remote_tmp" "$target_remote_port" <<'EOF'
"$1/start-targets.sh" "$1" "$2"
EOF
for _ in $(seq 1 240); do
	if polygon_exec test -f "$remote_tmp/target.ready"; then
		break
	fi
	sleep 0.25
done
if ! polygon_exec test -f "$remote_tmp/target.ready"; then
	polygon_exec bash -s -- "$remote_tmp" <<'EOF' >&2
cat "$1/target.log"
EOF
	echo "Polygon input target did not start" >&2
	exit 1
fi
target_ready=$(
	polygon_exec bash -s -- "$remote_tmp" <<'EOF'
cat "$1/target.ready"
EOF
)
if [[ $(jq -r '.backend' <<<"$target_ready") != GdkWaylandDisplay ]]; then
	echo "Polygon target did not use Wayland: $target_ready" >&2
	exit 1
fi
for _ in $(seq 1 40); do
	if target_status=$(curl -fsS "http://127.0.0.1:$target_local_port/status" 2>/dev/null); then
		break
	fi
	sleep 0.1
done
if [[ -z ${target_status:-} ]] ||
	(( $(jq -r '.width' <<<"$target_status") <= 0 )) ||
	(( $(jq -r '.height' <<<"$target_status") <= 0 )); then
	echo "Polygon Wayland target did not become drawable: ${target_status:-unavailable}" >&2
	exit 1
fi
window_ids=$(
	polygon_exec bash -s -- "$remote_tmp" <<'EOF'
set -euo pipefail
remote_tmp=$1
export XDG_RUNTIME_DIR=/run/user/$(id -u)
export DBUS_SESSION_BUS_ADDRESS=unix:path=$XDG_RUNTIME_DIR/bus
kdotool=$(nix build --no-link --print-out-paths nixpkgs#kdotool)/bin/kdotool
target=
for _ in $(seq 1 40); do
  target=$("$kdotool" search --name webdesktop-e2e-target --limit 1 | head -n 1)
  if [[ -n "$target" ]]; then
    break
  fi
  sleep 0.1
done
if [[ -z "$target" ]]; then
  echo "failed to find Polygon E2E target window" >&2
  exit 1
fi
"$kdotool" windowactivate "$target"
printf '%s\n%s\n' "$target" "$kdotool"
EOF
)
target_window=$(sed -n '1p' <<<"$window_ids")
kdotool_executable=$(sed -n '2p' <<<"$window_ids")
if [[ -z "$kdotool_executable" ]]; then
	echo "failed to resolve Polygon kdotool executable" >&2
	exit 1
fi

progress "connect isolated headless Chromium"
connect_started_ms=$(date +%s%3N)
browser open "http://127.0.0.1:$local_port/" >/dev/null
browser set viewport 1440 1000 >/dev/null
browser wait '[data-testid="connection-status"]'
browser wait --fn \
	'document.querySelector("[data-testid=connection-status]")?.getAttribute("data-phase") === "connected"'
connect_elapsed_ms=$(($(date +%s%3N) - connect_started_ms))
if ((connect_elapsed_ms > 10000)); then
	echo "initial WebRTC connection took ${connect_elapsed_ms}ms" >&2
	exit 1
fi

video_before=$(
	printf '%s\n' '
(() => {
  const video = document.querySelector("[data-testid=remote-video]");
  if (!(video instanceof HTMLVideoElement)) throw new Error("remote video missing");
  const stream = video.srcObject;
  if (!(stream instanceof MediaStream)) throw new Error("remote stream missing");
  void video.play();
  return {
    frames: video.getVideoPlaybackQuality().totalVideoFrames,
    width: video.videoWidth,
    height: video.videoHeight,
    audioTracks: stream.getAudioTracks().length,
    paused: video.paused,
  };
})()
' | browser eval --stdin
)
sleep 2
video_after=$(
	printf '%s\n' '
(() => {
  const video = document.querySelector("[data-testid=remote-video]");
  if (!(video instanceof HTMLVideoElement)) throw new Error("remote video missing");
  const stream = video.srcObject;
  if (!(stream instanceof MediaStream)) throw new Error("remote stream missing");
  return {
    frames: video.getVideoPlaybackQuality().totalVideoFrames,
    width: video.videoWidth,
    height: video.videoHeight,
    audioTracks: stream.getAudioTracks().length,
    paused: video.paused,
  };
})()
' | browser eval --stdin
)
before_frames=$(jq -er '.frames' <<<"$video_before")
after_frames=$(jq -er '.frames' <<<"$video_after")
video_width=$(jq -er '.width' <<<"$video_after")
video_height=$(jq -er '.height' <<<"$video_after")
audio_tracks=$(jq -er '.audioTracks' <<<"$video_after")
if ((after_frames <= before_frames || video_width == 0 || video_height == 0)); then
	echo "remote video did not render advancing frames: before=$video_before after=$video_after" >&2
	exit 1
fi
audio_enabled=$(
	curl -fsS "http://127.0.0.1:$local_port/api/config" | jq -er '.audio.enabled'
)
if [[ "$audio_enabled" == true && "$audio_tracks" != 1 ]]; then
	echo "enabled audio was not negotiated" >&2
	exit 1
fi
browser click '[data-testid="performance-trigger"]'
browser wait '[data-testid="performance-overlay"]'
browser wait --fn \
	'document.querySelector("[data-testid=performance-overlay]")?.getAttribute("data-ready") === "true"'
browser click '[data-testid="performance-trigger"]'
browser wait --fn \
	'document.querySelector("[data-testid=performance-overlay]") === null'

browser close >/dev/null
browser_headed=true
progress "connect isolated headed Chromium and verify real Wayland input"
input_connect_started_ms=$(date +%s%3N)
browser open "http://127.0.0.1:$local_port/" >/dev/null
browser set viewport 1440 1000 >/dev/null
browser wait '[data-testid="connection-status"]'
browser wait --fn \
	'document.querySelector("[data-testid=connection-status]")?.getAttribute("data-phase") === "connected"'
browser wait --fn \
	'document.querySelector("[data-testid=remote-viewport]")?.getAttribute("data-input-active") === "true"'
input_connect_elapsed_ms=$(($(date +%s%3N) - input_connect_started_ms))
if ((input_connect_elapsed_ms > 10000)); then
	echo "WebRTC reconnect and input lease took ${input_connect_elapsed_ms}ms" >&2
	exit 1
fi

paced_frontend_fps=$(
	printf '%s\n' '
(async () => {
  const video = document.querySelector("[data-testid=remote-video]");
  if (!(video instanceof HTMLVideoElement)) throw new Error("remote video missing for pacing check");
  const before = video.getVideoPlaybackQuality().totalVideoFrames;
  const started = performance.now();
  await new Promise((resolve) => setTimeout(resolve, 11000));
  const elapsed = (performance.now() - started) / 1000;
  return (video.getVideoPlaybackQuality().totalVideoFrames - before) / elapsed;
})()
' | browser eval --stdin | jq -er '.'
)
if awk "BEGIN { exit !($paced_frontend_fps > 45) }"; then
	echo "configured 30 fps decoded at $paced_frontend_fps fps" >&2
	exit 1
fi
browser click '[data-testid="performance-trigger"]'
browser wait --fn \
	'document.querySelector("[data-testid=performance-overlay]")?.getAttribute("data-ready") === "true"'
browser wait --fn '
(() => {
  const overlay = document.querySelector("[data-testid=performance-overlay]");
  if (!(overlay instanceof HTMLElement)) return false;
  const label = Array.from(overlay.querySelectorAll("dt")).find(
    (element) => element.textContent === "Jitter buffer recent",
  );
  const value = label?.nextElementSibling?.textContent;
  return Boolean(value && value !== "—");
})()
'
paced_jitter_buffer_ms=$(
	printf '%s\n' '
(() => {
  const overlay = document.querySelector("[data-testid=performance-overlay]");
  if (!(overlay instanceof HTMLElement)) throw new Error("performance overlay missing");
  const label = Array.from(overlay.querySelectorAll("dt")).find(
    (element) => element.textContent === "Jitter buffer recent",
  );
  const value = label?.nextElementSibling?.textContent;
  if (!value || value === "—") throw new Error("jitter buffer unavailable");
  return Number.parseFloat(value);
})()
' | browser eval --stdin | jq -er '.'
)
browser click '[data-testid="performance-trigger"]'
if awk "BEGIN { exit !($paced_jitter_buffer_ms > 250) }"; then
	echo "browser jitter buffer grew to ${paced_jitter_buffer_ms}ms" >&2
	exit 1
fi

video_point=$(
	printf '%s\n' '
(() => {
  const video = document.querySelector("[data-testid=remote-video]");
  if (!(video instanceof HTMLVideoElement)) throw new Error("remote video missing");
  const bounds = video.getBoundingClientRect();
  const scale = Math.min(bounds.width / video.videoWidth, bounds.height / video.videoHeight);
  const width = video.videoWidth * scale;
  const height = video.videoHeight * scale;
  return {
    x: Math.round(bounds.left + (bounds.width - width) / 2 + width * 420 / video.videoWidth),
    y: Math.round(bounds.top + (bounds.height - height) / 2 + height * 240 / video.videoHeight),
  };
})()
' | browser eval --stdin
)
pointer_x=$(jq -er '.x' <<<"$video_point")
pointer_y=$(jq -er '.y' <<<"$video_point")
target_input_log_line=$(
	polygon_exec bash -s -- "$remote_tmp" <<'EOF'
wc -l <"$1/target.log"
EOF
)
printf '%s\n' '
(() => {
  const viewport = document.querySelector("[data-testid=remote-viewport]");
  if (!(viewport instanceof HTMLElement)) throw new Error("remote viewport missing");
  window.webdesktopE2EPointerId = null;
  viewport.addEventListener("pointerdown", (event) => {
    window.webdesktopE2EPointerId = event.pointerId;
  }, { once: true });
  return true;
})()
' | browser eval --stdin >/dev/null
pointer_started_ms=$(date +%s%3N)
browser mouse move "$pointer_x" "$pointer_y" >/dev/null
browser mouse down left >/dev/null
printf '%s\n' '
(() => {
  const viewport = document.querySelector("[data-testid=remote-viewport]");
  if (!(viewport instanceof HTMLElement)) throw new Error("remote viewport missing");
  const pointerId = window.webdesktopE2EPointerId;
  if (!Number.isInteger(pointerId)) throw new Error("pointer id was not captured");
  if (!viewport.hasPointerCapture(pointerId)) throw new Error("pointer capture was not active");
  viewport.releasePointerCapture(pointerId);
  return true;
})()
' | browser eval --stdin >/dev/null
browser mouse up left >/dev/null
browser wait --fn \
	'document.querySelector("[data-testid=remote-viewport]")?.getAttribute("data-input-active") === "true"'
pointer_remote_x=
pointer_remote_y=
for _ in $(seq 1 20); do
	pointer_state=$(
		polygon_exec bash -s -- "$kdotool_executable" <<'EOF'
export XDG_RUNTIME_DIR=/run/user/$(id -u)
export DBUS_SESSION_BUS_ADDRESS=unix:path=$XDG_RUNTIME_DIR/bus
"$1" getmouselocation --shell
EOF
	)
	pointer_remote_x=$(awk -F= '$1 == "X" { print $2 }' <<<"$pointer_state")
	pointer_remote_y=$(awk -F= '$1 == "Y" { print $2 }' <<<"$pointer_state")
	if ((pointer_remote_x >= 418 && pointer_remote_x <= 422 && \
		pointer_remote_y >= 238 && pointer_remote_y <= 242)); then
		break
	fi
	sleep 0.05
done
pointer_elapsed_ms=$(($(date +%s%3N) - pointer_started_ms))
active_window=
for _ in $(seq 1 20); do
	active_window=$(
		polygon_exec bash -s -- "$kdotool_executable" <<'EOF'
export XDG_RUNTIME_DIR=/run/user/$(id -u)
export DBUS_SESSION_BUS_ADDRESS=unix:path=$XDG_RUNTIME_DIR/bus
"$1" getactivewindow
EOF
	)
	if [[ "$active_window" == "$target_window" ]]; then
		break
	fi
	sleep 0.05
done
if ((pointer_remote_x < 418 || pointer_remote_x > 422 || pointer_remote_y < 238 || pointer_remote_y > 242)); then
	echo "remote pointer reached unexpected coordinates: $pointer_remote_x,$pointer_remote_y" >&2
	exit 1
fi
if ((pointer_elapsed_ms > 2000)); then
	echo "remote pointer input took ${pointer_elapsed_ms}ms" >&2
	exit 1
fi
if [[ "$active_window" != "$target_window" ]]; then
	echo "remote pointer activated unexpected window: $active_window" >&2
	exit 1
fi
pointer_logged=false
for _ in $(seq 1 40); do
	if polygon_exec bash -s -- "$remote_tmp" "$target_input_log_line" <<'EOF'
tail -n "+$(( $2 + 1 ))" "$1/target.log" |
  jq -R 'fromjson?' |
  jq -se '
    any(.[];
      .event == "pointer_press" and
      .button == 1 and
      .x >= 418 and .x <= 422 and
      .y >= 238 and .y <= 242
    ) and
    any(.[]; .event == "pointer_release" and .button == 1)
  ' >/dev/null
EOF
	then
		pointer_logged=true
		break
	fi
	sleep 0.05
done
if [[ "$pointer_logged" != true ]]; then
	polygon_exec bash -s -- "$remote_tmp" "$target_input_log_line" <<'EOF' >&2
tail -n "+$(( $2 + 1 ))" "$1/target.log"
EOF
	echo "remote pointer did not reach the native Wayland target" >&2
	exit 1
fi
if [[ $(
	printf '%s\n' '
document.activeElement === document.querySelector("[data-testid=remote-viewport]")
' | browser eval --stdin
) != true ]]; then
	echo "remote viewport lost keyboard focus" >&2
	exit 1
fi
keyboard_started_ms=$(date +%s%3N)
browser batch --bail "press w" "press Enter" >/dev/null
for _ in $(seq 1 40); do
	if polygon_exec bash -s -- "$remote_tmp" "$target_input_log_line" <<'EOF'; then
tail -n "+$(( $2 + 1 ))" "$1/target.log" |
  jq -R 'fromjson?' |
  jq -se '
    any(.[]; .event == "key_press" and .name == "w") and
    any(.[]; .event == "key_release" and .name == "w") and
    any(.[]; .event == "key_press" and .name == "Return") and
    any(.[]; .event == "key_release" and .name == "Return")
  ' >/dev/null
EOF
		break
	fi
	sleep 0.25
done
keyboard_elapsed_ms=$(($(date +%s%3N) - keyboard_started_ms))
if ! polygon_exec bash -s -- "$remote_tmp" "$target_input_log_line" <<'EOF'; then
tail -n "+$(( $2 + 1 ))" "$1/target.log" |
  jq -R 'fromjson?' |
  jq -se '
    any(.[]; .event == "key_press" and .name == "w") and
    any(.[]; .event == "key_release" and .name == "w") and
    any(.[]; .event == "key_press" and .name == "Return") and
    any(.[]; .event == "key_release" and .name == "Return")
  ' >/dev/null
EOF
	polygon_exec bash -s -- "$remote_tmp" "$target_input_log_line" <<'EOF' >&2
tail -n "+$(( $2 + 1 ))" "$1/target.log"
EOF
	browser get text body >&2 || true
	echo "remote keyboard verification did not reach the native Wayland target" >&2
	exit 1
fi
if ((keyboard_elapsed_ms > 3000)); then
	echo "remote keyboard input took ${keyboard_elapsed_ms}ms" >&2
	exit 1
fi

progress "apply VP8 quality change and reconnect"
open_quality
printf '%s\n' '
(() => {
  const input = document.getElementById("quality-width");
  if (!(input instanceof HTMLInputElement)) throw new Error("quality-width missing");
  const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")?.set;
  if (!setter) throw new Error("input value setter missing");
  setter.call(input, "1024");
  input.dispatchEvent(new Event("input", { bubbles: true }));
  input.dispatchEvent(new Event("change", { bubbles: true }));
  return true;
})()
' | browser eval --stdin >/dev/null
printf '%s\n' '
(() => {
  const input = document.getElementById("quality-height");
  if (!(input instanceof HTMLInputElement)) throw new Error("quality-height missing");
  const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")?.set;
  if (!setter) throw new Error("input value setter missing");
  setter.call(input, "576");
  input.dispatchEvent(new Event("input", { bubbles: true }));
  input.dispatchEvent(new Event("change", { bubbles: true }));
  return true;
})()
' | browser eval --stdin >/dev/null
printf '%s\n' '
(() => {
  const input = document.getElementById("quality-framerate");
  if (!(input instanceof HTMLInputElement)) throw new Error("quality-framerate missing");
  const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")?.set;
  if (!setter) throw new Error("input value setter missing");
  setter.call(input, "24");
  input.dispatchEvent(new Event("input", { bubbles: true }));
  input.dispatchEvent(new Event("change", { bubbles: true }));
  return true;
})()
' | browser eval --stdin >/dev/null
printf '%s\n' '
(() => {
  const input = document.getElementById("quality-bitrate");
  if (!(input instanceof HTMLInputElement)) throw new Error("quality-bitrate missing");
  const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")?.set;
  if (!setter) throw new Error("input value setter missing");
  setter.call(input, "25000");
  input.dispatchEvent(new Event("input", { bubbles: true }));
  input.dispatchEvent(new Event("change", { bubbles: true }));
  return true;
})()
' | browser eval --stdin >/dev/null
browser find role button click --name "Apply quality"
for _ in $(seq 1 40); do
	applied_quality=$(curl -fsS "http://127.0.0.1:$local_port/api/status")
	if [[ $(jq -r '.video.width' <<<"$applied_quality") == 1024 ]] &&
		[[ $(jq -r '.video.height' <<<"$applied_quality") == 576 ]] &&
		[[ $(jq -r '.video.bitrate_kbps' <<<"$applied_quality") == 25000 ]]; then
		break
	fi
	sleep 0.1
done
if [[ $(jq -r '.video.width' <<<"$applied_quality") != 1024 ]] ||
	[[ $(jq -r '.video.height' <<<"$applied_quality") != 576 ]] ||
	[[ $(jq -r '.video.bitrate_kbps' <<<"$applied_quality") != 25000 ]]; then
	echo "quality change was not applied: $applied_quality" >&2
	exit 1
fi

peer_events_before=$(
	polygon_exec bash -s <<'EOF'
uid=$(id -u)
export XDG_RUNTIME_DIR=/run/user/$uid
export DBUS_SESSION_BUS_ADDRESS=unix:path=$XDG_RUNTIME_DIR/bus
journalctl --user -u webdesktop.service --no-pager |
  grep -c '"msg":"WebRTC peer created"' || true
EOF
)
browser click '[data-testid="reconnect"]'
for _ in $(seq 1 40); do
	peer_events_after=$(
		polygon_exec bash -s <<'EOF'
uid=$(id -u)
export XDG_RUNTIME_DIR=/run/user/$uid
export DBUS_SESSION_BUS_ADDRESS=unix:path=$XDG_RUNTIME_DIR/bus
journalctl --user -u webdesktop.service --no-pager |
  grep -c '"msg":"WebRTC peer created"' || true
EOF
	)
	if ((peer_events_after > peer_events_before)); then
		break
	fi
	sleep 0.1
done
if ((peer_events_after <= peer_events_before)); then
	echo "reconnect did not create a new Polygon peer" >&2
	exit 1
fi
browser wait --fn \
	'document.querySelector("[data-testid=connection-status]")?.getAttribute("data-phase") === "connected"'

progress "switch to H.264 1920x1080 at 60 fps"
open_quality
browser select '#quality-codec' h264
printf '%s\n' '
(() => {
  const input = document.getElementById("quality-width");
  if (!(input instanceof HTMLInputElement)) throw new Error("quality-width missing");
  const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")?.set;
  if (!setter) throw new Error("input value setter missing");
  setter.call(input, "1920");
  input.dispatchEvent(new Event("input", { bubbles: true }));
  input.dispatchEvent(new Event("change", { bubbles: true }));
  return true;
})()
' | browser eval --stdin >/dev/null
printf '%s\n' '
(() => {
  const input = document.getElementById("quality-height");
  if (!(input instanceof HTMLInputElement)) throw new Error("quality-height missing");
  const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")?.set;
  if (!setter) throw new Error("input value setter missing");
  setter.call(input, "1080");
  input.dispatchEvent(new Event("input", { bubbles: true }));
  input.dispatchEvent(new Event("change", { bubbles: true }));
  return true;
})()
' | browser eval --stdin >/dev/null
printf '%s\n' '
(() => {
  const input = document.getElementById("quality-framerate");
  if (!(input instanceof HTMLInputElement)) throw new Error("quality-framerate missing");
  const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")?.set;
  if (!setter) throw new Error("input value setter missing");
  setter.call(input, "60");
  input.dispatchEvent(new Event("input", { bubbles: true }));
  input.dispatchEvent(new Event("change", { bubbles: true }));
  return true;
})()
' | browser eval --stdin >/dev/null
printf '%s\n' '
(() => {
  const input = document.getElementById("quality-bitrate");
  if (!(input instanceof HTMLInputElement)) throw new Error("quality-bitrate missing");
  const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value")?.set;
  if (!setter) throw new Error("input value setter missing");
  setter.call(input, "8000");
  input.dispatchEvent(new Event("input", { bubbles: true }));
  input.dispatchEvent(new Event("change", { bubbles: true }));
  return true;
})()
' | browser eval --stdin >/dev/null
codec_started_ms=$(date +%s%3N)
browser find role button click --name "Apply quality"
for _ in $(seq 1 40); do
	applied_quality=$(curl -fsS "http://127.0.0.1:$local_port/api/status")
	if [[ $(jq -r '.video.codec' <<<"$applied_quality") == h264 ]] &&
		[[ $(jq -r '.video.width' <<<"$applied_quality") == 1920 ]] &&
		[[ $(jq -r '.video.height' <<<"$applied_quality") == 1080 ]] &&
		[[ $(jq -r '.video.framerate' <<<"$applied_quality") == 60 ]] &&
			[[ $(jq -r '.video.bitrate_kbps' <<<"$applied_quality") == 8000 ]]; then
		break
	fi
	sleep 0.1
done
if [[ $(jq -r '.video.codec' <<<"$applied_quality") != h264 ]] ||
	[[ $(jq -r '.video.width' <<<"$applied_quality") != 1920 ]] ||
	[[ $(jq -r '.video.height' <<<"$applied_quality") != 1080 ]] ||
	[[ $(jq -r '.video.framerate' <<<"$applied_quality") != 60 ]] ||
	[[ $(jq -r '.video.bitrate_kbps' <<<"$applied_quality") != 8000 ]]; then
	echo "codec change was not applied: $applied_quality" >&2
	exit 1
fi
browser wait --fn \
	'document.querySelector("[data-testid=connection-status]")?.getAttribute("data-phase") === "connected"'
printf '%s\n' '
(async () => {
  const video = document.querySelector("[data-testid=remote-video]");
  if (!(video instanceof HTMLVideoElement)) throw new Error("remote video missing after codec change");
  await new Promise((resolve, reject) => {
    const timeout = setTimeout(() => reject(new Error("codec-change video frame timeout")), 10000);
    video.requestVideoFrameCallback(() => {
      clearTimeout(timeout);
      resolve();
    });
  });
  return true;
})()
' | browser eval --stdin >/dev/null
codec_elapsed_ms=$(($(date +%s%3N) - codec_started_ms))
if ((codec_elapsed_ms > 15000)); then
	echo "codec switch and reconnect took ${codec_elapsed_ms}ms" >&2
	exit 1
fi

progress "measure sustained H.264 at 8,000 Kbit/s"
curl -fsS \
	"http://127.0.0.1:$target_local_port/mode?value=animate&sequence=100" \
	>/dev/null
sustained_8000=$(measure_sustained_video)
sustained_8000_browser_fps=$(jq -r '.browser.fps' <<<"$sustained_8000")
sustained_8000_target_fps=$(jq -r '.target_fps' <<<"$sustained_8000")
if [[ $(jq -r '.browser.width' <<<"$sustained_8000") != 1920 ]] ||
	[[ $(jq -r '.browser.height' <<<"$sustained_8000") != 1080 ]] ||
	! awk "BEGIN {
		browser = $sustained_8000_browser_fps
		target = $sustained_8000_target_fps
		exit !(browser > 35 && target > 45 && browser / target > 0.8)
	}" ||
	(( $(jq -r '.browser.visual_changes' <<<"$sustained_8000") < sustained_seconds * 5 )) ||
	(( $(jq -r '.target_unpresented_frames' <<<"$sustained_8000") > 2 )); then
	echo "H.264 8,000 Kbit/s sustained cadence failed: $sustained_8000" >&2
	exit 1
fi

progress "measure H.264 idle keepalive for ${idle_seconds}s"
curl -fsS \
	"http://127.0.0.1:$target_local_port/mode?value=idle&sequence=101" \
	>/dev/null
baseline_change=$(measure_visible_change 102 red)
if ! awk "BEGIN { exit !($(jq -r '.latency_ms' <<<"$baseline_change") < 1000) }" ||
	! awk "BEGIN { exit !($(jq -r '.target_present_ms' <<<"$baseline_change") < 250) }"; then
	echo "Wayland target baseline did not settle: $baseline_change" >&2
	exit 1
fi
static_frames_before=$(
	printf '%s\n' \
		'document.querySelector("[data-testid=remote-video]")?.getVideoPlaybackQuality().totalVideoFrames' |
		browser eval --stdin |
		jq -er '.'
)
sleep "$idle_seconds"
static_frames_after=$(
	printf '%s\n' \
		'document.querySelector("[data-testid=remote-video]")?.getVideoPlaybackQuality().totalVideoFrames' |
		browser eval --stdin |
		jq -er '.'
)
static_frame_count=$((static_frames_after - static_frames_before))
if ((static_frame_count < idle_seconds * 50)); then
	echo "H.264 idle keepalive cadence failed: ${static_frame_count} frames in ${idle_seconds}s" >&2
	exit 1
fi

peer_events_before_latency=$(
	polygon_exec bash -s -- "$trace_started_epoch" <<'EOF'
export XDG_RUNTIME_DIR=/run/user/$(id -u)
export DBUS_SESSION_BUS_ADDRESS=unix:path=$XDG_RUNTIME_DIR/bus
journalctl --user -u webdesktop.service --since "@$1" --no-pager -o cat |
  grep -c '"msg":"WebRTC peer created"' || true
EOF
)
branch_replacements_before_latency=$(
	polygon_exec bash -s -- "$trace_started_epoch" <<'EOF'
export XDG_RUNTIME_DIR=/run/user/$(id -u)
export DBUS_SESSION_BUS_ADDRESS=unix:path=$XDG_RUNTIME_DIR/bus
journalctl --user -u webdesktop.service --since "@$1" --no-pager -o cat |
  grep -c '"msg":"video encoder branch replaced"' || true
EOF
)
latency_cycles='[]'
baseline_pattern=red
for cycle in $(seq 1 "$latency_cycle_count"); do
	if ((cycle % 2)); then
		bitrate=10000
		pattern=green
	else
		bitrate=8000
		pattern=red
	fi
	progress "latency cycle ${cycle}/${latency_cycle_count}: ${bitrate} Kbit/s and ${pattern}"
	if ! apply_ms=$(apply_h264_bitrate "$bitrate"); then
		echo "failed to apply H.264 bitrate $bitrate" >&2
		exit 1
	fi
	latency=$(measure_visible_change "$((102 + cycle))" "$pattern" "$baseline_pattern")
	baseline_pattern=$pattern
	peer_events_now=$(
		polygon_exec bash -s -- "$trace_started_epoch" <<'EOF'
export XDG_RUNTIME_DIR=/run/user/$(id -u)
export DBUS_SESSION_BUS_ADDRESS=unix:path=$XDG_RUNTIME_DIR/bus
journalctl --user -u webdesktop.service --since "@$1" --no-pager -o cat |
  grep -c '"msg":"WebRTC peer created"' || true
EOF
		)
	new_peers=$((peer_events_now - peer_events_before_latency))
	branch_replacements_now=$(
		polygon_exec bash -s -- "$trace_started_epoch" <<'EOF'
export XDG_RUNTIME_DIR=/run/user/$(id -u)
export DBUS_SESSION_BUS_ADDRESS=unix:path=$XDG_RUNTIME_DIR/bus
journalctl --user -u webdesktop.service --since "@$1" --no-pager -o cat |
  grep -c '"msg":"video encoder branch replaced"' || true
EOF
	)
	new_branch_replacements=$((branch_replacements_now - branch_replacements_before_latency))
	if ((apply_ms > 3000 || new_peers != 0 || new_branch_replacements != 0)) ||
		! awk "BEGIN { exit !($(jq -r '.baseline_sync_ms' <<<"$latency") < 500) }" ||
		! awk "BEGIN { exit !($(jq -r '.latency_ms' <<<"$latency") < 750) }" ||
		! awk "BEGIN { exit !($(jq -r '.after_response_ms' <<<"$latency") < 500) }" ||
		! awk "BEGIN { exit !($(jq -r '.target_present_ms' <<<"$latency") < 250) }"; then
		echo "H.264 idle/change latency cycle $cycle failed: apply=${apply_ms}ms new_peers=$new_peers new_branch_replacements=$new_branch_replacements latency=$latency" >&2
		exit 1
	fi
	latency_cycles=$(
		jq -c \
			--argjson cycle "$cycle" \
			--argjson bitrate "$bitrate" \
			--argjson apply_ms "$apply_ms" \
			--argjson latency "$latency" \
			'. + [{
				cycle: $cycle,
				bitrate_kbps: $bitrate,
				apply_ms: $apply_ms,
				latency: $latency
			}]' \
			<<<"$latency_cycles"
	)
	sleep 1
done

progress "measure sustained H.264 at 10,000 Kbit/s"
if ! sustained_10000_apply_ms=$(apply_h264_bitrate 10000); then
	echo "failed to apply H.264 bitrate 10000 before sustained measurement" >&2
	exit 1
fi
peer_events_now=$(
	polygon_exec bash -s -- "$trace_started_epoch" <<'EOF'
export XDG_RUNTIME_DIR=/run/user/$(id -u)
export DBUS_SESSION_BUS_ADDRESS=unix:path=$XDG_RUNTIME_DIR/bus
journalctl --user -u webdesktop.service --since "@$1" --no-pager -o cat |
  grep -c '"msg":"WebRTC peer created"' || true
EOF
)
branch_replacements_now=$(
	polygon_exec bash -s -- "$trace_started_epoch" <<'EOF'
export XDG_RUNTIME_DIR=/run/user/$(id -u)
export DBUS_SESSION_BUS_ADDRESS=unix:path=$XDG_RUNTIME_DIR/bus
journalctl --user -u webdesktop.service --since "@$1" --no-pager -o cat |
  grep -c '"msg":"video encoder branch replaced"' || true
EOF
)
if ((sustained_10000_apply_ms > 3000)) ||
	((peer_events_now - peer_events_before_latency != 0)) ||
	((branch_replacements_now - branch_replacements_before_latency != 0)); then
	echo "H.264 10,000 Kbit/s sustained setup rebuilt transport: apply=${sustained_10000_apply_ms}ms peers=$((peer_events_now - peer_events_before_latency)) branches=$((branch_replacements_now - branch_replacements_before_latency))" >&2
	exit 1
fi
curl -fsS \
	"http://127.0.0.1:$target_local_port/mode?value=animate&sequence=106" \
	>/dev/null
sustained_10000=$(measure_sustained_video)
sustained_10000_browser_fps=$(jq -r '.browser.fps' <<<"$sustained_10000")
sustained_10000_target_fps=$(jq -r '.target_fps' <<<"$sustained_10000")
if [[ $(jq -r '.browser.width' <<<"$sustained_10000") != 1920 ]] ||
	[[ $(jq -r '.browser.height' <<<"$sustained_10000") != 1080 ]] ||
	! awk "BEGIN {
		browser = $sustained_10000_browser_fps
		target = $sustained_10000_target_fps
		exit !(browser > 35 && target > 45 && browser / target > 0.8)
	}" ||
	(( $(jq -r '.browser.visual_changes' <<<"$sustained_10000") < sustained_seconds * 5 )) ||
	(( $(jq -r '.target_unpresented_frames' <<<"$sustained_10000") > 2 )); then
	echo "H.264 10,000 Kbit/s sustained cadence failed: $sustained_10000" >&2
	exit 1
fi

progress "verify tracing and unattended portal restore"
trace_verified=false
paced_max_video_samples=
for _ in $(seq 1 60); do
	if trace_result=$(
		polygon_exec bash -s -- "$trace_started_epoch" <<'EOF'
set -euo pipefail
export XDG_RUNTIME_DIR=/run/user/$(id -u)
export DBUS_SESSION_BUS_ADDRESS=unix:path=$XDG_RUNTIME_DIR/bus
logs=$(journalctl --user -u webdesktop.service --since "@$1" --no-pager -o cat)
events=$(jq -R 'fromjson?' <<<"$logs")
peer_samples=$(
  jq -sr '
    [
      .[]
      | select(.msg == "peer trace snapshot" and .connected == true and .codec == "vp8")
      | {
          peer_id,
          ts,
          video_samples_seen
        }
    ]
    | group_by(.peer_id)
    | map(
        sort_by(.ts)
        | select(length >= 2)
        | [
            range(1; length) as $index
            | .[$index].video_samples_seen - .[$index - 1].video_samples_seen
          ]
      )
    | flatten
    | max // empty
  ' <<<"$events"
)
grep -F '"logger":"webrtc.client"' <<<"$logs" |
  grep -F '"client_event":"connect.ready"' >/dev/null
test -n "$peer_samples"
printf '%s\n' "$peer_samples"
EOF
	); then
		paced_max_video_samples=$trace_result
		trace_verified=true
		break
	fi
	sleep 0.25
done
if [[ "$trace_verified" != true ]]; then
	polygon_exec bash -s -- "$trace_started_epoch" <<'EOF' >&2
export XDG_RUNTIME_DIR=/run/user/$(id -u)
export DBUS_SESSION_BUS_ADDRESS=unix:path=$XDG_RUNTIME_DIR/bus
journalctl --user -u webdesktop.service --since "@$1" --no-pager -o cat
EOF
	echo "frontend and backend trace events were not recorded on Polygon" >&2
	exit 1
fi
if ((paced_max_video_samples > 225)); then
	echo "configured 30 fps produced $paced_max_video_samples video samples in a sustained five-second trace interval" >&2
	exit 1
fi
printf '%s\n' '
(() => {
  const status = document.querySelector("[data-testid=connection-status]");
  const video = document.querySelector("[data-testid=remote-video]");
  if (!(status instanceof HTMLElement)) throw new Error("connection status missing before restart");
  if (!(video instanceof HTMLVideoElement)) throw new Error("remote video missing before restart");
  if (!(video.srcObject instanceof MediaStream)) throw new Error("remote stream missing before restart");
  const oldTrack = video.srcObject.getVideoTracks()[0];
  if (!oldTrack) throw new Error("remote video track missing before restart");
  const phases = [status.getAttribute("data-phase")];
  const observer = new MutationObserver(() => {
    phases.push(status.getAttribute("data-phase"));
  });
  observer.observe(status, { attributes: true, attributeFilter: ["data-phase"] });
  window.webdesktopE2ERestartState = { oldTrack, phases, observer };
  return true;
})()
' | browser eval --stdin >/dev/null

polygon_exec bash -s -- "$remote_tmp" <<'EOF'
  remote_tmp=$1
  uid=$(id -u)
  export XDG_RUNTIME_DIR=/run/user/$uid
  export DBUS_SESSION_BUS_ADDRESS=unix:path=$XDG_RUNTIME_DIR/bus
  nohup busctl --user monitor org.freedesktop.portal.Desktop \
    >"$remote_tmp/portal-monitor.log" 2>&1 &
  echo $! >"$remote_tmp/monitor.pid"
  systemctl --user restart webdesktop.service
  systemctl --user show webdesktop.service --property=InvocationID --value \
    >"$remote_tmp/service-invocation-id"
EOF
wait_polygon_ready

polygon_exec bash -s -- "$remote_tmp" <<'EOF'
  remote_tmp=$1
  invocation_id=$(<"$remote_tmp/service-invocation-id")
  test -n "$invocation_id"
  journalctl --user "_SYSTEMD_INVOCATION_ID=$invocation_id" --no-pager -o cat |
    grep -F '\"restore_token_attempted\":true' >/dev/null
  journalctl --user "_SYSTEMD_INVOCATION_ID=$invocation_id" --no-pager -o cat |
    grep -F '\"restore_token_rotated\":true' >/dev/null
  grep -E '/org/freedesktop/portal/desktop/(request|session)/' \
    "$remote_tmp/portal-monitor.log" >/dev/null
EOF

browser wait --fn \
	'window.webdesktopE2ERestartState?.phases.includes("error") === true'
browser wait --fn \
	'(() => {
      const state = window.webdesktopE2ERestartState;
      const video = document.querySelector("[data-testid=remote-video]");
      return document.querySelector("[data-testid=connection-status]")?.getAttribute("data-phase") === "connected" &&
        state?.oldTrack.readyState === "ended" &&
        video instanceof HTMLVideoElement &&
        video.srcObject instanceof MediaStream &&
        video.srcObject.getVideoTracks()[0] !== state.oldTrack &&
        video.srcObject.getVideoTracks()[0]?.readyState === "live";
    })()'
printf '%s\n' '
(async () => {
const video = document.querySelector("[data-testid=remote-video]");
if (!(video instanceof HTMLVideoElement)) throw new Error("remote video missing after restart");
window.webdesktopE2ERestartState?.observer.disconnect();
await new Promise((resolve, reject) => {
  const timeout = setTimeout(() => reject(new Error("restored static video frame timeout")), 3000);
  video.requestVideoFrameCallback(() => {
    clearTimeout(timeout);
    resolve();
  });
});
return true;
})()
' | browser eval --stdin >/dev/null
browser close >/dev/null

for _ in $(seq 1 20); do
	if [[ "$local_polygon" == true ]]; then
		peers=$(
			curl -fsS http://127.0.0.1:8080/api/status |
				jq -r '.active_peers'
		)
	else
		peers=$(ssh "$polygon_host" \
			"curl -fsS http://127.0.0.1:8080/api/status | jq -r '.active_peers'")
	fi
	if [[ "$peers" == 0 ]]; then
		break
	fi
	sleep 0.25
done
if [[ "$peers" != 0 ]]; then
	echo "peer cleanup failed: $peers active peers" >&2
	exit 1
fi

latency_max_ms=$(jq -r '[.[].latency.latency_ms] | max' <<<"$latency_cycles")
latency_max_after_response_ms=$(
	jq -r '[.[].latency.after_response_ms] | max' <<<"$latency_cycles"
)
echo "webdesktop Polygon E2E passed: connect=${connect_elapsed_ms}ms input-connect=${input_connect_elapsed_ms}ms pointer=${pointer_elapsed_ms}ms keyboard=${keyboard_elapsed_ms}ms codec-switch=${codec_elapsed_ms}ms pacing=${paced_frontend_fps}fps/${paced_max_video_samples}samples h264-8000=${sustained_8000_browser_fps}fps/${sustained_8000_target_fps}source-fps h264-10000=${sustained_10000_browser_fps}fps/${sustained_10000_target_fps}source-fps/${sustained_10000_apply_ms}ms-apply static-${idle_seconds}s-frames=${static_frame_count} latency-cycles=${latency_cycle_count} latency-max=${latency_max_ms}ms latency-after-presented-response-max=${latency_max_after_response_ms}ms jitter-buffer=${paced_jitter_buffer_ms}ms h264-bitrate-live=verified tracing=verified"
