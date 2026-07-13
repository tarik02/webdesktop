import { z } from "zod";
import {
  controlResponseSchema,
  clipboardMessageSchema,
  inputResponseSchema,
  maximumClipboardBytes,
  signalResponseSchema,
  type ClientLogMessage,
  type ClipboardFormat,
  type ClientTraceLevel,
  type ControlResponse,
  type InputMessage,
  type Quality,
  type QualityPatch,
  type ServerConfig,
} from "#/lib/protocol.ts";

const maxPointerBufferedAmount = 16 * 1024;
const maxTraceQueueSize = 64;
const traceStatsInterval = 5000;

const inboundVideoStatsSchema = z
  .object({
    type: z.literal("inbound-rtp"),
    kind: z.literal("video").optional(),
    mediaType: z.literal("video").optional(),
    timestamp: z.number(),
    bytesReceived: z.number(),
    packetsLost: z.number().optional(),
    framesPerSecond: z.number().optional(),
    framesDecoded: z.number().optional(),
    frameWidth: z.number().optional(),
    frameHeight: z.number().optional(),
    jitter: z.number().optional(),
    jitterBufferDelay: z.number().optional(),
    jitterBufferEmittedCount: z.number().optional(),
    jitterBufferTargetDelay: z.number().optional(),
    jitterBufferMinimumDelay: z.number().optional(),
    framesDropped: z.number().optional(),
    freezeCount: z.number().optional(),
    totalFreezesDuration: z.number().optional(),
    totalDecodeTime: z.number().optional(),
    totalProcessingDelay: z.number().optional(),
  })
  .refine((stats) => stats.kind === "video" || stats.mediaType === "video");

type PerformanceSeries = "ui" | "trace";

const candidatePairStatsSchema = z.object({
  type: z.literal("candidate-pair"),
  state: z.literal("succeeded"),
  nominated: z.boolean().optional(),
  currentRoundTripTime: z.number().optional(),
});

export type ConnectionState =
  | { phase: "idle" }
  | { phase: "connecting"; detail: string }
  | { phase: "connected" }
  | { phase: "error"; message: string };

export type PerformanceStats = {
  bitrateBps: number | null;
  framesPerSecond: number | null;
  framesDecoded: number;
  width: number | null;
  height: number | null;
  packetsLost: number;
  jitterMs: number | null;
  jitterBufferMs: number | null;
  jitterBufferLifetimeMs: number | null;
  jitterBufferTargetMs: number | null;
  jitterBufferMinimumMs: number | null;
  decodeMsPerFrame: number | null;
  processingDelayMsPerFrame: number | null;
  framesDropped: number;
  intervalFramesDropped: number | null;
  freezeCount: number;
  intervalFreezeCount: number | null;
  intervalFreezeDurationMs: number | null;
  roundTripTimeMs: number | null;
  inputBufferedBytes: number;
};

type ConnectionCallbacks = {
  onState: (state: ConnectionState) => void;
  onStream: (stream: MediaStream) => void;
  onQuality: (quality: Quality) => void;
  onInputLease: (owned: boolean) => void;
  onInputError: (message: string) => void;
  onClipboard: (formats: ClipboardFormat[]) => Promise<void>;
  onClipboardError: (message: string) => void;
};

type PendingControl = {
  resolve: (response: ControlResponse) => void;
  reject: (error: Error) => void;
  timeout: number;
};

type PendingClipboard = {
  resolve: () => void;
  reject: (error: Error) => void;
  timeout: number;
};

type ClipboardReceive = {
  formats: Array<{ mimeType: ClipboardFormat["mimeType"]; data: Uint8Array<ArrayBuffer> }>;
  sizes: number[];
  index: number;
  written: number;
};

export class ProtocolError extends Error {
  constructor(
    readonly code: string,
    message: string,
  ) {
    super(message);
  }
}

export class DesktopConnection {
  private pc: RTCPeerConnection | null = null;
  private socket: WebSocket | null = null;
  private control: RTCDataChannel | null = null;
  private input: RTCDataChannel | null = null;
  private clipboard: RTCDataChannel | null = null;
  private pendingCandidates: RTCIceCandidateInit[] = [];
  private pendingControl = new Map<string, PendingControl>();
  private pendingClipboard = new Map<string, PendingClipboard>();
  private clipboardReceive: ClipboardReceive | null = null;
  private clipboardSend = Promise.resolve();
  private clipboardApply = Promise.resolve();
  private pressedKeys = new Set<number>();
  private pressedButtons = new Set<"primary" | "middle" | "secondary" | "back" | "forward">();
  private nextRequestID = 1;
  private nextSequence = 1;
  private leaseOwned = false;
  private closing = false;
  private reportedConnected = false;
  private previousVideoStats: Record<
    PerformanceSeries,
    z.infer<typeof inboundVideoStatsSchema> | null
  > = {
    ui: null,
    trace: null,
  };
  private traceQueue: ClientLogMessage[] = [];
  private traceStatsTimer: number | null = null;
  private connectStartedAt = 0;
  private offerSentAt = 0;

  constructor(
    private readonly config: ServerConfig,
    private readonly callbacks: ConnectionCallbacks,
  ) {
    this.trace("info", "client.created", {
      codec: config.video.codec,
      audio_enabled: String(config.audio.enabled),
      input_enabled: String(config.input.enabled),
      visibility: document.visibilityState,
    });
  }

  trace(level: ClientTraceLevel, event: string, details: Record<string, string> = {}) {
    if (!this.config.tracing.enabled) {
      return;
    }

    switch (level) {
      case "debug":
        console.debug("[webdesktop trace]", event, details);
        break;
      case "info":
        console.info("[webdesktop trace]", event, details);
        break;
      case "warn":
        console.warn("[webdesktop trace]", event, details);
        break;
      case "error":
        console.error("[webdesktop trace]", event, details);
        break;
      default: {
        const exhaustive: never = level;
        return exhaustive;
      }
    }

    const boundedDetails = Object.fromEntries(
      Object.entries(details)
        .slice(0, 32)
        .map(([key, value]) => [key.slice(0, 64), value.slice(0, 512)]),
    );
    const message: ClientLogMessage = {
      version: 1,
      type: "client-log",
      level,
      event,
      details: boundedDetails,
    };
    if (this.socket?.readyState === WebSocket.OPEN) {
      try {
        this.socket.send(JSON.stringify(message));
      } catch (error) {
        console.error(
          "[webdesktop trace] client-log.send-failed",
          error instanceof Error ? error.message : "WebSocket send failed",
        );
      }
      return;
    }
    if (this.traceQueue.length === maxTraceQueueSize) {
      this.traceQueue.shift();
    }
    this.traceQueue.push(message);
  }

  async connect() {
    if (this.pc || this.socket) {
      throw new Error("connection is already active");
    }

    this.closing = false;
    this.connectStartedAt = performance.now();
    this.trace("info", "connect.start", {
      codec: this.config.video.codec,
      audio_enabled: String(this.config.audio.enabled),
    });
    this.callbacks.onState({ phase: "connecting", detail: "opening signaling" });

    const pc = new RTCPeerConnection();
    this.pc = pc;

    const videoTransceiver = pc.addTransceiver("video", { direction: "recvonly" });
    const capabilities = RTCRtpReceiver.getCapabilities("video");
    if (!capabilities) {
      throw new Error("browser did not report video codec capabilities");
    }
    const preferredCodecs = capabilities.codecs.filter((codec) => {
      if (this.config.video.codec === "vp8") {
        return codec.mimeType.toLowerCase() === "video/vp8";
      }
      return (
        codec.mimeType.toLowerCase() === "video/h264" &&
        codec.sdpFmtpLine?.includes("packetization-mode=1") === true &&
        codec.sdpFmtpLine.includes("profile-level-id=42e0")
      );
    });
    if (preferredCodecs.length === 0) {
      throw new Error(`browser does not support ${this.config.video.codec.toUpperCase()}`);
    }
    videoTransceiver.setCodecPreferences(preferredCodecs);

    if (this.config.audio.enabled) {
      pc.addTransceiver("audio", { direction: "recvonly" });
    }

    const control = pc.createDataChannel("control", { ordered: true });
    const input = pc.createDataChannel("input", { ordered: true });
    const clipboard = this.config.clipboard.enabled
      ? pc.createDataChannel("clipboard", { ordered: true })
      : null;
    this.control = control;
    this.input = input;
    this.clipboard = clipboard;
    if (clipboard) {
      clipboard.binaryType = "arraybuffer";
      clipboard.bufferedAmountLowThreshold = 256 * 1024;
    }

    control.addEventListener("open", () => {
      this.trace("info", "data-channel.open", { channel: "control" });
      this.updateConnected();
    });
    control.addEventListener("close", () => {
      this.trace("warn", "data-channel.close", { channel: "control" });
      this.leaseOwned = false;
      this.callbacks.onInputLease(false);
      if (!this.closing) {
        this.fail(new Error("control data channel closed"));
      }
    });
    control.addEventListener("error", () => {
      this.trace("error", "data-channel.error", { channel: "control" });
      if (!this.closing) {
        this.fail(new Error("control data channel failed"));
      }
    });
    control.addEventListener("message", (event) => {
      try {
        const text = z.string().parse(event.data);
        const response = controlResponseSchema.parse(JSON.parse(text));
        this.trace(response.type === "error" ? "warn" : "debug", "control.response", {
          request_id: response.id,
          response_type: response.type,
          ok: String(response.ok),
          ...(response.type === "error" ? { error_code: response.error.code } : {}),
        });
        const pending = this.pendingControl.get(response.id);
        if (!pending) {
          throw new Error(`unexpected control response ${response.id}`);
        }
        window.clearTimeout(pending.timeout);
        this.pendingControl.delete(response.id);
        pending.resolve(response);
      } catch (error) {
        this.fail(error);
      }
    });

    input.addEventListener("open", () => {
      this.trace("info", "data-channel.open", { channel: "input" });
      this.updateConnected();
    });
    input.addEventListener("close", () => {
      this.trace("warn", "data-channel.close", { channel: "input" });
      this.leaseOwned = false;
      this.callbacks.onInputLease(false);
    });
    input.addEventListener("error", () => {
      this.trace("error", "data-channel.error", { channel: "input" });
      this.leaseOwned = false;
      this.callbacks.onInputLease(false);
    });
    input.addEventListener("message", (event) => {
      try {
        const text = z.string().parse(event.data);
        const response = inputResponseSchema.parse(JSON.parse(text));
        this.trace("warn", "input.response-error", {
          error_code: response.error.code,
          message: response.error.message,
          has_sequence: String(response.sequence !== undefined),
        });
        this.callbacks.onInputError(`${response.error.code}: ${response.error.message}`);
        if (
          response.error.code === "input_overloaded" ||
          response.error.code === "input_unavailable"
        ) {
          this.leaseOwned = false;
          this.callbacks.onInputLease(false);
        }
      } catch (error) {
        this.fail(error);
      }
    });

    clipboard?.addEventListener("open", () => {
      this.trace("info", "data-channel.open", { channel: "clipboard" });
      this.updateConnected();
    });
    clipboard?.addEventListener("close", () => {
      this.trace("warn", "data-channel.close", { channel: "clipboard" });
      if (!this.closing) {
        this.fail(new Error("clipboard data channel closed"));
      }
    });
    clipboard?.addEventListener("error", () => {
      this.trace("error", "data-channel.error", { channel: "clipboard" });
      if (!this.closing) {
        this.fail(new Error("clipboard data channel failed"));
      }
    });
    clipboard?.addEventListener("message", (event) => {
      void this.handleClipboardMessage(event.data).catch((error) => this.fail(error));
    });

    pc.addEventListener("track", (event) => {
      this.trace("info", "track.received", {
        kind: event.track.kind,
        muted: String(event.track.muted),
        ready_state: event.track.readyState,
      });
      event.track.addEventListener("mute", () => {
        this.trace("warn", "track.muted", { kind: event.track.kind });
      });
      event.track.addEventListener("unmute", () => {
        this.trace("info", "track.unmuted", { kind: event.track.kind });
      });
      event.track.addEventListener("ended", () => {
        this.trace("warn", "track.ended", { kind: event.track.kind });
      });
      const stream = event.streams[0] ?? new MediaStream([event.track]);
      this.callbacks.onStream(stream);
    });
    pc.addEventListener("connectionstatechange", () => {
      this.trace(
        pc.connectionState === "failed" || pc.connectionState === "closed" ? "error" : "info",
        "peer-connection.state",
        { state: pc.connectionState },
      );
      switch (pc.connectionState) {
        case "connected":
          this.updateConnected();
          break;
        case "failed":
        case "closed":
          if (!this.closing) {
            this.fail(new Error(`WebRTC connection ${pc.connectionState}`));
          }
          break;
        case "disconnected":
          this.reportedConnected = false;
          this.callbacks.onState({
            phase: "connecting",
            detail: "WebRTC connection interrupted",
          });
          break;
        case "new":
        case "connecting":
          break;
        default: {
          const exhaustive: never = pc.connectionState;
          return exhaustive;
        }
      }
    });
    pc.addEventListener("iceconnectionstatechange", () => {
      this.trace(
        pc.iceConnectionState === "failed" || pc.iceConnectionState === "closed"
          ? "error"
          : "debug",
        "ice-connection.state",
        { state: pc.iceConnectionState },
      );
    });
    pc.addEventListener("icegatheringstatechange", () => {
      this.trace("debug", "ice-gathering.state", { state: pc.iceGatheringState });
    });
    pc.addEventListener("signalingstatechange", () => {
      this.trace("debug", "signaling.state", { state: pc.signalingState });
    });

    const scheme = window.location.protocol === "https:" ? "wss:" : "ws:";
    const socket = new WebSocket(`${scheme}//${window.location.host}${this.config.signaling_path}`);
    this.socket = socket;

    socket.addEventListener("close", (event) => {
      this.trace("warn", "signaling.close", {
        code: String(event.code),
        clean: String(event.wasClean),
      });
      if (!this.closing) {
        this.fail(new Error("signaling connection closed"));
      }
    });
    socket.addEventListener("error", () => {
      this.trace("error", "signaling.error");
      if (!this.closing) {
        this.fail(new Error("signaling connection failed"));
      }
    });
    socket.addEventListener("message", (event) => {
      void this.handleSignal(event.data).catch((error) => this.fail(error));
    });

    await new Promise<void>((resolve, reject) => {
      const opened = () => {
        socket.removeEventListener("error", failed);
        this.flushTraceQueue();
        this.trace("info", "signaling.open", {
          elapsed_ms: (performance.now() - this.connectStartedAt).toFixed(1),
        });
        resolve();
      };
      const failed = () => {
        socket.removeEventListener("open", opened);
        reject(new Error("signaling connection failed"));
      };
      socket.addEventListener("open", opened, { once: true });
      socket.addEventListener("error", failed, { once: true });
    });

    pc.addEventListener("icecandidate", (event) => {
      if (!event.candidate || socket.readyState !== WebSocket.OPEN) {
        if (!event.candidate) {
          this.trace("debug", "ice.local-complete");
        }
        return;
      }
      this.trace("debug", "ice.local-candidate", {
        type: event.candidate.type ?? "unknown",
        protocol: event.candidate.protocol ?? "unknown",
      });
      socket.send(
        JSON.stringify({
          version: 1,
          type: "ice-candidate",
          candidate: event.candidate.toJSON(),
        }),
      );
    });

    this.callbacks.onState({ phase: "connecting", detail: "creating WebRTC offer" });
    const offerStartedAt = performance.now();
    const offer = await pc.createOffer();
    await pc.setLocalDescription(offer);
    const localDescription = pc.localDescription;
    if (!localDescription) {
      throw new Error("local WebRTC offer is unavailable");
    }
    this.offerSentAt = performance.now();
    this.trace("info", "offer.created", {
      sdp_chars: String(localDescription.sdp.length),
      create_ms: (this.offerSentAt - offerStartedAt).toFixed(1),
      elapsed_ms: (this.offerSentAt - this.connectStartedAt).toFixed(1),
    });
    socket.send(
      JSON.stringify({
        version: 1,
        type: "offer",
        sdp: localDescription.sdp,
      }),
    );
    this.callbacks.onState({ phase: "connecting", detail: "waiting for WebRTC answer" });
  }

  async disconnect() {
    this.trace("info", "disconnect.start", {
      lease_owned: String(this.leaseOwned),
    });
    this.closing = true;
    try {
      await this.releaseInput();
    } finally {
      this.trace("info", "disconnect.complete");
      this.closeTransports();
      this.callbacks.onState({ phase: "idle" });
    }
  }

  async acquireInput() {
    this.trace("info", "input.acquire.start");
    let response: ControlResponse;
    try {
      response = await this.requestControl({
        version: 1,
        id: this.requestID("input"),
        type: "input.acquire",
      });
    } catch (error) {
      this.trace("error", "input.acquire.failed", {
        message: error instanceof Error ? error.message : "input acquire failed",
      });
      throw error;
    }
    if (response.type === "error") {
      this.trace("warn", "input.acquire.rejected", {
        error_code: response.error.code,
        message: response.error.message,
      });
      throw new ProtocolError(response.error.code, response.error.message);
    }
    if (response.type !== "input.acquire.result") {
      throw new Error(`unexpected control response ${response.type}`);
    }
    this.leaseOwned = true;
    this.callbacks.onInputLease(true);
    this.trace("info", "input.acquire.complete", {
      pointer: String(response.input.pointer),
      keyboard: String(response.input.keyboard),
    });
    return response.input;
  }

  async releaseInput() {
    const pressedKeys = this.pressedKeys.size;
    const pressedButtons = this.pressedButtons.size;
    this.releasePressed();
    if (!this.leaseOwned) {
      if (pressedKeys > 0 || pressedButtons > 0) {
        this.trace("debug", "input.pressed-cleanup", {
          keys: String(pressedKeys),
          buttons: String(pressedButtons),
          lease_owned: "false",
        });
      }
      return;
    }
    this.trace("info", "input.release.start", {
      pressed_keys: String(pressedKeys),
      pressed_buttons: String(pressedButtons),
    });
    this.leaseOwned = false;
    this.callbacks.onInputLease(false);
    if (this.control?.readyState !== "open") {
      this.trace("warn", "input.release.transport-unavailable");
      return;
    }
    const response = await this.requestControl({
      version: 1,
      id: this.requestID("input"),
      type: "input.release",
    });
    if (response.type === "error" && response.error.code !== "input_not_owned") {
      this.trace("warn", "input.release.rejected", {
        error_code: response.error.code,
        message: response.error.message,
      });
      throw new ProtocolError(response.error.code, response.error.message);
    }
    this.trace("info", "input.release.complete");
  }

  async setQuality(quality: QualityPatch) {
    this.trace("info", "quality.set.start", {
      codec: quality.codec ?? this.config.video.codec,
      width: quality.width === undefined ? "unchanged" : String(quality.width),
      height: quality.height === undefined ? "unchanged" : String(quality.height),
      framerate: quality.framerate === undefined ? "unchanged" : String(quality.framerate),
      bitrate_kbps: quality.bitrate_kbps === undefined ? "unchanged" : String(quality.bitrate_kbps),
    });
    let response: ControlResponse;
    try {
      response = await this.requestControl({
        version: 1,
        id: this.requestID("quality"),
        type: "video.quality.set",
        quality,
      });
    } catch (error) {
      this.trace("error", "quality.set.failed", {
        message: error instanceof Error ? error.message : "quality request failed",
      });
      throw error;
    }
    if (response.type === "error") {
      this.trace("warn", "quality.set.rejected", {
        error_code: response.error.code,
        message: response.error.message,
      });
      throw new ProtocolError(response.error.code, response.error.message);
    }
    if (response.type !== "video.quality.set.result") {
      throw new Error(`unexpected control response ${response.type}`);
    }
    this.callbacks.onQuality(response.quality);
    this.trace("info", "quality.set.complete", {
      codec: response.quality.codec,
      width: String(response.quality.width),
      height: String(response.quality.height),
      framerate: String(response.quality.framerate),
      bitrate_kbps: String(response.quality.bitrate_kbps),
    });
    return response.quality;
  }

  async getPerformanceStats(series: PerformanceSeries = "ui"): Promise<PerformanceStats | null> {
    const pc = this.pc;
    if (!pc) {
      return null;
    }

    const report = await pc.getStats();
    let inbound: z.infer<typeof inboundVideoStatsSchema> | null = null;
    let candidatePair: z.infer<typeof candidatePairStatsSchema> | null = null;
    for (const entry of report.values()) {
      const inboundResult = inboundVideoStatsSchema.safeParse(entry);
      if (inboundResult.success) {
        inbound = inboundResult.data;
      }
      const candidatePairResult = candidatePairStatsSchema.safeParse(entry);
      if (
        candidatePairResult.success &&
        (candidatePairResult.data.nominated || candidatePair === null)
      ) {
        candidatePair = candidatePairResult.data;
      }
    }
    if (!inbound) {
      return null;
    }

    const previous = this.previousVideoStats[series];
    const bitrateBps =
      previous &&
      inbound.timestamp > previous.timestamp &&
      inbound.bytesReceived >= previous.bytesReceived
        ? ((inbound.bytesReceived - previous.bytesReceived) * 8000) /
          (inbound.timestamp - previous.timestamp)
        : null;
    const emittedCountDelta =
      previous &&
      inbound.jitterBufferEmittedCount !== undefined &&
      previous.jitterBufferEmittedCount !== undefined &&
      inbound.jitterBufferEmittedCount > previous.jitterBufferEmittedCount
        ? inbound.jitterBufferEmittedCount - previous.jitterBufferEmittedCount
        : null;
    const framesDecodedDelta =
      previous &&
      inbound.framesDecoded !== undefined &&
      previous.framesDecoded !== undefined &&
      inbound.framesDecoded > previous.framesDecoded
        ? inbound.framesDecoded - previous.framesDecoded
        : null;
    const jitterBufferMs =
      emittedCountDelta !== null &&
      inbound.jitterBufferDelay !== undefined &&
      previous?.jitterBufferDelay !== undefined &&
      inbound.jitterBufferDelay >= previous.jitterBufferDelay
        ? ((inbound.jitterBufferDelay - previous.jitterBufferDelay) / emittedCountDelta) * 1000
        : null;
    const jitterBufferTargetMs =
      emittedCountDelta !== null &&
      inbound.jitterBufferTargetDelay !== undefined &&
      previous?.jitterBufferTargetDelay !== undefined &&
      inbound.jitterBufferTargetDelay >= previous.jitterBufferTargetDelay
        ? ((inbound.jitterBufferTargetDelay - previous.jitterBufferTargetDelay) /
            emittedCountDelta) *
          1000
        : null;
    const jitterBufferMinimumMs =
      emittedCountDelta !== null &&
      inbound.jitterBufferMinimumDelay !== undefined &&
      previous?.jitterBufferMinimumDelay !== undefined &&
      inbound.jitterBufferMinimumDelay >= previous.jitterBufferMinimumDelay
        ? ((inbound.jitterBufferMinimumDelay - previous.jitterBufferMinimumDelay) /
            emittedCountDelta) *
          1000
        : null;
    const decodeMsPerFrame =
      framesDecodedDelta !== null &&
      inbound.totalDecodeTime !== undefined &&
      previous?.totalDecodeTime !== undefined &&
      inbound.totalDecodeTime >= previous.totalDecodeTime
        ? ((inbound.totalDecodeTime - previous.totalDecodeTime) / framesDecodedDelta) * 1000
        : null;
    const processingDelayMsPerFrame =
      framesDecodedDelta !== null &&
      inbound.totalProcessingDelay !== undefined &&
      previous?.totalProcessingDelay !== undefined &&
      inbound.totalProcessingDelay >= previous.totalProcessingDelay
        ? ((inbound.totalProcessingDelay - previous.totalProcessingDelay) / framesDecodedDelta) *
          1000
        : null;
    const intervalFramesDropped =
      previous &&
      inbound.framesDropped !== undefined &&
      previous.framesDropped !== undefined &&
      inbound.framesDropped >= previous.framesDropped
        ? inbound.framesDropped - previous.framesDropped
        : null;
    const intervalFreezeCount =
      previous &&
      inbound.freezeCount !== undefined &&
      previous.freezeCount !== undefined &&
      inbound.freezeCount >= previous.freezeCount
        ? inbound.freezeCount - previous.freezeCount
        : null;
    const intervalFreezeDurationMs =
      previous &&
      inbound.totalFreezesDuration !== undefined &&
      previous.totalFreezesDuration !== undefined &&
      inbound.totalFreezesDuration >= previous.totalFreezesDuration
        ? (inbound.totalFreezesDuration - previous.totalFreezesDuration) * 1000
        : null;
    this.previousVideoStats[series] = inbound;

    return {
      bitrateBps,
      framesPerSecond: inbound.framesPerSecond ?? null,
      framesDecoded: inbound.framesDecoded ?? 0,
      width: inbound.frameWidth ?? null,
      height: inbound.frameHeight ?? null,
      packetsLost: inbound.packetsLost ?? 0,
      jitterMs: inbound.jitter === undefined ? null : inbound.jitter * 1000,
      jitterBufferMs,
      jitterBufferLifetimeMs:
        inbound.jitterBufferDelay === undefined ||
        inbound.jitterBufferEmittedCount === undefined ||
        inbound.jitterBufferEmittedCount === 0
          ? null
          : (inbound.jitterBufferDelay / inbound.jitterBufferEmittedCount) * 1000,
      jitterBufferTargetMs,
      jitterBufferMinimumMs,
      decodeMsPerFrame,
      processingDelayMsPerFrame,
      framesDropped: inbound.framesDropped ?? 0,
      intervalFramesDropped,
      freezeCount: inbound.freezeCount ?? 0,
      intervalFreezeCount,
      intervalFreezeDurationMs,
      roundTripTimeMs:
        candidatePair?.currentRoundTripTime === undefined
          ? null
          : candidatePair.currentRoundTripTime * 1000,
      inputBufferedBytes: this.input?.bufferedAmount ?? 0,
    };
  }

  pointerAbsolute(x: number, y: number) {
    if ((this.input?.bufferedAmount ?? 0) > maxPointerBufferedAmount) {
      return;
    }
    this.sendInput({
      version: 1,
      sequence: this.nextInputSequence(),
      type: "input.pointer.motion.absolute",
      x,
      y,
    });
  }

  pointerButton(button: "primary" | "middle" | "secondary" | "back" | "forward", pressed: boolean) {
    if (pressed) {
      if (this.pressedButtons.has(button)) {
        return;
      }
      this.pressedButtons.add(button);
    } else if (!this.pressedButtons.delete(button)) {
      return;
    }
    this.sendInput({
      version: 1,
      sequence: this.nextInputSequence(),
      type: "input.pointer.button",
      button,
      pressed,
    });
  }

  pointerScroll(
    horizontal: number,
    vertical: number,
    stopHorizontal: boolean,
    stopVertical: boolean,
  ) {
    this.sendInput({
      version: 1,
      sequence: this.nextInputSequence(),
      type: "input.pointer.scroll",
      horizontal,
      vertical,
      stop_horizontal: stopHorizontal,
      stop_vertical: stopVertical,
    });
  }

  keyboardKey(keycode: number, pressed: boolean) {
    if (pressed) {
      if (this.pressedKeys.has(keycode)) {
        return;
      }
      this.pressedKeys.add(keycode);
    } else if (!this.pressedKeys.delete(keycode)) {
      return;
    }
    this.sendInput({
      version: 1,
      sequence: this.nextInputSequence(),
      type: "input.keyboard.key",
      keycode,
      pressed,
    });
  }

  pasteClipboard(formats: ClipboardFormat[]) {
    const transfer = this.clipboardSend
      .catch(() => undefined)
      .then(async () => {
        const channel = this.clipboard;
        if (!channel || channel.readyState !== "open") {
          throw new Error("clipboard data channel is not open");
        }
        if (formats.length === 0) {
          throw new Error("clipboard has no supported content");
        }
        if (formats.length > 8) {
          throw new Error("clipboard has too many formats");
        }
        const totalBytes = formats.reduce((total, format) => total + format.data.byteLength, 0);
        if (totalBytes > maximumClipboardBytes) {
          throw new Error("clipboard content exceeds 32 MiB");
        }

        const id = this.requestID("clipboard");
        const completion = new Promise<void>((resolve, reject) => {
          const timeout = window.setTimeout(() => {
            this.pendingClipboard.delete(id);
            reject(new Error(`clipboard transfer ${id} timed out`));
          }, 15_000);
          this.pendingClipboard.set(id, { resolve, reject, timeout });
        });
        try {
          channel.send(
            JSON.stringify({
              version: 1,
              type: "clipboard.content",
              id,
              formats: formats.map((format) => ({
                mime_type: format.mimeType,
                size: format.data.byteLength,
              })),
            }),
          );
          for (const format of formats) {
            const data = new Uint8Array(format.data);
            for (let offset = 0; offset < data.byteLength; offset += 16 * 1024) {
              if (channel.bufferedAmount > 512 * 1024) {
                await new Promise<void>((resolve, reject) => {
                  const drained = () => {
                    channel.removeEventListener("close", closed);
                    resolve();
                  };
                  const closed = () => {
                    channel.removeEventListener("bufferedamountlow", drained);
                    reject(new Error("clipboard data channel closed"));
                  };
                  channel.addEventListener("bufferedamountlow", drained, { once: true });
                  channel.addEventListener("close", closed, { once: true });
                });
              }
              channel.send(data.slice(offset, offset + 16 * 1024));
            }
          }
          await completion;
        } catch (error) {
          const pending = this.pendingClipboard.get(id);
          if (pending) {
            window.clearTimeout(pending.timeout);
            this.pendingClipboard.delete(id);
          }
          throw error;
        }

        this.releasePressed();
        this.keyboardKey(29, true);
        this.keyboardKey(47, true);
        this.keyboardKey(47, false);
        this.keyboardKey(29, false);
      });
    this.clipboardSend = transfer;
    return transfer;
  }

  releasePressed() {
    for (const keycode of this.pressedKeys) {
      this.sendInput({
        version: 1,
        sequence: this.nextInputSequence(),
        type: "input.keyboard.key",
        keycode,
        pressed: false,
      });
    }
    this.pressedKeys.clear();
    for (const button of this.pressedButtons) {
      this.sendInput({
        version: 1,
        sequence: this.nextInputSequence(),
        type: "input.pointer.button",
        button,
        pressed: false,
      });
    }
    this.pressedButtons.clear();
  }

  disposeImmediately() {
    this.trace("info", "client.dispose", {
      lease_owned: String(this.leaseOwned),
      pressed_keys: String(this.pressedKeys.size),
      pressed_buttons: String(this.pressedButtons.size),
    });
    this.closing = true;
    this.releasePressed();
    if (this.leaseOwned && this.control?.readyState === "open") {
      this.control.send(
        JSON.stringify({
          version: 1,
          id: this.requestID("input"),
          type: "input.release",
        }),
      );
    }
    this.leaseOwned = false;
    this.closeTransports();
  }

  private async handleClipboardMessage(data: string | ArrayBuffer) {
    if (typeof data === "string") {
      const message = clipboardMessageSchema.parse(JSON.parse(data));
      if (message.type === "clipboard.content.result") {
        const pending = this.pendingClipboard.get(message.id);
        if (!pending) {
          throw new Error(`unexpected clipboard response ${message.id}`);
        }
        window.clearTimeout(pending.timeout);
        this.pendingClipboard.delete(message.id);
        pending.resolve();
        return;
      }
      if (message.type === "error") {
        const error = new ProtocolError(message.error.code, message.error.message);
        const pending = this.pendingClipboard.get(message.id);
        if (pending) {
          window.clearTimeout(pending.timeout);
          this.pendingClipboard.delete(message.id);
          pending.reject(error);
        } else {
          this.callbacks.onClipboardError(`${error.code}: ${error.message}`);
        }
        return;
      }
      if (this.clipboardReceive) {
        throw new Error("another desktop clipboard transfer is in progress");
      }
      this.clipboardReceive = {
        formats: message.formats.map((format) => ({
          mimeType: format.mime_type,
          data: new Uint8Array(new ArrayBuffer(format.size)),
        })),
        sizes: message.formats.map((format) => format.size),
        index: 0,
        written: 0,
      };
      while (
        this.clipboardReceive.index < this.clipboardReceive.sizes.length &&
        this.clipboardReceive.sizes[this.clipboardReceive.index] === 0
      ) {
        this.clipboardReceive.index += 1;
      }
      if (this.clipboardReceive.index === this.clipboardReceive.formats.length) {
        const receive = this.clipboardReceive;
        this.clipboardReceive = null;
        this.applyClipboard(receive.formats);
      }
      return;
    }

    const receive = this.clipboardReceive;
    if (!receive) {
      throw new Error("clipboard data arrived without a header");
    }
    let offset = 0;
    const chunk = new Uint8Array(data);
    while (offset < chunk.byteLength && receive.index < receive.formats.length) {
      const available = receive.sizes[receive.index] - receive.written;
      const copied = Math.min(available, chunk.byteLength - offset);
      receive.formats[receive.index].data.set(
        chunk.subarray(offset, offset + copied),
        receive.written,
      );
      receive.written += copied;
      offset += copied;
      if (receive.written === receive.sizes[receive.index]) {
        receive.index += 1;
        receive.written = 0;
        while (receive.index < receive.sizes.length && receive.sizes[receive.index] === 0) {
          receive.index += 1;
        }
      }
    }
    if (offset !== chunk.byteLength) {
      this.clipboardReceive = null;
      throw new Error("clipboard payload exceeds declared sizes");
    }
    if (receive.index === receive.formats.length) {
      this.clipboardReceive = null;
      this.applyClipboard(receive.formats);
    }
  }

  private applyClipboard(
    formats: Array<{ mimeType: ClipboardFormat["mimeType"]; data: Uint8Array<ArrayBuffer> }>,
  ) {
    this.clipboardApply = this.clipboardApply
      .catch(() => undefined)
      .then(() =>
        this.callbacks.onClipboard(
          formats.map((format) => ({
            mimeType: format.mimeType,
            data: format.data.buffer,
          })),
        ),
      )
      .catch((error) => {
        this.callbacks.onClipboardError(
          error instanceof Error ? error.message : "could not write the browser clipboard",
        );
      });
  }

  private async handleSignal(data: unknown) {
    const text = z.string().parse(data);
    const response = signalResponseSchema.parse(JSON.parse(text));
    if (response.type === "error") {
      this.trace("error", "signaling.protocol-error", {
        error_code: response.error.code,
        message: response.error.message,
      });
      throw new ProtocolError(response.error.code, response.error.message);
    }
    const pc = this.pc;
    if (!pc) {
      throw new Error("WebRTC connection is unavailable");
    }
    if (response.type === "answer") {
      this.trace("info", "answer.received", {
        sdp_chars: String(response.sdp.length),
        answer_wait_ms:
          this.offerSentAt === 0 ? "unknown" : (performance.now() - this.offerSentAt).toFixed(1),
      });
      await pc.setRemoteDescription({ type: "answer", sdp: response.sdp });
      for (const candidate of this.pendingCandidates) {
        await pc.addIceCandidate(candidate);
      }
      this.pendingCandidates = [];
      this.callbacks.onState({ phase: "connecting", detail: "establishing media" });
      return;
    }
    const candidate = new RTCIceCandidate(response.candidate);
    this.trace("debug", "ice.remote-candidate", {
      type: candidate.type ?? "unknown",
      protocol: candidate.protocol ?? "unknown",
    });
    if (!pc.remoteDescription) {
      this.pendingCandidates.push(response.candidate);
      return;
    }
    await pc.addIceCandidate(response.candidate);
  }

  private requestControl(message: {
    version: 1;
    id: string;
    type: "input.acquire" | "input.release" | "video.quality.set";
    quality?: QualityPatch;
  }) {
    const channel = this.control;
    if (!channel || channel.readyState !== "open") {
      return Promise.reject(new Error("control data channel is not open"));
    }
    return new Promise<ControlResponse>((resolve, reject) => {
      const timeout = window.setTimeout(() => {
        this.pendingControl.delete(message.id);
        this.trace("error", "control.timeout", {
          request_id: message.id,
          request_type: message.type,
        });
        reject(new Error(`control request ${message.id} timed out`));
      }, 5000);
      this.pendingControl.set(message.id, { resolve, reject, timeout });
      this.trace("debug", "control.request", {
        request_id: message.id,
        request_type: message.type,
      });
      channel.send(JSON.stringify(message));
    });
  }

  private sendInput(message: InputMessage) {
    if (!this.leaseOwned) {
      return;
    }
    const channel = this.input;
    if (!channel || channel.readyState !== "open") {
      return;
    }
    channel.send(JSON.stringify(message));
  }

  private updateConnected() {
    if (
      this.pc?.connectionState === "connected" &&
      this.control?.readyState === "open" &&
      this.input?.readyState === "open" &&
      (!this.config.clipboard.enabled || this.clipboard?.readyState === "open")
    ) {
      if (this.reportedConnected) {
        return;
      }
      this.reportedConnected = true;
      this.trace("info", "connect.ready", {
        elapsed_ms: (performance.now() - this.connectStartedAt).toFixed(1),
      });
      if (this.config.tracing.enabled && this.traceStatsTimer === null) {
        void this.tracePerformanceSnapshot();
        this.traceStatsTimer = window.setInterval(() => {
          void this.tracePerformanceSnapshot();
        }, traceStatsInterval);
      }
      this.callbacks.onState({ phase: "connected" });
    }
  }

  private fail(error: unknown) {
    if (this.closing) {
      return;
    }
    this.trace("error", "connect.failed", {
      message: error instanceof Error ? error.message : "connection failed",
      elapsed_ms:
        this.connectStartedAt === 0
          ? "unknown"
          : (performance.now() - this.connectStartedAt).toFixed(1),
    });
    this.closing = true;
    this.closeTransports();
    this.callbacks.onState({
      phase: "error",
      message: error instanceof Error ? error.message : "connection failed",
    });
  }

  private closeTransports() {
    this.trace("debug", "transport.cleanup", {
      pending_control: String(this.pendingControl.size),
      pending_candidates: String(this.pendingCandidates.length),
      pressed_keys: String(this.pressedKeys.size),
      pressed_buttons: String(this.pressedButtons.size),
    });
    if (this.traceStatsTimer !== null) {
      window.clearInterval(this.traceStatsTimer);
      this.traceStatsTimer = null;
    }
    for (const pending of this.pendingControl.values()) {
      window.clearTimeout(pending.timeout);
      pending.reject(new Error("connection closed"));
    }
    this.pendingControl.clear();
    for (const pending of this.pendingClipboard.values()) {
      window.clearTimeout(pending.timeout);
      pending.reject(new Error("connection closed"));
    }
    this.pendingClipboard.clear();
    this.leaseOwned = false;
    this.callbacks.onInputLease(false);
    this.control?.close();
    this.input?.close();
    this.clipboard?.close();
    this.pc?.close();
    this.socket?.close(1000, "client disconnect");
    this.control = null;
    this.input = null;
    this.clipboard = null;
    this.clipboardReceive = null;
    this.clipboardSend = Promise.resolve();
    this.pc = null;
    this.socket = null;
    this.pendingCandidates = [];
    this.pressedKeys.clear();
    this.pressedButtons.clear();
    this.previousVideoStats.ui = null;
    this.previousVideoStats.trace = null;
    this.reportedConnected = false;
  }

  private flushTraceQueue() {
    if (!this.config.tracing.enabled || this.socket?.readyState !== WebSocket.OPEN) {
      return;
    }
    const queued = this.traceQueue;
    this.traceQueue = [];
    for (const message of queued) {
      try {
        this.socket.send(JSON.stringify(message));
      } catch (error) {
        console.error(
          "[webdesktop trace] client-log.flush-failed",
          error instanceof Error ? error.message : "WebSocket send failed",
        );
        break;
      }
    }
  }

  private async tracePerformanceSnapshot() {
    try {
      const stats = await this.getPerformanceStats("trace");
      if (!stats) {
        this.trace("debug", "performance.unavailable");
        return;
      }
      this.trace("debug", "performance.snapshot", {
        bitrate_bps: stats.bitrateBps === null ? "unavailable" : stats.bitrateBps.toFixed(0),
        fps: stats.framesPerSecond === null ? "unavailable" : stats.framesPerSecond.toFixed(1),
        frames_decoded: String(stats.framesDecoded),
        width: stats.width === null ? "unavailable" : String(stats.width),
        height: stats.height === null ? "unavailable" : String(stats.height),
        packets_lost: String(stats.packetsLost),
        jitter_ms: stats.jitterMs === null ? "unavailable" : stats.jitterMs.toFixed(1),
        jitter_buffer_interval_ms:
          stats.jitterBufferMs === null ? "unavailable" : stats.jitterBufferMs.toFixed(1),
        jitter_buffer_lifetime_ms:
          stats.jitterBufferLifetimeMs === null
            ? "unavailable"
            : stats.jitterBufferLifetimeMs.toFixed(1),
        jitter_buffer_target_ms:
          stats.jitterBufferTargetMs === null
            ? "unavailable"
            : stats.jitterBufferTargetMs.toFixed(1),
        jitter_buffer_minimum_ms:
          stats.jitterBufferMinimumMs === null
            ? "unavailable"
            : stats.jitterBufferMinimumMs.toFixed(1),
        decode_ms_per_frame:
          stats.decodeMsPerFrame === null ? "unavailable" : stats.decodeMsPerFrame.toFixed(2),
        processing_delay_ms_per_frame:
          stats.processingDelayMsPerFrame === null
            ? "unavailable"
            : stats.processingDelayMsPerFrame.toFixed(2),
        frames_dropped: String(stats.framesDropped),
        interval_frames_dropped:
          stats.intervalFramesDropped === null
            ? "unavailable"
            : String(stats.intervalFramesDropped),
        freeze_count: String(stats.freezeCount),
        interval_freeze_count:
          stats.intervalFreezeCount === null ? "unavailable" : String(stats.intervalFreezeCount),
        interval_freeze_duration_ms:
          stats.intervalFreezeDurationMs === null
            ? "unavailable"
            : stats.intervalFreezeDurationMs.toFixed(1),
        rtt_ms: stats.roundTripTimeMs === null ? "unavailable" : stats.roundTripTimeMs.toFixed(1),
        input_buffered_bytes: String(stats.inputBufferedBytes),
      });
    } catch (error) {
      this.trace("warn", "performance.failed", {
        message: error instanceof Error ? error.message : "getStats failed",
      });
    }
  }

  private requestID(prefix: string) {
    const id = `${prefix}-${this.nextRequestID}`;
    this.nextRequestID += 1;
    return id;
  }

  private nextInputSequence() {
    const sequence = this.nextSequence;
    this.nextSequence += 1;
    return sequence;
  }
}
