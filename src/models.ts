import { createHash } from "node:crypto";
import { createReadStream } from "node:fs";
import { readdir, lstat, readFile, realpath } from "node:fs/promises";
import { join } from "node:path";
import { ApiError, hash, registration } from "./contracts.js";
export async function buildBundle(
  raw: unknown,
  catalog: Record<string, string>,
) {
  const data = registration.parse(raw);
  const configured = catalog[data.artifact_handle];
  if (!configured)
    throw new ApiError(
      400,
      "missing_component",
      "Artifact handle is not in the operator catalog",
      "artifact_handle",
    );
  let root: string;
  try {
    root = await realpath(configured);
  } catch {
    throw new ApiError(
      400,
      "missing_component",
      "Approved bundle directory is unavailable",
    );
  }
  const files: Record<string, string> = {};
  async function walk(relative = "") {
    for (const name of (await readdir(join(root, relative))).sort()) {
      const key = relative ? `${relative}/${name}` : name;
      const path = join(root, key);
      const stat = await lstat(path);
      if (stat.isSymbolicLink())
        throw new ApiError(
          400,
          "unsupported",
          "Bundle symlinks are not supported",
        );
      if (stat.isDirectory()) await walk(key);
      else if (stat.isFile()) {
        if (/\.(py|pkl|pickle|bin|pt|pth)$/.test(name))
          throw new ApiError(
            400,
            "unsupported",
            "Use a complete safetensors-only Diffusers bundle",
          );
        const digest = createHash("sha256");
        for await (const chunk of createReadStream(path)) digest.update(chunk);
        files[key] = digest.digest("hex");
      }
    }
  }
  try {
    await walk();
  } catch (e) {
    if (e instanceof ApiError) throw e;
    throw new ApiError(
      400,
      "missing_component",
      "Cannot read the approved bundle",
    );
  }
  const roles = [
    "unet",
    "vae",
    "text_encoder",
    "text_encoder_2",
    "tokenizer",
    "tokenizer_2",
    "scheduler",
  ];
  let index: any;
  try {
    index = JSON.parse(await readFile(join(root, "model_index.json"), "utf8"));
  } catch {
    throw new ApiError(
      400,
      "missing_component",
      "Missing pipeline configuration",
    );
  }
  if (index._class_name !== "StableDiffusionXLPipeline")
    throw new ApiError(
      400,
      "unsupported",
      "Only SDXL base Diffusers bundles are supported",
    );
  const components = roles.map((role, i) => {
    const entries = Object.fromEntries(
      Object.entries(files).filter(([k]) => k.startsWith(role + "/")),
    );
    if (
      !Object.keys(entries).length ||
      (i < 4 && !Object.keys(entries).some((k) => k.endsWith(".safetensors")))
    )
      throw new ApiError(
        400,
        "missing_component",
        `Missing ${role} component`,
        role,
      );
    return {
      role,
      artifact_handle: data.artifact_handle,
      digest: hash(entries),
      tensor_format: i < 4 ? "safetensors" : "json",
    };
  });
  const bundle = {
    schema_version: 1,
    bundle_id: data.model_id,
    family: "sdxl-base",
    pipeline: "text-to-image",
    components,
    configuration: { artifact_handle: data.artifact_handle, files },
    source: data.source,
  };
  return {
    model_id: data.model_id,
    artifact_handle: data.artifact_handle,
    bundle: { ...bundle, bundle_digest: hash(bundle) },
  };
}
