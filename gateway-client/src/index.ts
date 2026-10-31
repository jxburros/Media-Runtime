/** One committed output of a job. Type-specific fields appear for their media types. */
export type OutputRecord = {
  output_id: string;
  mime_type: "image/png" | "application/zip" | "video/mp4" | "audio/wav";
  size: number;
  sha256: string;
  width?: number;
  height?: number;
  /** Seed of this image in a text-to-image batch: seed + k − 1 for output k. */
  seed?: number;
  /** Linear jobs: the producing operation, the worker's output ID and whether it is published. */
  op_id?: string;
  operation_output_id?: string;
  published?: boolean;
  /** image-to-video `frames` archive: frame count and rate from its frames.json, and
   * duration_seconds = frames / fps. The `poster` is frame 1 as a PNG. */
  frames?: number;
  fps?: number;
  duration_seconds?: number;
  sample_rate?: number;
  channels?: number;
  expires_at: string | null;
};

export type Fallback = "cpu" | "lower_precision";

export type Submission = {
  recipe_id: string;
  /** The model of the first operation. */
  model_id: string;
  /** Schema-2 recipes: the models of later operations by op_id. */
  models?: Record<string, string>;
  /** Schema-1 inputs, or the declared inputs of a schema-2 recipe (images are input IDs). */
  inputs: { prompt?: string; negative_prompt?: string; image?: string } | Record<string, string | number>;
  parameters?: {
    width?: number;
    height?: number;
    steps?: number;
    guidance?: number;
    seed?: number;
    sampler?: "euler";
    scheduler?: "euler";
    strength?: number;
    /** text-to-image only; produces image, image_2 … image_N in one execution. */
    count?: number;
    /** upscale only. */
    tile?: number;
    tile_overlap?: number;
    /** image-to-video only; frames defaults to the model's default_frames, the others to the
     * worker's task defaults. width/height are the input's and must be one of video_sizes. */
    frames?: number;
    fps?: number;
    motion?: number;
    noise_augmentation?: number;
    decode_chunk?: number;
  };
  adapters?: { adapter_id: string; scale?: number }[];
  backend_id?: "diffusers-sdxl" | "owned-sdxl";
  backend_options?: Record<
    string,
    { precision?: "float16" | "float32" | "auto"; attention?: "sdpa"; offload?: "none" }
  >;
  /** Forwarded only for credentials configured with allow_fallbacks. */
  authorized_fallbacks?: Fallback[];
  retry_of?: string;
  /** Pins the job to one configured worker of the pool; it waits for that worker. */
  worker_id?: string;
};

/** The device a 1.2 worker reports; host, device and index name it, pid is the process. */
export type DeviceIdentity = {
  device: string;
  index: number | null;
  name: string | null;
  total_memory_mb: number | null;
  host_id: string;
  host_id_source?: string;
  pid: number;
  [key: string]: unknown;
};

/** GET /v1/workers: one configured worker of the pool. */
export type WorkerSummary = {
  worker_id: string;
  ready: boolean;
  state: string | null;
  protocol: string | null;
  backend_id: string | null;
  device: string | null;
  device_identity: DeviceIdentity | null;
  configured_device: string | null;
  configured_memory_budget_mb: number | null;
  owned_process: boolean;
  /** "http" for a loopback worker, "mtls" for a remote https:// worker (never owned). */
  transport: "http" | "mtls";
  remote: boolean;
  /** Remote workers: the server-certificate SHA-256 every connection must present (pinned or
   * recorded on first contact); null for loopback workers and before first contact. */
  certificate_sha256: string | null;
  /** Round trip of the last /v1/info request in milliseconds, null before the first. */
  latency_ms: number | null;
  /** The loaded identity: bundle, the registered models with that bundle, options and adapters. */
  loaded: {
    bundle_digest: string | null;
    model_ids: string[];
    options: Record<string, unknown> | null;
    adapters: { adapter_id: string | null; digest: string | null; scale: number | null }[];
  } | null;
  busy: boolean;
  /** Set only when the running job belongs to the caller's namespace. */
  running_job_id: string | null;
  error?: string;
  message?: string;
};

export type JobState = "queued" | "running" | "succeeded" | "failed" | "cancelled";

export type Task = "text-to-image" | "image-to-image" | "upscale" | "image-to-video";

export type Family = "sdxl-base" | "esrgan" | "svd";

/** Facts of a job's video operation, from the verified frame archive record. */
export type VideoFacts = {
  type: "video";
  frames: number;
  fps: number;
  duration_seconds: number;
  width: number;
  height: number;
  /** Whether the worker also produced the optional encoded `video` (video/mp4) output. */
  encoded_video: boolean;
};

export type MediaFacts = VideoFacts;

export type InputDeclaration =
  | { type: "string"; required: boolean; max_length?: number }
  | { type: "image"; required: boolean }
  | { type: "number"; required: boolean; minimum?: number; maximum?: number };

/** A recipe operation input: "input:<name>" or "op:<op_id>/<output_id>". */
export type Binding = { from: string };

export type RecipeOperation = {
  op_id: string;
  task: Task;
  model_family: Family;
  inputs: Record<string, Binding>;
  defaults: Record<string, number | string>;
  adapters?: { adapter_id: string; scale?: number }[];
};

/** GET /v1/recipes/{id}: any recipe in its schema-2 form, for native-workflow input mapping. */
export type RecipeDetail = {
  recipe_id: string;
  revision: number;
  /** The registered schema; schema-1 recipes are shown as one operation. */
  schema_version: 1 | 2;
  digest: string;
  inputs: Record<string, InputDeclaration>;
  operations: RecipeOperation[];
  /** op_id/output_id entries exposed under the job's outputs. */
  publish: string[];
};

export type OperationState = "pending" | "running" | "succeeded" | "failed" | "cancelled";

export type JobOperation = {
  op_id: string;
  task: Task;
  state: OperationState;
  model: { model_id: string; bundle_digest: string };
  input_from?: { op_id: string; output_id: string };
  parameters: Record<string, unknown>;
  attempt_id?: string;
  execution_id?: string;
  /** The worker the operation ran on; a linear job runs every operation on one worker. */
  worker_id?: string;
  outputs: OutputRecord[];
  /** Video operations: the committed media facts. */
  media?: MediaFacts;
  error?: { code: string; message: string };
  [key: string]: unknown;
};

export type Job = {
  job_id: string;
  state: JobState;
  stage: string | null;
  /** The first operation's task. */
  task: Task;
  cancel_requested: boolean;
  created_at: string;
  updated_at: string;
  request: Submission;
  /** Published outputs. */
  outputs: OutputRecord[];
  /** Linear jobs: committed outputs the recipe does not publish. */
  intermediate_outputs?: OutputRecord[];
  /** Jobs with a video operation: its facts (the last one's in a linear job). */
  media?: MediaFacts;
  operations: JobOperation[];
  current_operation: number;
  /** Linear jobs: the operation running now, or the last one run. */
  current_op_id?: string;
  applied_fallbacks?: Fallback[];
  /** The worker the job was dispatched to (null while queued) and its device identity. */
  worker_id: string | null;
  device_identity?: DeviceIdentity | null;
  /** The workers the job was admitted against, with the minor each negotiated then. */
  candidates?: { worker_id: string; protocol: string }[];
  /** `operation` names the failed operation of a linear job. */
  error?: { code: string; message: string; operation?: string; [key: string]: unknown };
  model: { model_id: string; bundle_digest: string };
  recipe: { recipe_id: string; revision: number; digest: string };
  backend: Record<string, unknown>;
  [key: string]: unknown;
};

export type ModelSummary = {
  model_id: string;
  family: Family;
  tasks: string[];
  /** Upscale factor of an esrgan model. */
  scale?: number;
  /** svd models: the [width, height] sizes the bundle serves and its default frame count. */
  video_sizes?: [number, number][];
  default_frames?: number;
  bundle_digest: string;
  source?: {
    repository?: string;
    revision?: string;
    license?: string;
    conversion?: {
      source_sha256: string;
      reference_bundle_digest: string;
      converter: Record<string, string>;
      mapped_keys: Record<string, number>;
    };
  };
  available: boolean;
};

/**
 * Gateway client. Credentials belong on the server, never in a browser. A runtime in network mode
 * is reached at an https:// URL; for a certificate from a private CA, start Node with
 * NODE_EXTRA_CA_CERTS naming that CA (fetch has no per-request trust option).
 */
export class MediaRuntimeClient {
  constructor(
    public url: string,
    private token: string,
    private namespace?: string,
  ) {}
  async response(path: string, options: RequestInit = {}) {
    const response = await fetch(this.url.replace(/\/$/, "") + path, {
      ...options,
      headers: {
        Authorization: `Bearer ${this.token}`,
        ...(this.namespace ? { "X-Media-Namespace": this.namespace } : {}),
        "Content-Type": "application/json",
        ...options.headers,
      },
      redirect: "error",
    });
    if (!response.ok) {
      const data = await response.json().catch(() => ({}));
      const error = new Error(
        data.error?.message || `Runtime returned ${response.status}`,
      ) as Error & { status: number; code: string; field?: string };
      error.status = response.status;
      error.code = data.error?.code;
      error.field = data.error?.field;
      throw error;
    }
    return response;
  }
  async json(path: string, method = "GET", body?: unknown) {
    return (
      await this.response(path, {
        method,
        body: body === undefined ? undefined : JSON.stringify(body),
      })
    ).json();
  }
  health() {
    return this.json("/v1/runtime");
  }
  models(): Promise<ModelSummary[]> {
    return this.json("/v1/models");
  }
  model(model_id: string, verify = false) {
    return this.json(
      `/v1/models/${encodeURIComponent(model_id)}${verify ? "?verify=1" : ""}`,
    );
  }
  catalog(handle?: string) {
    return this.json(
      "/v1/catalog" + (handle ? `?handle=${encodeURIComponent(handle)}` : ""),
    );
  }
  adapters() {
    return this.json("/v1/adapters");
  }
  recipes() {
    return this.json("/v1/recipes");
  }
  recipe(recipe_id: string): Promise<RecipeDetail> {
    return this.json(`/v1/recipes/${encodeURIComponent(recipe_id)}`);
  }
  validate(request: Submission | Record<string, unknown>) {
    return this.json("/v1/jobs/validate", "POST", request);
  }
  submit(request: Submission | Record<string, unknown>, key?: string): Promise<{ job_id: string; state: JobState }> {
    return this.response("/v1/jobs", {
      method: "POST",
      body: JSON.stringify(request),
      headers: key ? { "Idempotency-Key": key } : {},
    }).then((r) => r.json());
  }
  workers(): Promise<WorkerSummary[]> {
    return this.json("/v1/workers");
  }
  jobs(options: { limit?: number; after?: number; newest?: boolean; worker_id?: string } = {}) {
    const query = new URLSearchParams();
    if (options.worker_id) query.set("worker_id", options.worker_id);
    if (options.limit) query.set("limit", String(options.limit));
    if (options.after) query.set("after", String(options.after));
    if (options.newest) query.set("order", "newest");
    const text = query.toString();
    return this.json("/v1/jobs" + (text ? "?" + text : ""));
  }
  status(job: string): Promise<Job> {
    return this.json(`/v1/jobs/${encodeURIComponent(job)}`);
  }
  cancel(job: string): Promise<Job> {
    return this.json(`/v1/jobs/${encodeURIComponent(job)}/cancel`, "POST", {});
  }
  manifest(job: string) {
    return this.json(`/v1/jobs/${encodeURIComponent(job)}/manifest`);
  }
  /** Download one committed output by its ID (image, image_2 …, or generate__image). */
  output(job: string, output = "image") {
    return this.response(
      `/v1/jobs/${encodeURIComponent(job)}/outputs/${encodeURIComponent(output)}`,
    );
  }
  /** Upload PNG bytes as a durable, namespace-owned input for image-to-image recipes. */
  uploadInput(png: Uint8Array) {
    return this.response("/v1/inputs", {
      method: "POST",
      body: png,
      headers: { "Content-Type": "image/png" },
    }).then((r) => r.json());
  }
  inputs() {
    return this.json("/v1/inputs");
  }
  input(input_id: string) {
    return this.json(`/v1/inputs/${encodeURIComponent(input_id)}`);
  }
  deleteInput(input_id: string) {
    return this.json(`/v1/inputs/${encodeURIComponent(input_id)}`, "DELETE");
  }
}
