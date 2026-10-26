import { test } from "node:test";
import assert from "node:assert/strict";
import { renderDashboard, describeStage, outputLocation } from "../dist/tui.js";
import { explainDoctor, explainCatalog } from "../dist/cli.js";

const running = {
  job_id: "job_11111111-1111-1111-1111-111111111111",
  state: "running",
  stage: "sampling",
  task: "image-to-image",
  progress: { stage: "sampling", completed_steps: 2, total_steps: 4 },
  model: { model_id: "studio-sdxl" },
  backend: { backend_id: "owned-sdxl" },
  request: { parameters: { seed: 42, width: 64, height: 64, steps: 4, strength: 0.6 } },
  created_at: "2026-10-26T10:00:00.000Z",
  outputs: [],
};
const done = {
  ...running,
  job_id: "job_22222222-2222-2222-2222-222222222222",
  state: "succeeded",
  stage: null,
  progress: undefined,
  outputs: [{ output_id: "image", expires_at: null }],
};
const failed = {
  ...running,
  job_id: "job_33333333-3333-3333-3333-333333333333",
  state: "failed",
  error: { code: "execution_interrupted", forced_termination: true },
};

test("dashboard renders queue, stage, model, backend, seed, output and errors", () => {
  const lines = renderDashboard(
    {
      runtime: {
        runtime_version: "0.2.0",
        worker: {
          backend_id: "owned-sdxl",
          ownership: "owned-pipeline",
          worker_version: "0.2.0",
          protocol: "media-worker/1.1",
          device: "cuda:0",
          device_ready: true,
          state: "busy",
          memory_estimate: { value_mb: 6500, source: "safetensors_headers" },
        },
        queue: { running: 1, queued: 0, capacity: 32 },
        models: [{ model_id: "studio-sdxl", loaded: true, available: true }],
      },
      jobs: [running, done, failed],
      selected: 0,
      message: "",
      updated_at: "10:00:00Z",
    },
    132,
    20,
  );
  assert.equal(lines.length, 20);
  assert.ok(lines.every((l) => l.length <= 132));
  const text = lines.join("\n");
  assert.ok(text.includes("owned-sdxl (owned-pipeline)"));
  assert.ok(text.includes("1 running, 0 queued of 32"));
  assert.ok(text.includes("studio-sdxl*"));
  assert.ok(text.includes("sampling 2/4"));
  assert.ok(text.includes("image-to-image"));
  assert.ok(text.includes("output ready"));
  const detail = renderDashboard(
    { runtime: null, jobs: [done], selected: 0, message: "", updated_at: "" },
    200,
    12,
  ).join("\n");
  assert.ok(detail.includes(`/v1/jobs/${done.job_id}/outputs/image`));
  assert.ok(text.includes("execution_interrupted (forced)"));
  assert.ok(text.includes("strength 0.6"));
  assert.ok(text.includes("6500 MiB"));
  assert.equal(describeStage({ state: "queued", cancel_requested: true }), "cancelling");
  assert.equal(outputLocation({ state: "succeeded", outputs: [{ output_id: "image", expires_at: "2000-01-01T00:00:00Z" }] }), "expired");
});

test("doctor explanation flags unavailable workers and missing models", () => {
  const unhealthy = explainDoctor({
    runtime_version: "0.2.0",
    protocols: ["media-worker/1.0", "media-worker/1.1"],
    worker: { error: "backend_unavailable", message: "Worker is unavailable" },
    models: [],
    recipes: [{ recipe_id: "portrait", pipeline: "text-to-image" }],
    storage: { writable: true, available_bytes: 10 * 1024 ** 3, output_retention_days: null },
    queue: { capacity: 32, queued: 0, running: 0 },
  });
  assert.equal(unhealthy.healthy, false);
  assert.ok(unhealthy.lines.some((l) => l.includes("UNAVAILABLE")));
  const healthy = explainDoctor({
    runtime_version: "0.2.0",
    protocols: ["media-worker/1.0", "media-worker/1.1"],
    worker: {
      backend_id: "diffusers-sdxl",
      ownership: "reference",
      worker_version: "0.2.0",
      protocol: "media-worker/1.1",
      owned_process: true,
      device: "cuda:0",
      device_ready: true,
      state: "ready",
      tasks: ["text-to-image", "image-to-image"],
      missing_dependencies: [],
      dependencies: { torch: "2.6.0" },
      load_defaults: { precision: "float16" },
      precisions: ["float16", "float32"],
      memory_budget_mb: null,
      memory_estimate: { value_mb: 6500, precision: "float16", source: "safetensors_headers", uncertainty: "excludes activations" },
    },
    models: [{ model_id: "studio-sdxl", artifact_handle: "sdxl-base", available: true, missing_roles: [], loaded: true }],
    recipes: [{ recipe_id: "portrait", pipeline: "text-to-image" }],
    storage: { writable: true, available_bytes: 10 * 1024 ** 3, output_retention_days: 7 },
    queue: { capacity: 32, queued: 0, running: 0 },
  });
  assert.equal(healthy.healthy, true);
  assert.ok(healthy.lines.some((l) => l.includes("6500 MiB parameters at float16")));
  const catalog = explainCatalog([
    { artifact_handle: "sdxl-base", complete: false, present: true, file_count: 3, total_bytes: 0, problems: ["Missing or weightless components: unet"] },
  ]);
  assert.ok(catalog[0].includes("NOT READY"));
  assert.ok(catalog[1].includes("unet"));
});
