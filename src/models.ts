import { createHash } from "node:crypto";
import { createReadStream } from "node:fs";
import { readdir, lstat, readFile, realpath, stat } from "node:fs/promises";
import { join } from "node:path";
import { ApiError, adapterRegistration, hash, registration } from "./contracts.js";
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

const ROLES = [
  "unet",
  "vae",
  "text_encoder",
  "text_encoder_2",
  "tokenizer",
  "tokenizer_2",
  "scheduler",
];
const WEIGHT_ROLES = ROLES.slice(0, 4);
export type CatalogEntry = {
  artifact_handle: string;
  present: boolean;
  pipeline: string | null;
  roles: Record<string, { present: boolean; safetensors: number; files: number }>;
  missing_roles: string[];
  forbidden_files: string[];
  symlinks: string[];
  scheduler: string | null;
  total_bytes: number;
  file_count: number;
  complete: boolean;
  problems: string[];
  registration: { model_id: string; artifact_handle: string } | null;
};
/**
 * The explicit installation workflow: inspect approved catalog directories without hashing or
 * registering, so an operator sees what is missing before downloading or registering anything.
 * No path leaves this function; entries name opaque handles and relative roles only.
 */
export async function inspectCatalog(
  catalog: Record<string, string>,
  handle?: string,
): Promise<CatalogEntry[]> {
  const entries: CatalogEntry[] = [];
  for (const [artifact_handle, configured] of Object.entries(catalog)) {
    if (handle && handle !== artifact_handle) continue;
    const entry: CatalogEntry = {
      artifact_handle,
      present: false,
      pipeline: null,
      roles: Object.fromEntries(
        ROLES.map((r) => [r, { present: false, safetensors: 0, files: 0 }]),
      ),
      missing_roles: [...ROLES],
      forbidden_files: [],
      symlinks: [],
      scheduler: null,
      total_bytes: 0,
      file_count: 0,
      complete: false,
      problems: [],
      registration: null,
    };
    entries.push(entry);
    let root: string;
    try {
      root = await realpath(configured);
      if (!(await stat(root)).isDirectory()) throw new Error();
      entry.present = true;
    } catch {
      entry.problems.push("Approved directory is missing or not a directory");
      continue;
    }
    async function walk(relative = "") {
      for (const name of (await readdir(join(root, relative))).sort()) {
        const key = relative ? `${relative}/${name}` : name;
        const info = await lstat(join(root, key));
        if (info.isSymbolicLink()) {
          entry.symlinks.push(key);
          continue;
        }
        if (info.isDirectory()) await walk(key);
        else if (info.isFile()) {
          entry.file_count++;
          entry.total_bytes += info.size;
          if (/\.(py|pkl|pickle|bin|pt|pth)$/.test(name))
            entry.forbidden_files.push(key);
          const role = key.split("/")[0];
          if (entry.roles[role] && key.includes("/")) {
            entry.roles[role].files++;
            entry.roles[role].present = true;
            if (name.endsWith(".safetensors")) entry.roles[role].safetensors++;
          }
        }
      }
    }
    try {
      await walk();
    } catch {
      entry.problems.push("Approved directory could not be read");
      continue;
    }
    try {
      const index = JSON.parse(
        await readFile(join(root, "model_index.json"), "utf8"),
      );
      entry.pipeline = String(index._class_name ?? "unknown");
    } catch {
      entry.problems.push("model_index.json is missing or invalid");
    }
    try {
      const scheduler = JSON.parse(
        await readFile(join(root, "scheduler/scheduler_config.json"), "utf8"),
      );
      entry.scheduler = String(scheduler._class_name ?? "unknown");
    } catch {
      entry.problems.push("scheduler/scheduler_config.json is missing or invalid");
    }
    entry.missing_roles = ROLES.filter(
      (r) =>
        !entry.roles[r].present ||
        (WEIGHT_ROLES.includes(r) && entry.roles[r].safetensors === 0),
    );
    if (entry.missing_roles.length)
      entry.problems.push(
        `Missing or weightless components: ${entry.missing_roles.join(", ")}`,
      );
    if (entry.pipeline && entry.pipeline !== "StableDiffusionXLPipeline")
      entry.problems.push("Only SDXL base Diffusers bundles are supported");
    if (entry.scheduler && entry.scheduler !== "EulerDiscreteScheduler")
      entry.problems.push("Only the Euler discrete scheduler is supported");
    if (entry.forbidden_files.length)
      entry.problems.push(
        "Remove Python, pickle and non-safetensors weight files before registering",
      );
    if (entry.symlinks.length)
      entry.problems.push("Symlinks are rejected; copy the real files");
    entry.complete = entry.problems.length === 0;
    if (entry.complete)
      entry.registration = {
        model_id: artifact_handle,
        artifact_handle,
      };
  }
  return entries;
}

/** Register an approved LoRA adapter directory: one safetensors file, hashed, no paths exposed. */
export async function buildAdapter(
  raw: unknown,
  catalog: Record<string, string>,
) {
  const data = adapterRegistration.parse(raw);
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
    throw new ApiError(400, "missing_component", "Approved adapter directory is unavailable");
  }
  const files: Record<string, string> = {};
  try {
    for (const name of (await readdir(root)).sort()) {
      const info = await lstat(join(root, name));
      if (info.isSymbolicLink())
        throw new ApiError(400, "unsupported", "Adapter symlinks are not supported");
      if (info.isDirectory())
        throw new ApiError(400, "unsupported", "An adapter directory holds flat files only");
      if (/\.(py|pkl|pickle|bin|pt|pth)$/.test(name))
        throw new ApiError(400, "unsupported", "Use a safetensors LoRA file");
      const digest = createHash("sha256");
      for await (const chunk of createReadStream(join(root, name))) digest.update(chunk);
      files[name] = digest.digest("hex");
    }
  } catch (e) {
    if (e instanceof ApiError) throw e;
    throw new ApiError(400, "missing_component", "Cannot read the approved adapter");
  }
  if (Object.keys(files).filter((f) => f.endsWith(".safetensors")).length !== 1)
    throw new ApiError(
      400,
      "unsupported",
      "An adapter directory contains exactly one .safetensors file",
    );
  const record = {
    schema_version: 1,
    adapter_id: data.adapter_id,
    artifact_handle: data.artifact_handle,
    files,
    source: data.source,
  };
  return { ...record, digest: hash(record) };
}
