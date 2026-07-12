#!/usr/bin/env python3

import argparse
import colorsys
from importlib import import_module
import json
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
import signal
import sys
import threading
import time
from urllib.parse import parse_qs, urlsplit

import gi

gi.require_version("Gtk", "4.0")
gi.require_version("Gdk", "4.0")
Gdk = import_module("gi.repository.Gdk")
Gio = import_module("gi.repository.Gio")
GLib = import_module("gi.repository.GLib")
Gtk = import_module("gi.repository.Gtk")


COMMAND_TIMEOUT_SECONDS = 5
PATTERN_NAMES = {"black", "blue", "checker", "green", "red", "stripes", "white"}


class TargetHandler(BaseHTTPRequestHandler):
    server_version = "webdesktop-e2e-target"

    def do_OPTIONS(self):
        parsed = urlsplit(self.path)
        if parsed.path not in {"/status", "/mode", "/trigger"} or parsed.query:
            self._json(HTTPStatus.NOT_FOUND, {"error": "not found"})
            return
        self.send_response(HTTPStatus.NO_CONTENT)
        self._cors_headers()
        self.send_header("Access-Control-Allow-Methods", "GET, OPTIONS")
        self.send_header("Access-Control-Allow-Headers", "Content-Type")
        self.send_header("Content-Length", "0")
        self.end_headers()

    def do_GET(self):
        parsed = urlsplit(self.path)
        try:
            query = parse_qs(
                parsed.query,
                keep_blank_values=True,
                strict_parsing=True,
                max_num_fields=3,
            )
        except ValueError:
            self._json(HTTPStatus.BAD_REQUEST, {"error": "invalid query"})
            return

        if parsed.path == "/status":
            if query:
                self._json(HTTPStatus.BAD_REQUEST, {"error": "status takes no query"})
                return
            self._json(HTTPStatus.OK, self.server.target.status())
            return

        if parsed.path == "/mode":
            if set(query) != {"value", "sequence"} or any(
                len(values) != 1 for values in query.values()
            ):
                self._json(
                    HTTPStatus.BAD_REQUEST,
                    {"error": "mode requires one value and one sequence"},
                )
                return
            value = query["value"][0]
            if value not in {"animate", "idle"}:
                self._json(HTTPStatus.BAD_REQUEST, {"error": "invalid mode"})
                return
            sequence = self._sequence(query["sequence"][0])
            if sequence is None:
                return
            command = {
                "event": threading.Event(),
                "sequence": sequence,
                "value": value,
                "result": None,
            }
            GLib.idle_add(self.server.target.apply_mode, command)
            if not command["event"].wait(COMMAND_TIMEOUT_SECONDS):
                self._json(
                    HTTPStatus.GATEWAY_TIMEOUT, {"error": "GTK command timed out"}
                )
                return
            self._json(HTTPStatus.OK, command["result"])
            return

        if parsed.path == "/trigger":
            if set(query) != {"sequence", "pattern"} or any(
                len(values) != 1 for values in query.values()
            ):
                self._json(
                    HTTPStatus.BAD_REQUEST,
                    {"error": "trigger requires one sequence and one pattern"},
                )
                return
            sequence = self._sequence(query["sequence"][0])
            if sequence is None:
                return
            pattern_text = query["pattern"][0]
            if pattern_text in PATTERN_NAMES:
                pattern = pattern_text
            else:
                try:
                    pattern = int(pattern_text)
                except ValueError:
                    self._json(HTTPStatus.BAD_REQUEST, {"error": "invalid pattern"})
                    return
                if str(pattern) != pattern_text or not 0 <= pattern <= 15:
                    self._json(HTTPStatus.BAD_REQUEST, {"error": "invalid pattern"})
                    return

            command = {
                "armed": False,
                "command_received_monotonic_ns": time.monotonic_ns(),
                "event": threading.Event(),
                "failure": None,
                "painted": False,
                "pattern": pattern,
                "presentation_attempts": 0,
                "presentation_feedback_misses": 0,
                "presentation_pending": False,
                "result": None,
                "sequence": sequence,
                "timing_lookup_misses": 0,
            }
            if not self.server.target.reserve_trigger(command):
                self._json(HTTPStatus.CONFLICT, {"error": "trigger already pending"})
                return
            GLib.idle_add(self.server.target.apply_trigger, command)
            if not command["event"].wait(COMMAND_TIMEOUT_SECONDS):
                self.server.target.cancel_trigger(command)
                self._json(HTTPStatus.GATEWAY_TIMEOUT, {"error": "frame timed out"})
                return
            if command["failure"] is not None:
                self._json(
                    HTTPStatus.BAD_GATEWAY,
                    {"error": command["failure"]},
                )
                return
            self._json(HTTPStatus.OK, command["result"])
            return

        self._json(HTTPStatus.NOT_FOUND, {"error": "not found"})

    def _sequence(self, value):
        try:
            sequence = int(value)
        except ValueError:
            self._json(HTTPStatus.BAD_REQUEST, {"error": "invalid sequence"})
            return None
        if str(sequence) != value or not 0 <= sequence <= 2_147_483_647:
            self._json(HTTPStatus.BAD_REQUEST, {"error": "invalid sequence"})
            return None
        return sequence

    def _json(self, status, body):
        encoded = json.dumps(body, separators=(",", ":"), sort_keys=True).encode()
        self.send_response(status)
        self._cors_headers()
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(encoded)))
        self.end_headers()
        self.wfile.write(encoded)

    def _cors_headers(self):
        self.send_header("Access-Control-Allow-Origin", "*")
        self.send_header("Cache-Control", "no-store")

    def log_message(self, _format, *_args):
        return


class TargetApplication(Gtk.Application):
    def __init__(self, listen_address, port, ready_file):
        super().__init__(
            application_id="net.brokenbuild.webdesktop-e2e-target",
            flags=Gio.ApplicationFlags.NON_UNIQUE,
        )
        self.ready_file = ready_file
        self.area = None
        self.window = None
        self.http_server = ThreadingHTTPServer((listen_address, port), TargetHandler)
        self.http_server.daemon_threads = True
        self.http_server.target = self
        self.http_thread = threading.Thread(
            target=self.http_server.serve_forever,
            name="target-http",
            daemon=True,
        )
        self.lock = threading.Lock()
        self.log_lock = threading.Lock()
        self.mode = "animate"
        self.mode_sequence = None
        self.current_pattern = "checker"
        self.frame_count = 0
        self.presented_frame_count = 0
        self.unpresented_frame_count = 0
        self.last_timing_counter = None
        self.width = 0
        self.height = 0
        self.tick_id = None
        self.pending_trigger = None
        self.after_paint_handler = None
        self.initial_presentation_pending = False
        self.ready = False

    def do_activate(self):
        if self.window is not None:
            self.window.present()
            self.area.grab_focus()
            return

        self.window = Gtk.ApplicationWindow(application=self)
        self.window.set_title("webdesktop-e2e-target")
        self.window.connect("close-request", self._close)

        self.area = Gtk.DrawingArea()
        self.area.set_focusable(True)
        self.area.set_draw_func(self._draw)
        self.window.set_child(self.area)

        keys = Gtk.EventControllerKey()
        keys.connect("key-pressed", self._key_pressed)
        keys.connect("key-released", self._key_released)
        self.area.add_controller(keys)

        motion = Gtk.EventControllerMotion()
        motion.connect("motion", self._pointer_motion)
        self.area.add_controller(motion)

        click = Gtk.GestureClick()
        click.set_button(0)
        click.connect("pressed", self._pointer_pressed)
        click.connect("released", self._pointer_released)
        self.area.add_controller(click)

        self.window.fullscreen()
        self.window.present()
        GLib.idle_add(self._ready)

    def _ready(self):
        frame_clock = self.area.get_frame_clock()
        if frame_clock is None:
            return GLib.SOURCE_CONTINUE
        self.after_paint_handler = frame_clock.connect("after-paint", self._after_paint)
        self._start_animation()
        self.area.grab_focus()
        return GLib.SOURCE_REMOVE

    def _mark_ready(self):
        if self.ready:
            return
        self.ready = True
        self.http_thread.start()
        ready = {
            "backend": Gdk.Display.get_default().__gtype__.name,
            "event": "ready",
            "port": self.http_server.server_address[1],
        }
        if self.ready_file is not None:
            self.ready_file.write_text(
                json.dumps(ready, separators=(",", ":"), sort_keys=True) + "\n",
                encoding="utf-8",
            )
        self._log(**ready)
        self._log(event="presentation", title="webdesktop-e2e-target")

    def _start_animation(self):
        if self.tick_id is None:
            self.tick_id = self.area.add_tick_callback(self._tick)

    def _stop_animation(self):
        if self.tick_id is not None:
            self.area.remove_tick_callback(self.tick_id)
            self.tick_id = None

    def _tick(self, _widget, frame_clock):
        self._collect_timings(frame_clock)
        self.area.queue_draw()
        return GLib.SOURCE_CONTINUE

    def _collect_timings(self, frame_clock):
        history_start = frame_clock.get_history_start()
        current = frame_clock.get_frame_counter()
        if self.last_timing_counter is None:
            self.last_timing_counter = history_start - 1
        for counter in range(
            max(history_start, self.last_timing_counter + 1),
            current,
        ):
            timings = frame_clock.get_timings(counter)
            if timings is None or not timings.get_complete():
                break
            if timings.get_presentation_time() > 0:
                self.presented_frame_count += 1
            else:
                self.unpresented_frame_count += 1
            self.last_timing_counter = counter

    def _draw(self, _area, context, width, height):
        with self.lock:
            self.frame_count += 1
            frame_count = self.frame_count
            self.width = width
            self.height = height
            pending = self.pending_trigger
            mode = self.mode
            trigger_frame = (
                pending is not None and pending["armed"] and not pending["painted"]
            )
            if trigger_frame:
                pattern = pending["pattern"]
            elif mode == "animate":
                pattern = frame_count % 16
            else:
                pattern = self.current_pattern

        if mode == "animate" and not trigger_frame:
            red, green, blue = (
                (0.04, 0.12, 0.92),
                (0.92, 0.08, 0.05),
                (0.05, 0.82, 0.18),
                (0.88, 0.76, 0.04),
            )[frame_count % 4]
            context.set_source_rgb(red, green, blue)
            context.paint()
            stripe_width = max(32, width // 16)
            context.set_source_rgb(1 - red, 1 - green, 1 - blue)
            for x in range(
                (frame_count * stripe_width) % (stripe_width * 4) - stripe_width,
                width,
                stripe_width * 4,
            ):
                context.rectangle(x, 0, stripe_width, height)
            context.fill()
        else:
            self._draw_pattern(context, width, height, pattern)

        with self.lock:
            if (
                self.pending_trigger is pending
                and pending is not None
                and pending["armed"]
                and trigger_frame
                and not pending["painted"]
            ):
                pending["presentation_attempts"] += 1
                pending["painted"] = True
                pending["result"] = {
                    "command_received_monotonic_ns": pending[
                        "command_received_monotonic_ns"
                    ],
                    "frame_count": frame_count,
                    "height": height,
                    "pattern": pending["pattern"],
                    "presentation_attempts": pending["presentation_attempts"],
                    "presentation_feedback_misses": pending[
                        "presentation_feedback_misses"
                    ],
                    "sequence": pending["sequence"],
                    "timing_lookup_misses": pending["timing_lookup_misses"],
                    "width": width,
                }

    def _draw_pattern(self, context, width, height, pattern):
        named_colors = {
            "black": (0.0, 0.0, 0.0),
            "blue": (0.05, 0.2, 0.95),
            "green": (0.05, 0.85, 0.25),
            "red": (0.95, 0.08, 0.05),
            "white": (1.0, 1.0, 1.0),
        }
        if pattern in named_colors:
            context.set_source_rgb(*named_colors[pattern])
            context.paint()
            return

        value = (
            pattern if isinstance(pattern, int) else 6 if pattern == "checker" else 11
        )
        red, green, blue = colorsys.hsv_to_rgb((value % 16) / 16, 0.78, 0.92)
        context.set_source_rgb(red, green, blue)
        context.paint()
        context.set_source_rgb(1 - red, 1 - green, 1 - blue)
        tile = max(32, min(width, height) // 10)
        if pattern == "stripes" or value % 2:
            for x in range(0, width, tile * 2):
                context.rectangle(x, 0, tile, height)
        else:
            for y in range(0, height, tile):
                for x in range((y // tile % 2) * tile, width, tile * 2):
                    context.rectangle(x, y, tile, tile)
        context.fill()

    def _after_paint(self, frame_clock):
        if not self.ready and not self.initial_presentation_pending:
            timings = frame_clock.get_current_timings()
            if timings is not None:
                self.initial_presentation_pending = True
                GLib.timeout_add(
                    1,
                    self._wait_for_initial_presentation,
                    timings.copy(),
                )

        with self.lock:
            pending = self.pending_trigger
            if (
                pending is None
                or not pending["painted"]
                or pending["presentation_pending"]
            ):
                return
            timings = frame_clock.get_current_timings()
            if timings is None:
                pending["timing_lookup_misses"] += 1
                pending["painted"] = False
                pending["result"] = None
                self.area.queue_draw()
                return
            pending["presentation_pending"] = True
            pending["result"]["painted_monotonic_ns"] = time.monotonic_ns()
        GLib.timeout_add(
            1,
            self._wait_for_trigger_presentation,
            pending,
            timings.copy(),
        )

    def _wait_for_initial_presentation(self, timings):
        if not timings.get_complete():
            return GLib.SOURCE_CONTINUE
        presentation_time = timings.get_presentation_time()
        if presentation_time <= 0:
            self.initial_presentation_pending = False
            self.area.queue_draw()
            return GLib.SOURCE_REMOVE
        self._mark_ready()
        return GLib.SOURCE_REMOVE

    def _wait_for_trigger_presentation(self, pending, timings):
        with self.lock:
            if self.pending_trigger is not pending:
                return GLib.SOURCE_REMOVE
        if not timings.get_complete():
            return GLib.SOURCE_CONTINUE

        presentation_time = timings.get_presentation_time()
        retry = False
        with self.lock:
            if self.pending_trigger is not pending:
                return GLib.SOURCE_REMOVE
            if presentation_time <= 0:
                pending["presentation_feedback_misses"] += 1
                pending["painted"] = False
                pending["presentation_pending"] = False
                pending["result"] = None
                retry = True
            else:
                self.pending_trigger = None
                self.current_pattern = pending["pattern"]
                pending["result"]["presentation_feedback_monotonic_ns"] = (
                    time.monotonic_ns()
                )
                pending["result"]["presentation_time_us"] = presentation_time
                result = pending["result"]
        if retry:
            self.area.queue_draw()
            return GLib.SOURCE_REMOVE
        self._log(event="trigger", **result)
        pending["event"].set()
        return GLib.SOURCE_REMOVE

    def reserve_trigger(self, command):
        with self.lock:
            if self.pending_trigger is not None:
                return False
            self.pending_trigger = command
            return True

    def apply_trigger(self, command):
        with self.lock:
            if self.pending_trigger is not command:
                return GLib.SOURCE_REMOVE
            command["armed"] = True
        self.area.queue_draw()
        return GLib.SOURCE_REMOVE

    def cancel_trigger(self, command):
        with self.lock:
            if self.pending_trigger is command:
                self.pending_trigger = None
        return GLib.SOURCE_REMOVE

    def apply_mode(self, command):
        with self.lock:
            self.mode = command["value"]
            self.mode_sequence = command["sequence"]
            mode = self.mode
        if mode == "animate":
            self._start_animation()
        else:
            self._stop_animation()
        command["result"] = {
            "mode": mode,
            "sequence": self.mode_sequence,
        }
        command["event"].set()
        return GLib.SOURCE_REMOVE

    def status(self):
        with self.lock:
            return {
                "frame_count": self.frame_count,
                "height": self.height,
                "mode": self.mode,
                "mode_sequence": self.mode_sequence,
                "pending_trigger": self.pending_trigger is not None,
                "presented_frame_count": self.presented_frame_count,
                "unpresented_frame_count": self.unpresented_frame_count,
                "width": self.width,
            }

    def _key_pressed(self, _controller, keyval, keycode, state):
        self._log(
            event="key_press",
            keycode=keycode,
            keyval=keyval,
            name=Gdk.keyval_name(keyval),
            modifiers=int(state),
        )
        return False

    def _key_released(self, _controller, keyval, keycode, state):
        self._log(
            event="key_release",
            keycode=keycode,
            keyval=keyval,
            name=Gdk.keyval_name(keyval),
            modifiers=int(state),
        )

    def _pointer_motion(self, _controller, x, y):
        self._log(event="pointer_motion", x=round(x, 3), y=round(y, 3))

    def _pointer_pressed(self, gesture, count, x, y):
        self.area.grab_focus()
        self._log(
            button=gesture.get_current_button(),
            count=count,
            event="pointer_press",
            x=round(x, 3),
            y=round(y, 3),
        )

    def _pointer_released(self, gesture, count, x, y):
        self._log(
            button=gesture.get_current_button(),
            count=count,
            event="pointer_release",
            x=round(x, 3),
            y=round(y, 3),
        )

    def _log(self, **record):
        record["monotonic_ns"] = time.monotonic_ns()
        with self.log_lock:
            print(
                json.dumps(record, separators=(",", ":"), sort_keys=True),
                flush=True,
            )

    def _close(self, _window):
        self.quit()
        return False

    def close(self):
        if self.http_thread.is_alive():
            self.http_server.shutdown()
            self.http_thread.join()
        self.http_server.server_close()
        if self.ready_file is not None:
            self.ready_file.unlink(missing_ok=True)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--listen-address", default="127.0.0.1")
    parser.add_argument("--port", type=int, default=18081)
    parser.add_argument("--ready-file", type=Path)
    args = parser.parse_args()
    if not 1 <= args.port <= 65535:
        parser.error("--port must be between 1 and 65535")

    Gtk.init()
    display = Gdk.Display.get_default()
    if display is None or display.__gtype__.name != "GdkWaylandDisplay":
        backend = "none" if display is None else display.__gtype__.name
        raise SystemExit(f"Wayland backend required, got {backend}")

    application = TargetApplication(args.listen_address, args.port, args.ready_file)
    signal.signal(signal.SIGINT, lambda _signal, _frame: application.quit())
    signal.signal(signal.SIGTERM, lambda _signal, _frame: application.quit())
    try:
        return application.run([])
    finally:
        application.close()


if __name__ == "__main__":
    sys.exit(main())
