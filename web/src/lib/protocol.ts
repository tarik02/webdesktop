import { z } from "zod";

export const minimumVideoBitrateKbps = 100;
export const maximumVP8BitrateKbps = 2_147_483;
export const maximumH264BitrateKbps = 50_000;
export const maximumClipboardBytes = 32 * 1024 * 1024;

export const clipboardMIMESchema = z.enum([
  "text/plain",
  "text/html",
  "image/png",
  "image/jpeg",
  "image/webp",
  "image/gif",
  "image/svg+xml",
]);

const protocolErrorSchema = z
  .object({
    code: z.string(),
    message: z.string(),
  })
  .strict();

const qualitySchema = z
  .object({
    codec: z.enum(["vp8", "h264"]),
    width: z
      .number()
      .int()
      .min(320)
      .max(7680)
      .refine((width) => width % 2 === 0),
    height: z
      .number()
      .int()
      .min(240)
      .max(4320)
      .refine((height) => height % 2 === 0),
    framerate: z.number().int().min(1).max(120),
    bitrate_kbps: z.number().int().min(minimumVideoBitrateKbps),
  })
  .strict()
  .superRefine((quality, context) => {
    if (quality.codec === "vp8") {
      if (quality.bitrate_kbps > maximumVP8BitrateKbps) {
        context.addIssue({
          code: "custom",
          path: ["bitrate_kbps"],
          message: `VP8 bitrate must not exceed ${maximumVP8BitrateKbps} Kbit/s`,
        });
      }
      return;
    }

    const widthMacroblocks = Math.ceil(quality.width / 16);
    const heightMacroblocks = Math.ceil(quality.height / 16);
    const macroblocks = widthMacroblocks * heightMacroblocks;
    if (widthMacroblocks > 263) {
      context.addIssue({
        code: "custom",
        path: ["width"],
        message: `H.264 Level 4.2 width must not exceed 263 macroblocks; width ${quality.width} requires ${widthMacroblocks}`,
      });
    }
    if (heightMacroblocks > 263) {
      context.addIssue({
        code: "custom",
        path: ["height"],
        message: `H.264 Level 4.2 height must not exceed 263 macroblocks; height ${quality.height} requires ${heightMacroblocks}`,
      });
    }
    if (macroblocks > 8704) {
      context.addIssue({
        code: "custom",
        path: ["width"],
        message: `H.264 Level 4.2 supports at most 8704 macroblocks per frame; ${quality.width}x${quality.height} requires ${macroblocks}`,
      });
    }
    if (macroblocks * quality.framerate > 522_240) {
      context.addIssue({
        code: "custom",
        path: ["framerate"],
        message: `H.264 Level 4.2 supports at most 522240 macroblocks per second; ${quality.width}x${quality.height} at ${quality.framerate} fps requires ${macroblocks * quality.framerate}`,
      });
    }
    if (quality.bitrate_kbps > maximumH264BitrateKbps) {
      context.addIssue({
        code: "custom",
        path: ["bitrate_kbps"],
        message: `H.264 Level 4.2 bitrate must not exceed ${maximumH264BitrateKbps} Kbit/s`,
      });
    }
  });

export const serverConfigSchema = z
  .object({
    version: z.literal(1),
    signaling_path: z.string().startsWith("/"),
    video: qualitySchema,
    audio: z
      .object({
        enabled: z.boolean(),
      })
      .strict(),
    tracing: z
      .object({
        enabled: z.boolean(),
      })
      .strict(),
    input: z
      .object({
        enabled: z.boolean(),
        pointer: z.boolean(),
        keyboard: z.boolean(),
      })
      .strict(),
    clipboard: z
      .object({
        enabled: z.boolean(),
      })
      .strict(),
  })
  .strict();

const iceCandidateSchema = z
  .object({
    candidate: z.string(),
    sdpMid: z.string().nullable().optional(),
    sdpMLineIndex: z.number().int().nullable().optional(),
    usernameFragment: z.string().nullable().optional(),
  })
  .strict();

export const signalResponseSchema = z.discriminatedUnion("type", [
  z
    .object({
      version: z.literal(1),
      type: z.literal("answer"),
      sdp: z.string(),
    })
    .strict(),
  z
    .object({
      version: z.literal(1),
      type: z.literal("ice-candidate"),
      candidate: iceCandidateSchema,
    })
    .strict(),
  z
    .object({
      version: z.literal(1),
      type: z.literal("error"),
      error: protocolErrorSchema,
    })
    .strict(),
]);

export const controlResponseSchema = z.discriminatedUnion("type", [
  z
    .object({
      version: z.literal(1),
      id: z.string(),
      type: z.literal("video.quality.set.result"),
      ok: z.literal(true),
      quality: qualitySchema,
    })
    .strict(),
  z
    .object({
      version: z.literal(1),
      id: z.string(),
      type: z.literal("input.acquire.result"),
      ok: z.literal(true),
      input: z
        .object({
          pointer: z.boolean(),
          keyboard: z.boolean(),
        })
        .strict(),
    })
    .strict(),
  z
    .object({
      version: z.literal(1),
      id: z.string(),
      type: z.literal("input.release.result"),
      ok: z.literal(true),
    })
    .strict(),
  z
    .object({
      version: z.literal(1),
      id: z.string(),
      type: z.literal("error"),
      ok: z.literal(false),
      error: protocolErrorSchema,
    })
    .strict(),
]);

export const clipboardMessageSchema = z.discriminatedUnion("type", [
  z
    .object({
      version: z.literal(1),
      type: z.literal("clipboard.content"),
      id: z.string(),
      formats: z
        .array(
          z
            .object({
              mime_type: clipboardMIMESchema,
              size: z.number().int().nonnegative().max(maximumClipboardBytes),
            })
            .strict(),
        )
        .min(1)
        .max(8)
        .refine(
          (formats) =>
            formats.reduce((total, format) => total + format.size, 0) <= maximumClipboardBytes,
        ),
    })
    .strict(),
  z
    .object({
      version: z.literal(1),
      type: z.literal("clipboard.content.result"),
      id: z.string(),
      ok: z.literal(true),
    })
    .strict(),
  z
    .object({
      version: z.literal(1),
      type: z.literal("error"),
      id: z.string(),
      error: protocolErrorSchema,
    })
    .strict(),
]);

export const inputResponseSchema = z
  .object({
    version: z.literal(1),
    sequence: z.number().int().positive().optional(),
    type: z.literal("error"),
    ok: z.literal(false),
    error: protocolErrorSchema,
  })
  .strict();

export type ServerConfig = z.infer<typeof serverConfigSchema>;
export type Quality = z.infer<typeof qualitySchema>;
export type SignalResponse = z.infer<typeof signalResponseSchema>;
export type ControlResponse = z.infer<typeof controlResponseSchema>;
export type ClipboardMessage = z.infer<typeof clipboardMessageSchema>;
export type ClipboardMIME = z.infer<typeof clipboardMIMESchema>;
export type ClipboardFormat = { mimeType: ClipboardMIME; data: ArrayBuffer };
export type ClientTraceLevel = "debug" | "info" | "warn" | "error";

export type ClientLogMessage = {
  version: 1;
  type: "client-log";
  level: ClientTraceLevel;
  event: string;
  details: Record<string, string>;
};

export type QualityPatch = {
  codec?: "vp8" | "h264";
  width?: number;
  height?: number;
  framerate?: number;
  bitrate_kbps?: number;
};

export type InputMessage =
  | {
      version: 1;
      sequence: number;
      type: "input.pointer.motion.absolute";
      x: number;
      y: number;
    }
  | {
      version: 1;
      sequence: number;
      type: "input.pointer.button";
      button: "primary" | "middle" | "secondary" | "back" | "forward";
      pressed: boolean;
    }
  | {
      version: 1;
      sequence: number;
      type: "input.pointer.scroll";
      horizontal: number;
      vertical: number;
      stop_horizontal: boolean;
      stop_vertical: boolean;
    }
  | {
      version: 1;
      sequence: number;
      type: "input.keyboard.key";
      keycode: number;
      pressed: boolean;
    };
