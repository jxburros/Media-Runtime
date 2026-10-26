import { z } from "zod";
import { createHash, randomBytes } from "node:crypto";
export const PROTOCOLS = ["media-worker/1.0", "media-worker/1.1"] as const;
export const PROTOCOL = PROTOCOLS[PROTOCOLS.length - 1];
export const VERSION = "0.2.0";
export const BACKENDS = ["diffusers-sdxl", "owned-sdxl"] as const;
export const TASKS = ["text-to-image", "image-to-image"] as const;
export const id = z.string().regex(/^[a-zA-Z0-9_-]{1,80}$/);
export const inputId = z.string().regex(/^input_[a-f0-9]{32}$/);
export const parameters = z
  .object({
    width: z.number().int().min(64).max(2048).multipleOf(8),
    height: z.number().int().min(64).max(2048).multipleOf(8),
    steps: z.number().int().min(1).max(150),
    guidance: z.number().min(0).max(30),
    seed: z.number().int().min(0).max(4294967295),
    sampler: z.literal("euler"),
    scheduler: z.literal("euler"),
    strength: z.number().gt(0).max(1).optional(),
  })
  .strict();
export const inputs = z
  .object({
    prompt: z
      .string()
      .min(1)
      .max(10000)
      .refine((s) => !!s.trim()),
    negative_prompt: z.string().max(10000).optional(),
    image: inputId.optional(),
  })
  .strict();
export const loadOptions = z
  .object({
    precision: z.enum(["float16", "float32"]),
    attention: z.literal("sdpa"),
    offload: z.literal("none"),
  })
  .strict();
export const submission = z
  .object({
    recipe_id: id,
    model_id: id,
    inputs,
    parameters: parameters.partial().default({}),
    backend_id: z.enum(BACKENDS).optional(),
    backend_options: z
      .object({
        "diffusers-sdxl": loadOptions.partial().optional(),
        "owned-sdxl": loadOptions.partial().optional(),
      })
      .strict()
      .optional(),
    retry_of: id.optional(),
  })
  .strict();
export const recipeSchema = z
  .object({
    schema_version: z.literal(1),
    recipe_id: id,
    revision: z.number().int().positive(),
    pipeline: z.enum(TASKS),
    model_family: z.literal("sdxl-base"),
    inputs: z
      .object({
        prompt: z
          .object({ type: z.literal("string"), required: z.literal(true) })
          .strict(),
        image: z
          .object({ type: z.literal("image"), required: z.literal(true) })
          .strict()
          .optional(),
      })
      .strict(),
    defaults: parameters.partial(),
  })
  .strict()
  .refine(
    (r) =>
      r.pipeline === "image-to-image"
        ? !!r.inputs.image
        : !r.inputs.image && r.defaults.strength === undefined,
    {
      message:
        "image-to-image recipes declare an image input; text-to-image recipes declare neither image nor strength",
      path: ["inputs"],
    },
  );
export const registration = z
  .object({
    model_id: id,
    artifact_handle: id,
    source: z
      .object({
        repository: z.string().max(300).optional(),
        revision: z.string().max(100).optional(),
        license: z.string().max(300).optional(),
      })
      .strict()
      .default({}),
  })
  .strict();
export const defaultRecipes = [
  recipeSchema.parse({
    schema_version: 1,
    recipe_id: "portrait",
    revision: 1,
    pipeline: "text-to-image",
    model_family: "sdxl-base",
    inputs: { prompt: { type: "string", required: true } },
    defaults: { width: 1024, height: 1024, steps: 30, guidance: 6 },
  }),
  recipeSchema.parse({
    schema_version: 1,
    recipe_id: "variation",
    revision: 1,
    pipeline: "image-to-image",
    model_family: "sdxl-base",
    inputs: {
      prompt: { type: "string", required: true },
      image: { type: "image", required: true },
    },
    defaults: { steps: 30, guidance: 6, strength: 0.6 },
  }),
];
export const defaultRecipe = defaultRecipes[0];
export function canonical(value: any): string {
  if (Array.isArray(value)) return "[" + value.map(canonical).join(",") + "]";
  if (value && typeof value === "object")
    return (
      "{" +
      Object.keys(value)
        .sort()
        .map((k) => JSON.stringify(k) + ":" + canonical(value[k]))
        .join(",") +
      "}"
    );
  return JSON.stringify(value);
}
export const hash = (value: unknown) =>
  "sha256:" + createHash("sha256").update(canonical(value)).digest("hex");
export const seed = () => randomBytes(4).readUInt32BE();
export const newInputId = () => "input_" + randomBytes(16).toString("hex");
export class ApiError extends Error {
  constructor(
    public status: number,
    public code: string,
    message: string,
    public field?: string,
  ) {
    super(message);
  }
}
export const terminal = (state: string) =>
  ["succeeded", "failed", "cancelled"].includes(state);
export const effectiveSteps = (p: { steps: number; strength?: number }) =>
  p.strength === undefined
    ? p.steps
    : Math.min(Math.floor(p.steps * p.strength), p.steps);
/** Validate a PNG signature, IHDR and IEND without decoding pixels; returns its dimensions. */
export function pngDimensions(data: Buffer) {
  if (
    data.length < 33 ||
    data.subarray(0, 8).toString("hex") !== "89504e470d0a1a0a" ||
    data.subarray(12, 16).toString("latin1") !== "IHDR" ||
    data.subarray(-8, -4).toString("latin1") !== "IEND"
  )
    throw new ApiError(400, "invalid_image", "Expected a complete PNG file");
  const width = data.readUInt32BE(16);
  const height = data.readUInt32BE(20);
  const depth = data[24];
  const color = data[25];
  if (depth !== 8 || ![0, 2, 4, 6].includes(color))
    throw new ApiError(
      400,
      "unsupported",
      "Use an 8-bit greyscale or RGB(A) PNG",
      "image",
    );
  for (const value of [width, height])
    if (value < 64 || value > 2048 || value % 8)
      throw new ApiError(
        400,
        "invalid_image",
        "Image sides must be 64–2048 pixels and multiples of eight",
      );
  return { width, height };
}
/** Choose the highest protocol minor both the runtime and the worker support. */
export function negotiate(
  advertised: unknown,
  fallback: unknown,
): (typeof PROTOCOLS)[number] {
  const offered = Array.isArray(advertised)
    ? advertised
    : typeof fallback === "string"
      ? [fallback]
      : [];
  for (let i = PROTOCOLS.length - 1; i >= 0; i--)
    if (offered.includes(PROTOCOLS[i])) return PROTOCOLS[i];
  throw new ApiError(
    503,
    "protocol_mismatch",
    `Requires one of ${PROTOCOLS.join(", ")} from the worker`,
  );
}
export type InputRecord = {
  input_id: string;
  namespace: string;
  mime_type: "image/png";
  size: number;
  sha256: string;
  width: number;
  height: number;
  created_at: string;
  expires_at: string | null;
};
export type Job = {
  job_id: string;
  namespace: string;
  task: string;
  state: string;
  stage: string | null;
  cancel_requested: boolean;
  created_at: string;
  updated_at: string;
  request: any;
  recipe: any;
  model: any;
  backend: any;
  input?: InputRecord;
  attempt_id?: string;
  session_id?: string;
  execution_id?: string;
  worker_input_id?: string;
  load_operation_id?: string;
  dispatch_phase?: "loading" | "executing";
  worker_sequence: number;
  progress?: any;
  error?: any;
  outputs: any[];
  manifest?: any;
  cancel_at?: number;
  retry_of?: string;
  released?: boolean;
};
