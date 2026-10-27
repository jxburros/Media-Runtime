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
  model_id: string;
  inputs: { prompt?: string; negative_prompt?: string; image?: string };
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
};

export type JobState = "queued" | "running" | "succeeded" | "failed" | "cancelled";

export type Job = {
  job_id: string;
  state: JobState;
  stage: string | null;
  task: "text-to-image" | "image-to-image" | "upscale";
  cancel_requested: boolean;
  created_at: string;
  updated_at: string;
  request: Submission;
  outputs: OutputRecord[];
  applied_fallbacks?: Fallback[];
  error?: { code: string; message: string; [key: string]: unknown };
  model: { model_id: string; bundle_digest: string };
  recipe: { recipe_id: string; revision: number; digest: string };
  backend: Record<string, unknown>;
  [key: string]: unknown;
};

export type ModelSummary = {
  model_id: string;
  family: "sdxl-base" | "esrgan";
  tasks: string[];
  /** Upscale factor of an esrgan model. */
  scale?: number;
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

/** Gateway client. Credentials belong on the server, never in a browser. */
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
  jobs(options: { limit?: number; after?: number; newest?: boolean } = {}) {
    const query = new URLSearchParams();
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
  /** Download one committed output by its ID (image, image_2 …). */
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
