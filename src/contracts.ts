import { z } from "zod";
import { createHash, randomBytes } from "node:crypto";
export const PROTOCOL = "media-worker/1.0";
export const VERSION = "0.1.0";
export const id = z.string().regex(/^[a-zA-Z0-9_-]{1,80}$/);
export const parameters = z
  .object({
    width: z.number().int().min(64).max(2048).multipleOf(8),
    height: z.number().int().min(64).max(2048).multipleOf(8),
    steps: z.number().int().min(1).max(150),
    guidance: z.number().min(0).max(30),
    seed: z.number().int().min(0).max(4294967295),
    sampler: z.literal("euler"),
    scheduler: z.literal("euler"),
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
    backend_id: id.optional(),
    backend_options: z
      .object({ "diffusers-sdxl": loadOptions.partial() })
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
    pipeline: z.literal("text-to-image"),
    model_family: z.literal("sdxl-base"),
    inputs: z
      .object({
        prompt: z
          .object({ type: z.literal("string"), required: z.literal(true) })
          .strict(),
      })
      .strict(),
    defaults: parameters.partial(),
  })
  .strict();
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
export const defaultRecipe = recipeSchema.parse({
  schema_version: 1,
  recipe_id: "portrait",
  revision: 1,
  pipeline: "text-to-image",
  model_family: "sdxl-base",
  inputs: { prompt: { type: "string", required: true } },
  defaults: { width: 1024, height: 1024, steps: 30, guidance: 6 },
});
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
export type Job = {
  job_id: string;
  namespace: string;
  state: string;
  stage: string | null;
  cancel_requested: boolean;
  created_at: string;
  updated_at: string;
  request: any;
  recipe: any;
  model: any;
  backend: any;
  attempt_id?: string;
  session_id?: string;
  execution_id?: string;
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
