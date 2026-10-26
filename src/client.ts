/** Gateway/CLI client. Credentials belong on the server, never in a browser. */
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
  models() {
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
  validate(request: unknown) {
    return this.json("/v1/jobs/validate", "POST", request);
  }
  submit(request: unknown, key?: string) {
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
  status(job: string) {
    return this.json(`/v1/jobs/${encodeURIComponent(job)}`);
  }
  cancel(job: string) {
    return this.json(`/v1/jobs/${encodeURIComponent(job)}/cancel`, "POST", {});
  }
  manifest(job: string) {
    return this.json(`/v1/jobs/${encodeURIComponent(job)}/manifest`);
  }
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
