import { z } from "zod";

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
    profile: z.string().min(1),
    option: z.string().min(1),
    width: z.number().int().min(320).max(7680).multipleOf(2),
    height: z.number().int().min(240).max(4320).multipleOf(2),
    framerate: z.number().int().min(1).max(120),
    bitrate_kbps: z.number().int().min(100),
  })
  .strict();

const videoQualityOptionSchema = z
  .object({
    label: z.string().min(1),
    width: z.number().int().min(320).max(7680).multipleOf(2),
    height: z.number().int().min(240).max(4320).multipleOf(2),
    framerate: z.number().int().min(1).max(120),
    bitrate_kbps: z.number().int().min(100),
  })
  .strict();

const videoProfileSchema = z
  .object({
    label: z.string().min(1),
    default_option: z.string().min(1),
    frontend_transform: z.enum(["none", "flip-horizontal", "flip-vertical", "rotate-180"]),
    codec: z
      .object({
        id: z.string().min(1),
        mime_type: z.string().startsWith("video/"),
        sdp_fmtp_line: z.string(),
      })
      .strict(),
    options: z.record(z.string(), videoQualityOptionSchema),
    limits: z
      .object({
        max_bitrate_kbps: z.number().int().nonnegative(),
        max_macroblocks_per_dimension: z.number().int().nonnegative(),
        max_macroblocks_per_frame: z.number().int().nonnegative(),
        max_macroblocks_per_second: z.number().int().nonnegative(),
      })
      .strict(),
  })
  .strict()
  .superRefine((profile, context) => {
    if (!profile.options[profile.default_option]) {
      context.addIssue({
        code: "custom",
        path: ["default_option"],
        message: `default quality option ${profile.default_option} is unavailable`,
      });
    }
  });

export const serverConfigSchema = z
  .object({
    version: z.literal(3),
    signaling_path: z.string().startsWith("/"),
    video: qualitySchema,
    video_profiles: z.record(z.string(), videoProfileSchema),
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
  .strict()
  .superRefine((config, context) => {
    const profile = config.video_profiles[config.video.profile];
    if (!profile) {
      context.addIssue({
        code: "custom",
        path: ["video", "profile"],
        message: `video profile ${config.video.profile} is unavailable`,
      });
      return;
    }
    if (!profile.options[config.video.option]) {
      context.addIssue({
        code: "custom",
        path: ["video", "option"],
        message: `quality option ${config.video.option} is unavailable for ${profile.label}`,
      });
      return;
    }
    const widthMacroblocks = Math.ceil(config.video.width / 16);
    const heightMacroblocks = Math.ceil(config.video.height / 16);
    const macroblocks = widthMacroblocks * heightMacroblocks;
    if (
      profile.limits.max_bitrate_kbps > 0 &&
      config.video.bitrate_kbps > profile.limits.max_bitrate_kbps
    ) {
      context.addIssue({
        code: "custom",
        path: ["video", "bitrate_kbps"],
        message: `${profile.label} bitrate exceeds ${profile.limits.max_bitrate_kbps} Kbit/s`,
      });
    }
    if (
      profile.limits.max_macroblocks_per_dimension > 0 &&
      (widthMacroblocks > profile.limits.max_macroblocks_per_dimension ||
        heightMacroblocks > profile.limits.max_macroblocks_per_dimension)
    ) {
      context.addIssue({
        code: "custom",
        path: ["video"],
        message: `${profile.label} dimensions exceed ${profile.limits.max_macroblocks_per_dimension} macroblocks`,
      });
    }
    if (
      profile.limits.max_macroblocks_per_frame > 0 &&
      macroblocks > profile.limits.max_macroblocks_per_frame
    ) {
      context.addIssue({
        code: "custom",
        path: ["video"],
        message: `${profile.label} frame size exceeds ${profile.limits.max_macroblocks_per_frame} macroblocks`,
      });
    }
    if (
      profile.limits.max_macroblocks_per_second > 0 &&
      macroblocks * config.video.framerate > profile.limits.max_macroblocks_per_second
    ) {
      context.addIssue({
        code: "custom",
        path: ["video", "framerate"],
        message: `${profile.label} frame rate exceeds ${profile.limits.max_macroblocks_per_second} macroblocks per second`,
      });
    }
  });

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
      version: z.literal(3),
      id: z.string(),
      type: z.literal("video.quality.set.result"),
      ok: z.literal(true),
      quality: qualitySchema,
    })
    .strict(),
  z
    .object({
      version: z.literal(3),
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
      version: z.literal(3),
      id: z.string(),
      type: z.literal("input.release.result"),
      ok: z.literal(true),
    })
    .strict(),
  z
    .object({
      version: z.literal(3),
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

export type QualityPatch = Quality;

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
    }
  | {
      version: 1;
      sequence: number;
      type: "input.keyboard.text";
      text: string;
    };
