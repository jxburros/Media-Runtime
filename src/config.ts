import { z } from "zod";
import { readFileSync } from "node:fs";
import { resolve, isAbsolute } from "node:path";
import { BACKENDS, id } from "./contracts.js";
const positive = z.number().int().positive();
const schema = z
  .object({
    port: z.number().int().min(0).max(65535).default(7810),
    directory: z.string().default(".runtime"),
    queue_capacity: positive.default(32),
    cancellation_grace_ms: positive.default(10000),
    poll_interval_ms: positive.default(250),
    worker_timeout_ms: positive.default(30000),
    max_output_bytes: positive.default(32 * 1024 * 1024),
    max_input_bytes: positive.default(32 * 1024 * 1024),
    output_retention_days: z.number().positive().nullable().default(null),
    credentials: z
      .array(
        z
          .object({
            token_env: z.string(),
            namespace: id,
            operator: z.boolean().default(false),
            allowed_namespaces: z.array(z.string().min(1)).default([]),
          })
          .strict(),
      )
      .min(1),
    artifact_catalog: z.record(z.string().refine(isAbsolute)),
    worker: z
      .object({
        url: z.string().url().default("http://127.0.0.1:7811"),
        token_env: z.string().default("MEDIA_ENGINE_TOKEN"),
        command: z.array(z.string()).min(1).optional(),
        backend_id: z.enum(BACKENDS).optional(),
      })
      .strict(),
  })
  .strict();
export type Config = z.infer<typeof schema> & {
  tokens: {
    token: string;
    namespace: string;
    operator: boolean;
    allowed_namespaces: string[];
  }[];
};
export function readConfig(
  file = process.env.MEDIA_RUNTIME_CONFIG || "runtime.config.json",
): Config {
  const raw = JSON.parse(readFileSync(file, "utf8"));
  const parsed = schema.parse(raw);
  const url = new URL(parsed.worker.url);
  if (
    url.protocol !== "http:" ||
    !["127.0.0.1", "localhost", "[::1]"].includes(url.hostname) ||
    url.username ||
    url.password ||
    url.search ||
    url.hash ||
    url.pathname !== "/"
  )
    throw new Error("Worker must use an authenticated loopback HTTP URL");
  const tokens = parsed.credentials.map((c) => {
    const token = process.env[c.token_env];
    if (!token || token.length < 32)
      throw new Error(`Set ${c.token_env} to at least 32 random characters`);
    return { ...c, token };
  });
  if (new Set(tokens.map((c) => c.token)).size !== tokens.length)
    throw new Error("Credentials must be unique");
  return { ...parsed, directory: resolve(parsed.directory), tokens };
}
