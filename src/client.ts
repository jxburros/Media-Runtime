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
      ) as Error & { status: number; code: string };
      error.status = response.status;
      error.code = data.error?.code;
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
}
