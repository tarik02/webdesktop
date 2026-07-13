import {
  ActivityIcon,
  AlertCircleIcon,
  CircleCheckIcon,
  CircleOffIcon,
  GaugeIcon,
  LoaderCircleIcon,
  Maximize2Icon,
  RefreshCwIcon,
  Volume2Icon,
  VolumeXIcon,
} from "lucide-react";
import {
  useEffect,
  useRef,
  useState,
  type FormEvent,
  type ClipboardEvent as ReactClipboardEvent,
  type KeyboardEvent,
  type PointerEvent,
} from "react";
import { Badge } from "#/components/ui/badge.tsx";
import { Button } from "#/components/ui/button.tsx";
import { Card, CardContent, CardHeader, CardTitle } from "#/components/ui/card.tsx";
import { Field, FieldGroup, FieldLabel, FieldSet } from "#/components/ui/field.tsx";
import { Input } from "#/components/ui/input.tsx";
import { NativeSelect, NativeSelectOption } from "#/components/ui/native-select.tsx";
import { Popover, PopoverContent, PopoverTrigger } from "#/components/ui/popover.tsx";
import { Tooltip, TooltipContent, TooltipTrigger } from "#/components/ui/tooltip.tsx";
import {
  DesktopConnection,
  ProtocolError,
  type ConnectionState,
  type PerformanceStats,
} from "#/lib/desktop-connection.ts";
import { evdevKeycode } from "#/lib/keyboard.ts";
import {
  maximumH264BitrateKbps,
  clipboardMIMESchema,
  maximumVP8BitrateKbps,
  minimumVideoBitrateKbps,
  serverConfigSchema,
  type Quality,
  type ClipboardFormat,
  type ServerConfig,
} from "#/lib/protocol.ts";

const pointerButtons: Readonly<
  Record<number, "primary" | "middle" | "secondary" | "back" | "forward">
> = {
  0: "primary",
  1: "middle",
  2: "secondary",
  3: "back",
  4: "forward",
};

function errorMessage(error: unknown) {
  if (error instanceof ProtocolError) {
    return `${error.code}: ${error.message}`;
  }
  return error instanceof Error ? error.message : "request failed";
}

export function App() {
  const [config, setConfig] = useState<ServerConfig | null>(null);
  const [connectionState, setConnectionState] = useState<ConnectionState>({
    phase: "idle",
  });
  const [quality, setQuality] = useState<Quality | null>(null);
  const [appliedQuality, setAppliedQuality] = useState<Quality | null>(null);
  const [leaseOwned, setLeaseOwned] = useState(false);
  const [audioPlaying, setAudioPlaying] = useState(false);
  const [qualityOpen, setQualityOpen] = useState(false);
  const [performanceOpen, setPerformanceOpen] = useState(false);
  const [performanceStats, setPerformanceStats] = useState<PerformanceStats | null>(null);
  const [error, setError] = useState<string | null>(null);
  const connectionRef = useRef<DesktopConnection | null>(null);
  const videoRef = useRef<HTMLVideoElement>(null);
  const viewportRef = useRef<HTMLDivElement>(null);
  const audioPlayingRef = useRef(false);
  const leaseOwnedRef = useRef(false);
  const inputCapabilitiesRef = useRef({ pointer: false, keyboard: false });
  const inputAcquirePendingRef = useRef(false);
  const wheelStopRef = useRef<number | null>(null);
  const pointerMotionFrameRef = useRef<number | null>(null);
  const pendingPointerRef = useRef<{ x: number; y: number } | null>(null);
  const capturedPointersRef = useRef(new Set<number>());

  useEffect(() => {
    let active = true;
    void fetch("/api/config")
      .then(async (response) => {
        if (!response.ok) {
          throw new Error(`service configuration returned ${response.status}`);
        }
        return serverConfigSchema.parse(await response.json());
      })
      .then((loaded) => {
        if (!active) {
          return;
        }
        setConfig(loaded);
        setQuality(loaded.video);
        setAppliedQuality(loaded.video);
      })
      .catch((cause) => {
        if (active) {
          setError(errorMessage(cause));
        }
      });

    return () => {
      active = false;
      if (pointerMotionFrameRef.current !== null) {
        window.cancelAnimationFrame(pointerMotionFrameRef.current);
        pointerMotionFrameRef.current = null;
      }
      pendingPointerRef.current = null;
      connectionRef.current?.disposeImmediately();
    };
  }, []);

  useEffect(() => {
    const video = videoRef.current;
    if (!video) {
      return;
    }
    const metadata = () => {
      connectionRef.current?.trace("info", "video.metadata", {
        width: String(video.videoWidth),
        height: String(video.videoHeight),
      });
    };
    const playing = () => connectionRef.current?.trace("info", "video.playing");
    const waiting = () => connectionRef.current?.trace("warn", "video.waiting");
    const stalled = () => connectionRef.current?.trace("warn", "video.stalled");
    const ended = () => connectionRef.current?.trace("warn", "video.ended");
    const videoError = () => {
      connectionRef.current?.trace("error", "video.error", {
        media_error_code: video.error ? String(video.error.code) : "unavailable",
      });
    };
    const windowError = (event: ErrorEvent) => {
      connectionRef.current?.trace("error", "window.error", {
        message: event.message || "window error",
      });
    };
    const rejected = (event: PromiseRejectionEvent) => {
      connectionRef.current?.trace("error", "window.unhandled-rejection", {
        message: errorMessage(event.reason),
      });
    };

    video.addEventListener("loadedmetadata", metadata);
    video.addEventListener("playing", playing);
    video.addEventListener("waiting", waiting);
    video.addEventListener("stalled", stalled);
    video.addEventListener("ended", ended);
    video.addEventListener("error", videoError);
    window.addEventListener("error", windowError);
    window.addEventListener("unhandledrejection", rejected);
    return () => {
      video.removeEventListener("loadedmetadata", metadata);
      video.removeEventListener("playing", playing);
      video.removeEventListener("waiting", waiting);
      video.removeEventListener("stalled", stalled);
      video.removeEventListener("ended", ended);
      video.removeEventListener("error", videoError);
      window.removeEventListener("error", windowError);
      window.removeEventListener("unhandledrejection", rejected);
    };
  }, []);

  useEffect(() => {
    const viewport = viewportRef.current;
    if (!viewport) {
      return;
    }
    const wheel = (event: globalThis.WheelEvent) => {
      if (!leaseOwned || !inputCapabilitiesRef.current.pointer) {
        return;
      }
      event.preventDefault();
      const scale = event.deltaMode === 1 ? 16 : event.deltaMode === 2 ? viewport.clientHeight : 1;
      connectionRef.current?.pointerScroll(
        event.deltaX * scale,
        event.deltaY * scale,
        false,
        false,
      );
      if (wheelStopRef.current !== null) {
        window.clearTimeout(wheelStopRef.current);
      }
      wheelStopRef.current = window.setTimeout(() => {
        connectionRef.current?.pointerScroll(0, 0, true, true);
        wheelStopRef.current = null;
      }, 80);
    };

    viewport.addEventListener("wheel", wheel, { passive: false });
    return () => {
      viewport.removeEventListener("wheel", wheel);
      if (wheelStopRef.current !== null) {
        window.clearTimeout(wheelStopRef.current);
        wheelStopRef.current = null;
      }
    };
  }, [leaseOwned]);

  useEffect(() => {
    const release = (cause: "window-blur" | "visibility-hidden") => {
      connectionRef.current?.trace("info", "input.cleanup", { cause });
      if (pointerMotionFrameRef.current !== null) {
        window.cancelAnimationFrame(pointerMotionFrameRef.current);
        pointerMotionFrameRef.current = null;
      }
      pendingPointerRef.current = null;
      void connectionRef.current?.releaseInput().catch((cause) => {
        setError(errorMessage(cause));
      });
    };
    const visibility = () => {
      if (document.visibilityState === "hidden") {
        release("visibility-hidden");
      }
    };
    const blur = () => release("window-blur");
    const unload = () => {
      connectionRef.current?.trace("info", "page.unload");
      connectionRef.current?.disposeImmediately();
    };

    window.addEventListener("blur", blur);
    document.addEventListener("visibilitychange", visibility);
    window.addEventListener("beforeunload", unload);
    return () => {
      window.removeEventListener("blur", blur);
      document.removeEventListener("visibilitychange", visibility);
      window.removeEventListener("beforeunload", unload);
    };
  }, []);

  const connect = async () => {
    if (!config || connectionRef.current) {
      return;
    }
    setError(null);
    const connection = new DesktopConnection(config, {
      onState: (state) => {
        setConnectionState(state);
        if (state.phase === "connected") {
          void captureInput();
        }
        if (state.phase === "idle" || state.phase === "error") {
          connectionRef.current = null;
          leaseOwnedRef.current = false;
          inputAcquirePendingRef.current = false;
          setLeaseOwned(false);
          if (videoRef.current) {
            videoRef.current.srcObject = null;
          }
        }
      },
      onStream: (stream) => {
        const video = videoRef.current;
        if (!video) {
          return;
        }
        if (video.srcObject instanceof MediaStream) {
          for (const track of stream.getTracks()) {
            if (!video.srcObject.getTrackById(track.id)) {
              video.srcObject.addTrack(track);
            }
          }
        } else {
          video.srcObject = new MediaStream(stream.getTracks());
        }
        video.muted = !audioPlayingRef.current;
        if (video.paused) {
          void video.play().catch((cause) => {
            connection.trace("error", "video.play-failed", {
              message: errorMessage(cause),
            });
            setError(errorMessage(cause));
          });
        }
      },
      onQuality: (nextQuality) => {
        setAppliedQuality(nextQuality);
        setQuality(nextQuality);
      },
      onInputLease: (owned) => {
        leaseOwnedRef.current = owned;
        if (!owned) {
          inputCapabilitiesRef.current = { pointer: false, keyboard: false };
        }
        inputAcquirePendingRef.current = false;
        setLeaseOwned(owned);
      },
      onInputError: setError,
      onClipboard: async (formats) => {
        const entries = Object.fromEntries(
          formats
            .filter((format) => ClipboardItem.supports(format.mimeType))
            .map((format) => [format.mimeType, new Blob([format.data], { type: format.mimeType })]),
        );
        if (Object.keys(entries).length > 0) {
          try {
            await navigator.clipboard.write([new ClipboardItem(entries)]);
            setError(null);
            return;
          } catch (cause) {
            if (!formats.some((format) => format.mimeType === "text/plain")) {
              throw cause;
            }
          }
        }
        const text = formats.find((format) => format.mimeType === "text/plain");
        if (text) {
          await navigator.clipboard.writeText(new TextDecoder().decode(text.data));
          setError(null);
        }
      },
      onClipboardError: setError,
    });
    connectionRef.current = connection;
    try {
      await connection.connect();
    } catch (cause) {
      connection.trace("error", "connect.setup-failed", {
        message: errorMessage(cause),
      });
      connection.disposeImmediately();
      connectionRef.current = null;
      setConnectionState({ phase: "error", message: errorMessage(cause) });
    }
  };

  const disconnect = async () => {
    if (pointerMotionFrameRef.current !== null) {
      window.cancelAnimationFrame(pointerMotionFrameRef.current);
      pointerMotionFrameRef.current = null;
    }
    pendingPointerRef.current = null;
    const connection = connectionRef.current;
    connectionRef.current = null;
    if (!connection) {
      return;
    }
    try {
      await connection.disconnect();
    } catch (cause) {
      setError(errorMessage(cause));
      setConnectionState({ phase: "idle" });
    }
    const video = videoRef.current;
    if (video) {
      video.srcObject = null;
    }
  };

  const reconnect = async () => {
    connectionRef.current?.trace("info", "reconnect.requested");
    setError(null);
    await disconnect();
    setConnectionState({ phase: "idle" });
  };

  useEffect(() => {
    if (!config || connectionRef.current) {
      return;
    }
    if (connectionState.phase === "idle") {
      void connect();
      return;
    }
    if (connectionState.phase === "error") {
      const retry = window.setTimeout(() => {
        void fetch("/api/config")
          .then(async (response) => {
            if (!response.ok) {
              throw new Error(`service configuration returned ${response.status}`);
            }
            return serverConfigSchema.parse(await response.json());
          })
          .then((loaded) => {
            setConfig(loaded);
            setQuality(loaded.video);
            setAppliedQuality(loaded.video);
            setConnectionState({ phase: "idle" });
          })
          .catch(() => {
            setConnectionState({ phase: "idle" });
          });
      }, 1000);
      return () => window.clearTimeout(retry);
    }
  }, [config, connectionState.phase]);

  useEffect(() => {
    if (!performanceOpen || connectionState.phase !== "connected") {
      setPerformanceStats(null);
      return;
    }

    let active = true;
    const update = () => {
      void connectionRef.current
        ?.getPerformanceStats()
        .then((stats) => {
          if (active) {
            setPerformanceStats(stats);
          }
        })
        .catch((cause) => {
          if (active) {
            setPerformanceStats(null);
            setError(errorMessage(cause));
          }
        });
    };
    update();
    const interval = window.setInterval(update, 1000);
    return () => {
      active = false;
      window.clearInterval(interval);
    };
  }, [connectionState.phase, performanceOpen]);

  const captureInput = async () => {
    if (
      !config?.input.enabled ||
      !connectionRef.current ||
      leaseOwnedRef.current ||
      inputAcquirePendingRef.current
    ) {
      return;
    }
    inputAcquirePendingRef.current = true;
    try {
      inputCapabilitiesRef.current = await connectionRef.current.acquireInput();
      viewportRef.current?.focus();
      setError(null);
    } catch (cause) {
      setError(errorMessage(cause));
    } finally {
      inputAcquirePendingRef.current = false;
    }
  };

  const releaseInput = async () => {
    if (pointerMotionFrameRef.current !== null) {
      window.cancelAnimationFrame(pointerMotionFrameRef.current);
      pointerMotionFrameRef.current = null;
    }
    pendingPointerRef.current = null;
    inputAcquirePendingRef.current = false;
    try {
      await connectionRef.current?.releaseInput();
    } catch (cause) {
      setError(errorMessage(cause));
    }
  };

  const applyQuality = async (event: FormEvent) => {
    event.preventDefault();
    if (!quality || !appliedQuality || !connectionRef.current) {
      return;
    }
    setError(null);
    try {
      const codecChanged = quality.codec !== appliedQuality.codec;
      const nextQuality = await connectionRef.current.setQuality({
        ...(quality.codec === appliedQuality.codec ? {} : { codec: quality.codec }),
        width: quality.width,
        height: quality.height,
        framerate: quality.framerate,
        bitrate_kbps: quality.bitrate_kbps,
      });
      setQualityOpen(false);
      if (codecChanged) {
        setConfig((current) => (current ? { ...current, video: nextQuality } : current));
        await reconnect();
      }
    } catch (cause) {
      setError(errorMessage(cause));
    }
  };

  const normalizedPoint = (event: PointerEvent<HTMLDivElement>) => {
    const video = videoRef.current;
    if (!video || video.videoWidth === 0 || video.videoHeight === 0) {
      return null;
    }
    const bounds = video.getBoundingClientRect();
    const scale = Math.min(bounds.width / video.videoWidth, bounds.height / video.videoHeight);
    const width = video.videoWidth * scale;
    const height = video.videoHeight * scale;
    const left = bounds.left + (bounds.width - width) / 2;
    const top = bounds.top + (bounds.height - height) / 2;
    if (
      event.clientX < left ||
      event.clientX > left + width ||
      event.clientY < top ||
      event.clientY > top + height
    ) {
      return null;
    }
    return {
      x: Math.min(1, Math.max(0, (event.clientX - left) / width)),
      y: Math.min(1, Math.max(0, (event.clientY - top) / height)),
    };
  };

  const pointerMotion = (event: PointerEvent<HTMLDivElement>) => {
    if (!leaseOwnedRef.current || !inputCapabilitiesRef.current.pointer) {
      return;
    }
    const point = normalizedPoint(event);
    if (!point) {
      return;
    }
    pendingPointerRef.current = point;
    if (pointerMotionFrameRef.current === null) {
      pointerMotionFrameRef.current = window.requestAnimationFrame(() => {
        pointerMotionFrameRef.current = null;
        const pending = pendingPointerRef.current;
        pendingPointerRef.current = null;
        if (pending && leaseOwnedRef.current) {
          connectionRef.current?.pointerAbsolute(pending.x, pending.y);
        }
      });
    }
  };

  const pointerDown = (event: PointerEvent<HTMLDivElement>) => {
    event.currentTarget.focus();
    if (!leaseOwnedRef.current) {
      event.preventDefault();
      void captureInput();
      return;
    }
    if (!inputCapabilitiesRef.current.pointer) {
      return;
    }
    event.preventDefault();
    const point = normalizedPoint(event);
    if (point) {
      pendingPointerRef.current = null;
      connectionRef.current?.pointerAbsolute(point.x, point.y);
    }
    const button = pointerButtons[event.button];
    if (button) {
      capturedPointersRef.current.add(event.pointerId);
      event.currentTarget.setPointerCapture(event.pointerId);
      connectionRef.current?.pointerButton(button, true);
    }
  };

  const pointerUp = (event: PointerEvent<HTMLDivElement>) => {
    if (!leaseOwnedRef.current || !inputCapabilitiesRef.current.pointer) {
      return;
    }
    event.preventDefault();
    const point = normalizedPoint(event);
    if (point) {
      pendingPointerRef.current = null;
      connectionRef.current?.pointerAbsolute(point.x, point.y);
    }
    const button = pointerButtons[event.button];
    if (button) {
      connectionRef.current?.pointerButton(button, false);
    }
    capturedPointersRef.current.delete(event.pointerId);
  };

  const pointerCaptureLost = (event: PointerEvent<HTMLDivElement>) => {
    if (!capturedPointersRef.current.delete(event.pointerId)) {
      return;
    }
    connectionRef.current?.trace("debug", "pointer-capture.lost");
    if (pointerMotionFrameRef.current !== null) {
      window.cancelAnimationFrame(pointerMotionFrameRef.current);
      pointerMotionFrameRef.current = null;
    }
    pendingPointerRef.current = null;
    connectionRef.current?.releasePressed();
  };

  const paste = async (event: ReactClipboardEvent<HTMLDivElement>) => {
    if (
      !config?.clipboard.enabled ||
      !leaseOwnedRef.current ||
      !inputCapabilitiesRef.current.keyboard ||
      !connectionRef.current
    ) {
      return;
    }
    event.preventDefault();
    const formats = new Map<ClipboardFormat["mimeType"], ClipboardFormat>();
    for (const mimeType of event.clipboardData.types) {
      const supported = clipboardMIMESchema.safeParse(mimeType.toLowerCase());
      if (!supported.success || !supported.data.startsWith("text/")) {
        continue;
      }
      const encoded = new TextEncoder().encode(event.clipboardData.getData(mimeType));
      formats.set(supported.data, {
        mimeType: supported.data,
        data: encoded.buffer,
      });
    }
    for (const item of event.clipboardData.items) {
      const supported = clipboardMIMESchema.safeParse(item.type.toLowerCase());
      if (!supported.success || item.kind !== "file") {
        continue;
      }
      const file = item.getAsFile();
      if (file) {
        formats.set(supported.data, {
          mimeType: supported.data,
          data: await file.arrayBuffer(),
        });
      }
    }
    try {
      await connectionRef.current.pasteClipboard([...formats.values()]);
      setError(null);
    } catch (cause) {
      setError(errorMessage(cause));
    }
  };

  const keyboard = (event: KeyboardEvent<HTMLDivElement>, pressed: boolean) => {
    if (!leaseOwnedRef.current || !inputCapabilitiesRef.current.keyboard) {
      return;
    }
    if (pressed && event.code === "Escape" && event.ctrlKey && event.altKey && event.shiftKey) {
      event.preventDefault();
      void releaseInput();
      return;
    }
    if (
      pressed &&
      config?.clipboard.enabled &&
      ((event.code === "KeyV" && (event.ctrlKey || event.metaKey)) ||
        (event.code === "Insert" && event.shiftKey))
    ) {
      return;
    }
    const keycode = evdevKeycode(event.code);
    if (!keycode) {
      return;
    }
    event.preventDefault();
    if (!pressed || !event.repeat) {
      connectionRef.current?.keyboardKey(keycode, pressed);
    }
  };

  const toggleAudio = async (checked: boolean) => {
    connectionRef.current?.trace("info", "audio.playback-toggle", {
      enabled: String(checked),
    });
    const video = videoRef.current;
    audioPlayingRef.current = checked;
    setAudioPlaying(checked);
    if (!video) {
      return;
    }
    video.muted = !checked;
    if (checked) {
      try {
        await video.play();
      } catch (cause) {
        connectionRef.current?.trace("error", "audio.playback-failed", {
          message: errorMessage(cause),
        });
        audioPlayingRef.current = false;
        setAudioPlaying(false);
        video.muted = true;
        setError(errorMessage(cause));
      }
    }
  };

  const fullscreen = async () => {
    connectionRef.current?.trace("info", "fullscreen.requested");
    try {
      await viewportRef.current?.requestFullscreen();
    } catch (cause) {
      connectionRef.current?.trace("error", "fullscreen.failed", {
        message: errorMessage(cause),
      });
      setError(errorMessage(cause));
    }
  };

  const connected = connectionState.phase === "connected";
  const statusLabel =
    connectionState.phase === "connecting"
      ? connectionState.detail
      : connectionState.phase === "connected"
        ? "connected"
        : connectionState.phase === "error"
          ? connectionState.message
          : "disconnected";

  return (
    <main className="relative h-svh w-screen overflow-hidden bg-black">
      <div
        ref={viewportRef}
        data-testid="remote-viewport"
        data-input-active={leaseOwned}
        className="absolute inset-0 flex items-center justify-center overflow-hidden bg-black outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-inset"
        role="application"
        aria-label="Remote Plasma desktop"
        tabIndex={0}
        onPointerEnter={() => {
          if (connected) {
            void captureInput();
          }
        }}
        onFocus={() => {
          if (connected) {
            void captureInput();
          }
        }}
        onPointerMove={pointerMotion}
        onPointerDown={pointerDown}
        onPointerUp={pointerUp}
        onLostPointerCapture={pointerCaptureLost}
        onPointerCancel={pointerCaptureLost}
        onKeyDown={(event) => keyboard(event, true)}
        onKeyUp={(event) => keyboard(event, false)}
        onPaste={(event) => void paste(event)}
        onContextMenu={(event) => event.preventDefault()}
      >
        <video
          ref={videoRef}
          data-testid="remote-video"
          className="size-full object-contain"
          autoPlay
          playsInline
          muted={!audioPlaying}
        />
      </div>

      <div className="pointer-events-none absolute inset-x-0 top-0 flex justify-center p-3">
        <div
          className="pointer-events-auto flex max-w-full flex-wrap items-center gap-1 rounded-xl border bg-background/90 p-2 shadow-lg backdrop-blur"
          role="toolbar"
          aria-label="Remote desktop controls"
        >
          <Tooltip>
            <TooltipTrigger
              render={
                <Badge
                  data-testid="connection-status"
                  data-phase={connectionState.phase}
                  className="size-8 p-0"
                  variant={
                    connectionState.phase === "error"
                      ? "destructive"
                      : connected
                        ? "default"
                        : "secondary"
                  }
                  aria-label={statusLabel}
                  tabIndex={0}
                />
              }
            >
              {connectionState.phase === "connected" ? (
                <CircleCheckIcon />
              ) : connectionState.phase === "connecting" ? (
                <LoaderCircleIcon className="animate-spin" />
              ) : connectionState.phase === "error" ? (
                <AlertCircleIcon />
              ) : (
                <CircleOffIcon />
              )}
            </TooltipTrigger>
            <TooltipContent side="bottom">{statusLabel}</TooltipContent>
          </Tooltip>
          {error && connectionState.phase !== "error" ? (
            <Tooltip>
              <TooltipTrigger
                render={
                  <Badge
                    data-testid="connection-error"
                    className="size-8 p-0"
                    variant="destructive"
                    aria-label={error}
                    tabIndex={0}
                  />
                }
              >
                <AlertCircleIcon />
              </TooltipTrigger>
              <TooltipContent side="bottom">{error}</TooltipContent>
            </Tooltip>
          ) : null}

          <Popover open={qualityOpen} onOpenChange={setQualityOpen}>
            <Tooltip>
              <TooltipTrigger render={<span className="inline-flex" />}>
                <PopoverTrigger
                  render={
                    <Button
                      data-testid="quality-trigger"
                      variant="outline"
                      size="icon"
                      aria-label="Quality"
                      disabled={!config}
                    />
                  }
                >
                  <GaugeIcon />
                </PopoverTrigger>
              </TooltipTrigger>
              <TooltipContent side="bottom">Quality</TooltipContent>
            </Tooltip>
            <PopoverContent className="w-[min(22rem,calc(100vw-1.5rem))]" align="end" side="bottom">
              <form id="quality-form" onSubmit={(event) => void applyQuality(event)}>
                <FieldGroup>
                  <FieldSet disabled={!connected || !quality}>
                    <Field>
                      <FieldLabel htmlFor="quality-codec">Codec</FieldLabel>
                      <NativeSelect
                        id="quality-codec"
                        className="w-full"
                        value={quality?.codec ?? ""}
                        onChange={(event) => {
                          const codec = event.currentTarget.value;
                          if (codec === "vp8" || codec === "h264") {
                            setQuality((current) => (current ? { ...current, codec } : current));
                          }
                        }}
                      >
                        <NativeSelectOption value="vp8">VP8</NativeSelectOption>
                        <NativeSelectOption value="h264">H.264</NativeSelectOption>
                      </NativeSelect>
                    </Field>
                    <FieldGroup className="grid grid-cols-2 gap-3">
                      <Field>
                        <FieldLabel htmlFor="quality-width">Width</FieldLabel>
                        <Input
                          id="quality-width"
                          type="number"
                          min={320}
                          max={7680}
                          step={2}
                          value={quality?.width ?? ""}
                          onChange={(event) => {
                            const width = event.currentTarget.valueAsNumber;
                            setQuality((current) => (current ? { ...current, width } : current));
                          }}
                        />
                      </Field>
                      <Field>
                        <FieldLabel htmlFor="quality-height">Height</FieldLabel>
                        <Input
                          id="quality-height"
                          type="number"
                          min={240}
                          max={4320}
                          step={2}
                          value={quality?.height ?? ""}
                          onChange={(event) => {
                            const height = event.currentTarget.valueAsNumber;
                            setQuality((current) => (current ? { ...current, height } : current));
                          }}
                        />
                      </Field>
                      <Field>
                        <FieldLabel htmlFor="quality-framerate">Frame rate</FieldLabel>
                        <Input
                          id="quality-framerate"
                          type="number"
                          min={1}
                          max={120}
                          value={quality?.framerate ?? ""}
                          onChange={(event) => {
                            const framerate = event.currentTarget.valueAsNumber;
                            setQuality((current) =>
                              current ? { ...current, framerate } : current,
                            );
                          }}
                        />
                      </Field>
                      <Field>
                        <FieldLabel htmlFor="quality-bitrate">Bitrate Kbit/s</FieldLabel>
                        <Input
                          id="quality-bitrate"
                          type="number"
                          min={minimumVideoBitrateKbps}
                          max={
                            quality?.codec === "h264"
                              ? maximumH264BitrateKbps
                              : maximumVP8BitrateKbps
                          }
                          value={quality?.bitrate_kbps ?? ""}
                          onChange={(event) => {
                            const bitrate_kbps = event.currentTarget.valueAsNumber;
                            setQuality((current) =>
                              current ? { ...current, bitrate_kbps } : current,
                            );
                          }}
                        />
                      </Field>
                    </FieldGroup>
                  </FieldSet>
                  <Button type="submit" disabled={!connected || !quality}>
                    Apply quality
                  </Button>
                </FieldGroup>
              </form>
            </PopoverContent>
          </Popover>

          <Tooltip>
            <TooltipTrigger
              render={
                <Button
                  data-testid="performance-trigger"
                  variant="outline"
                  size="icon"
                  aria-label="Performance"
                  aria-pressed={performanceOpen}
                  disabled={!connected}
                  onClick={() => setPerformanceOpen((open) => !open)}
                />
              }
            >
              <ActivityIcon />
            </TooltipTrigger>
            <TooltipContent side="bottom">Performance</TooltipContent>
          </Tooltip>

          {config?.audio.enabled ? (
            <Tooltip>
              <TooltipTrigger
                render={
                  <Button
                    variant="outline"
                    size="icon"
                    aria-label={audioPlaying ? "Mute desktop audio" : "Play desktop audio"}
                    aria-pressed={audioPlaying}
                    disabled={!connected}
                    onClick={() => void toggleAudio(!audioPlaying)}
                  />
                }
              >
                {audioPlaying ? <Volume2Icon /> : <VolumeXIcon />}
              </TooltipTrigger>
              <TooltipContent side="bottom">
                {audioPlaying ? "Mute desktop audio" : "Play desktop audio"}
              </TooltipContent>
            </Tooltip>
          ) : null}
          <Tooltip>
            <TooltipTrigger
              render={
                <Button
                  variant="outline"
                  size="icon"
                  aria-label="Fullscreen"
                  disabled={!connected}
                  onClick={() => void fullscreen()}
                />
              }
            >
              <Maximize2Icon />
            </TooltipTrigger>
            <TooltipContent side="bottom">Fullscreen</TooltipContent>
          </Tooltip>
          <Tooltip>
            <TooltipTrigger
              render={
                <Button
                  data-testid="reconnect"
                  variant="outline"
                  size="icon"
                  aria-label="Reconnect"
                  disabled={!config}
                  onClick={() => void reconnect()}
                />
              }
            >
              <RefreshCwIcon />
            </TooltipTrigger>
            <TooltipContent side="bottom">Reconnect</TooltipContent>
          </Tooltip>
        </div>
      </div>

      {performanceOpen ? (
        <div className="pointer-events-none absolute bottom-3 left-3">
          <Card
            data-testid="performance-overlay"
            data-ready={performanceStats !== null}
            size="sm"
            className="w-72 bg-card/90 shadow-lg backdrop-blur"
          >
            <CardHeader>
              <CardTitle>Performance</CardTitle>
            </CardHeader>
            <CardContent>
              {performanceStats ? (
                <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1 tabular-nums">
                  <dt className="text-muted-foreground">Stream</dt>
                  <dd className="text-right">
                    {appliedQuality?.codec.toUpperCase() ?? "—"} ·{" "}
                    {performanceStats.width && performanceStats.height
                      ? `${performanceStats.width}×${performanceStats.height}`
                      : "—"}
                  </dd>
                  <dt className="text-muted-foreground">FPS</dt>
                  <dd className="text-right">
                    {performanceStats.framesPerSecond?.toFixed(1) ?? "—"}
                  </dd>
                  <dt className="text-muted-foreground">Receive</dt>
                  <dd className="text-right">
                    {performanceStats.bitrateBps === null
                      ? "—"
                      : `${(performanceStats.bitrateBps / 1_000_000).toFixed(2)} Mbit/s`}
                  </dd>
                  <dt className="text-muted-foreground">RTT</dt>
                  <dd className="text-right">
                    {performanceStats.roundTripTimeMs === null
                      ? "—"
                      : `${performanceStats.roundTripTimeMs.toFixed(0)} ms`}
                  </dd>
                  <dt className="text-muted-foreground">Jitter</dt>
                  <dd className="text-right">
                    {performanceStats.jitterMs === null
                      ? "—"
                      : `${performanceStats.jitterMs.toFixed(1)} ms`}
                  </dd>
                  <dt className="text-muted-foreground">Jitter buffer recent</dt>
                  <dd className="text-right">
                    {performanceStats.jitterBufferMs === null
                      ? "—"
                      : `${performanceStats.jitterBufferMs.toFixed(1)} ms`}
                  </dd>
                  <dt className="text-muted-foreground">Jitter target</dt>
                  <dd className="text-right">
                    {performanceStats.jitterBufferTargetMs === null
                      ? "—"
                      : `${performanceStats.jitterBufferTargetMs.toFixed(1)} ms`}
                  </dd>
                  <dt className="text-muted-foreground">Decode per frame</dt>
                  <dd className="text-right">
                    {performanceStats.decodeMsPerFrame === null
                      ? "—"
                      : `${performanceStats.decodeMsPerFrame.toFixed(2)} ms`}
                  </dd>
                  <dt className="text-muted-foreground">Packets lost</dt>
                  <dd className="text-right">{performanceStats.packetsLost}</dd>
                  <dt className="text-muted-foreground">Frames dropped</dt>
                  <dd className="text-right">{performanceStats.framesDropped}</dd>
                  <dt className="text-muted-foreground">Freezes</dt>
                  <dd className="text-right">{performanceStats.freezeCount}</dd>
                  <dt className="text-muted-foreground">Frames decoded</dt>
                  <dd className="text-right">{performanceStats.framesDecoded}</dd>
                  <dt className="text-muted-foreground">Input backlog</dt>
                  <dd className="text-right">{performanceStats.inputBufferedBytes} B</dd>
                </dl>
              ) : (
                <span className="text-muted-foreground">Waiting for stats</span>
              )}
            </CardContent>
          </Card>
        </div>
      ) : null}
    </main>
  );
}
