import { test } from "node:test";
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { mkdtemp, rm, unlink, readFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import { Runtime } from "../dist/runtime.js";
import { createRuntimeServer } from "../dist/server.js";
import { MediaRuntimeClient } from "../dist/client.js";
const engine = resolve(process.env.MEDIA_ENGINE_REPO || "../Media-Engine");
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const token = "runtime-test-token-not-a-secret-000000000000";
import { deflateSync } from "node:zlib";
function makePng(width, height) {
  const crcTable = [];
  for (let n = 0; n < 256; n++) {
    let c = n;
    for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
    crcTable.push(c >>> 0);
  }
  const crc = (buf) => {
    let c = 0xffffffff;
    for (const b of buf) c = crcTable[(c ^ b) & 0xff] ^ (c >>> 8);
    return (c ^ 0xffffffff) >>> 0;
  };
  const chunk = (type, data) => {
    const length = Buffer.alloc(4);
    length.writeUInt32BE(data.length);
    const body = Buffer.concat([Buffer.from(type, "latin1"), data]);
    const sum = Buffer.alloc(4);
    sum.writeUInt32BE(crc(body));
    return Buffer.concat([length, body, sum]);
  };
  const ihdr = Buffer.alloc(13);
  ihdr.writeUInt32BE(width, 0);
  ihdr.writeUInt32BE(height, 4);
  ihdr[8] = 8;
  ihdr[9] = 2;
  const row = Buffer.concat([Buffer.from([0]), Buffer.alloc(width * 3, 0x40)]);
  return Buffer.concat([
    Buffer.from("89504e470d0a1a0a", "hex"),
    chunk("IHDR", ihdr),
    chunk("IDAT", deflateSync(Buffer.concat(Array(height).fill(row)))),
    chunk("IEND", Buffer.alloc(0)),
  ]);
}
const workerToken = "worker-test-token-not-a-secret-000000000000";
const submit = {
  recipe_id: "portrait",
  model_id: "fixture",
  inputs: { prompt: "test" },
  parameters: { width: 64, height: 64, steps: 3 },
};

test(
  "Runtime and the real Engine HTTP protocol cooperate",
  { timeout: 30000 },
  async (t) => {
    const root = await mkdtemp(join(tmpdir(), "media-contract-"));
    const child = spawn(
      "python",
      [join(engine, "tests/serve_fixture.py"), root],
      {
        env: {
          ...process.env,
          PYTHONPATH: join(engine, "src"),
          MEDIA_ENGINE_TOKEN: workerToken,
        },
        stdio: ["ignore", "pipe", "pipe"],
      },
    );
    let stderr = "";
    child.stderr.on("data", (c) => (stderr += c));
    const discovery = await new Promise((resolve, reject) => {
      let text = "";
      child.stdout.on("data", (c) => {
        text += c;
        if (text.includes("\n")) resolve(JSON.parse(text.split("\n")[0]));
      });
      child.on("exit", () => reject(new Error(stderr)));
    });
    process.env.MEDIA_TEST_WORKER_TOKEN = workerToken;
    const config = {
      directory: join(root, "runtime"),
      port: 0,
      queue_capacity: 2,
      cancellation_grace_ms: 500,
      poll_interval_ms: 20,
      worker_timeout_ms: 1000,
      max_output_bytes: 32 * 1024 * 1024,
      output_retention_days: null,
      max_input_bytes: 32 * 1024 * 1024,
      artifact_catalog: {
        fixture: join(root, "model"),
        "fixture-lora": join(root, "lora"),
        absent: join(root, "nope"),
      },
      worker: {
        url: `http://127.0.0.1:${discovery.port}`,
        token_env: "MEDIA_TEST_WORKER_TOKEN",
      },
      credentials: [],
      tokens: [
        {
          token,
          namespace: "owner",
          operator: true,
          allowed_namespaces: ["studio:*"],
        },
        {
          token: "reader-token-not-a-secret-00000000000000",
          namespace: "reader",
          operator: false,
          allowed_namespaces: [],
        },
      ],
    };
    let runtime, server, client;
    async function boot() {
      runtime = new Runtime(config);
      await runtime.start();
      server = createRuntimeServer(runtime);
      await new Promise((r) => server.listen(0, "127.0.0.1", r));
      client = new MediaRuntimeClient(
        `http://127.0.0.1:${server.address().port}`,
        token,
      );
    }
    async function shutdown() {
      await new Promise((r) => server.close(r));
      await runtime.stop();
    }
    async function wait(
      id,
      predicate = (j) => ["succeeded", "failed", "cancelled"].includes(j.state),
    ) {
      const start = Date.now();
      while (Date.now() - start < 10000) {
        const job = await client.status(id);
        if (predicate(job)) return job;
        await sleep(25);
      }
      throw new Error(
        "job timed out: " + JSON.stringify(runtime.store.get(id)),
      );
    }
    try {
      await boot();
      await client.json("/v1/models", "POST", {
        model_id: "fixture",
        artifact_handle: "fixture",
        source: { license: "test-only" },
      });
      await t.test(
        "strict validation, auth and namespace boundaries",
        async () => {
          assert.equal((await fetch(client.url + "/health")).status, 200);
          assert.equal((await fetch(client.url + "/v1/models")).status, 401);
          assert.equal(
            (
              await fetch(client.url + "/health", {
                headers: { Origin: "https://example.test" },
              })
            ).status,
            403,
          );
          await assert.rejects(
            () => client.submit({ ...submit, namespace: "someone" }),
            (e) => e.status === 400,
          );
          await assert.rejects(
            () => client.submit({ ...submit, parameters: { unknown: true } }),
            (e) => e.status === 400,
          );
          const reader = new MediaRuntimeClient(
            client.url,
            config.tokens[1].token,
          );
          await assert.rejects(
            () =>
              reader.json("/v1/models", "POST", {
                model_id: "x",
                artifact_handle: "fixture",
              }),
            (e) => e.status === 403,
          );
          const scoped = new MediaRuntimeClient(
            client.url,
            token,
            "studio:personal:user-a",
          );
          const job = await scoped.submit(submit);
          await assert.rejects(
            () => client.status(job.job_id),
            (e) => e.status === 404,
          );
          await assert.rejects(
            () =>
              new MediaRuntimeClient(
                client.url,
                config.tokens[1].token,
                "studio:personal:user-a",
              ).status(job.job_id),
            (e) => e.status === 403,
          );
          await scoped.cancel(job.job_id);
        },
      );
      await t.test(
        "durable PNG, provenance, idempotency and replayable SSE",
        async () => {
          const accepted = await client.submit(submit, "one");
          const retried = await client.submit(submit, "one");
          assert.equal(accepted.job_id, retried.job_id);
          await assert.rejects(
            () =>
              client.submit(
                { ...submit, inputs: { prompt: "different" } },
                "one",
              ),
            (e) => e.status === 409,
          );
          const done = await wait(accepted.job_id);
          assert.equal(done.state, "succeeded");
          const bytes = Buffer.from(
            await (await client.output(done.job_id)).arrayBuffer(),
          );
          assert.equal(
            bytes.subarray(0, 8).toString("hex"),
            "89504e470d0a1a0a",
          );
          const manifest = await client.manifest(done.job_id);
          assert.equal(manifest.worker.test_fixture, true);
          assert.equal(
            manifest.request.parameters.seed,
            done.request.parameters.seed,
          );
          assert.ok(!JSON.stringify(manifest).includes(root));
          assert.ok(!JSON.stringify(manifest).includes(token));
          const events = await (
            await client.response(`/v1/jobs/${done.job_id}/events`, {
              headers: { "Last-Event-ID": "1" },
            })
          ).text();
          assert.ok(events.includes("event: progress"));
          assert.ok(!events.includes("id: 1\n"));
          await assert.rejects(
            () =>
              client.response(`/v1/jobs/${done.job_id}/events`, {
                headers: { "Last-Event-ID": "9999" },
              }),
            (e) => e.status === 409,
          );
          await unlink(
            join(config.directory, "outputs", done.job_id, "image.png"),
          );
          await assert.rejects(
            () => client.output(done.job_id),
            (e) => e.status === 410,
          );
        },
      );
      await t.test(
        "bounded FIFO, queued cancellation and running cancellation settle",
        async () => {
          const first = await client.submit({
            ...submit,
            parameters: { ...submit.parameters, steps: 100 },
          });
          await wait(first.job_id, (j) => !!j.execution_id);
          const second = await client.submit(submit);
          const third = await client.submit(submit);
          await assert.rejects(
            () => client.submit(submit),
            (e) => e.status === 429,
          );
          assert.equal((await client.cancel(second.job_id)).state, "cancelled");
          const cancelled = await client.cancel(first.job_id);
          assert.ok(cancelled.cancel_requested);
          assert.equal((await wait(first.job_id)).state, "cancelled");
          assert.equal((await wait(third.job_id)).state, "succeeded");
        },
      );
      await t.test(
        "restart reconciles surviving execution and keeps queued work",
        async () => {
          const first = await client.submit({
            ...submit,
            parameters: { ...submit.parameters, steps: 25 },
          });
          const active = await wait(first.job_id, (j) => !!j.execution_id);
          const queued = await client.submit(submit);
          await shutdown();
          await sleep(120);
          await boot();
          const recovered = await wait(first.job_id);
          assert.equal(recovered.state, "succeeded");
          assert.equal(recovered.attempt_id, active.attempt_id);
          assert.equal(recovered.execution_id, active.execution_id);
          assert.equal((await wait(queued.job_id)).state, "succeeded");
        },
      );
      await t.test(
        "lost acceptance response reuses one session attempt",
        async () => {
          const original = runtime.worker.json.bind(runtime.worker);
          let intercepted = false;
          runtime.worker.json = async (path, method, body) => {
            const result = await original(path, method, body);
            if (
              path === "/v1/executions" &&
              method === "POST" &&
              !intercepted
            ) {
              intercepted = true;
              const { ApiError } = await import("../dist/contracts.js");
              throw new ApiError(503, "backend_unavailable", "lost response");
            }
            return result;
          };
          const accepted = await client.submit(submit);
          assert.equal((await wait(accepted.job_id)).state, "succeeded");
          assert.equal(intercepted, true);
          runtime.worker.json = original;
        },
      );
      if (process.env.MEDIA_STUDIO_REPO)
        await t.test(
          "Studio adapter completes through Runtime and Engine in personal and project namespaces",
          async () => {
            const { LocalMediaRuntime } = await import(
              join(
                resolve(process.env.MEDIA_STUDIO_REPO),
                "app/backend/dist/media/localMediaRuntime.js",
              )
            );
            for (const namespace of [
              "studio:personal-smoke",
              "studio:project-smoke",
            ]) {
              const adapter = new LocalMediaRuntime({
                id: "local-media",
                kind: "media-runtime",
                label: "Media Runtime",
                target: "local-server",
                baseUrl: client.url,
                authHeader: "Bearer " + token,
                callerNamespace: namespace,
              });
              assert.equal((await adapter.health()).ok, true);
              const job = await adapter.generateImage({
                task: "text-to-image",
                model: "fixture",
                prompt: "test",
                width: 64,
                height: 64,
                steps: 3,
                seed: 42,
              });
              const stages = [];
              const result = await adapter.waitForJob(job, {
                pollIntervalMs: 20,
                onStage: (stage) => stages.push(stage),
              });
              assert.equal(result.state, "succeeded");
              assert.ok(stages.length > 0);
              const [output] = await adapter.getOutputs(job);
              assert.equal(output.mimeType, "image/png");
              assert.equal(
                adapter.describeProvenance(job).nativeRequest.worker
                  .test_fixture,
                true,
              );
              await assert.rejects(
                () => client.status(job.nativeJobId),
                (e) => e.status === 404,
              );
            }
          },
        );
      await t.test(
        "image-to-image flows through durable, namespace-owned inputs",
        async () => {
          const record = await client.uploadInput(makePng(64, 64));
          assert.equal(record.width, 64);
          assert.equal(record.namespace, undefined);
          const reader = new MediaRuntimeClient(client.url, config.tokens[1].token);
          await assert.rejects(
            () => reader.input(record.input_id),
            (e) => e.status === 404,
          );
          await assert.rejects(
            () =>
              client.submit({
                ...submit,
                inputs: { prompt: "test", image: record.input_id },
              }),
            (e) => e.status === 400 && e.field === "inputs.image",
          );
          await assert.rejects(
            () =>
              client.submit({
                recipe_id: "variation",
                model_id: "fixture",
                inputs: { prompt: "test", image: record.input_id },
                parameters: { width: 128, steps: 4 },
              }),
            (e) => e.status === 400 && e.field === "parameters.width",
          );
          await assert.rejects(
            () =>
              client.submit({
                recipe_id: "variation",
                model_id: "fixture",
                inputs: { prompt: "test" },
                parameters: { steps: 4 },
              }),
            (e) => e.status === 400 && e.field === "inputs.image",
          );
          const accepted = await client.submit({
            recipe_id: "variation",
            model_id: "fixture",
            inputs: { prompt: "test", image: record.input_id },
            parameters: { steps: 4 },
          });
          await wait(accepted.job_id, (j) => !!j.execution_id);
          await assert.rejects(
            () => client.deleteInput(record.input_id),
            (e) => e.status === 409,
          );
          const done = await wait(accepted.job_id);
          assert.equal(done.state, "succeeded", JSON.stringify(done.error));
          assert.equal(done.task, "image-to-image");
          assert.equal(done.request.parameters.width, 64);
          assert.equal(done.request.parameters.strength, 0.6);
          assert.equal(done.input.sha256, record.sha256);
          const manifest = await client.manifest(done.job_id);
          assert.equal(manifest.task, "image-to-image");
          assert.equal(manifest.inputs[0].sha256, record.sha256);
          assert.equal(manifest.effective_settings.effective_steps, 2);
          assert.equal(manifest.backend.protocol, "media-worker/1.1");
          await wait(done.job_id, (j) => j.released === true);
          assert.equal((await client.deleteInput(record.input_id)).deleted, true);
          assert.equal((await client.inputs()).length, 0);
        },
      );
      await t.test(
        "LoRA adapters are registered, validated and part of the load identity",
        async () => {
          await assert.rejects(
            () => client.submit({ ...submit, adapters: [{ adapter_id: "fixture-lora" }] }),
            (e) => e.status === 400 && e.code === "unknown_adapter",
          );
          const registered = await client.json("/v1/adapters", "POST", {
            adapter_id: "fixture-lora",
            artifact_handle: "fixture-lora",
            source: { license: "test-only" },
          });
          assert.equal(registered.layout.format, "peft-lora");
          assert.deepEqual(registered.layout.targets, ["text_encoder", "unet"]);
          assert.equal((await client.adapters())[0].adapter_id, "fixture-lora");
          await assert.rejects(
            () =>
              client.submit({
                ...submit,
                adapters: [{ adapter_id: "fixture-lora" }, { adapter_id: "fixture-lora" }],
              }),
            (e) => e.status === 400,
          );
          await assert.rejects(
            () => client.submit({ ...submit, adapters: [{ adapter_id: "fixture-lora", scale: 5 }] }),
            (e) => e.status === 400,
          );
          const before = (await client.health()).worker.loaded_bundle_digest;
          const accepted = await client.submit({
            ...submit,
            adapters: [{ adapter_id: "fixture-lora", scale: 0.7 }],
          });
          const done = await wait(accepted.job_id);
          assert.equal(done.state, "succeeded", JSON.stringify(done.error));
          assert.deepEqual(done.adapters, [
            { adapter_id: "fixture-lora", digest: registered.digest, scale: 0.7 },
          ]);
          const manifest = await client.manifest(done.job_id);
          assert.equal(manifest.adapters[0].scale, 0.7);
          assert.equal(manifest.backend.memory_estimate.adapters.length, 1);
          const info = await runtime.worker.json("/v1/info");
          assert.equal(info.loaded_bundle.adapters[0].scale, 0.7);
          assert.equal(info.loaded_bundle.bundle_digest, before);
          // A job without adapters needs a fresh load; the identity includes the adapter set.
          const plain = await wait((await client.submit(submit)).job_id);
          assert.equal(plain.state, "succeeded");
          assert.deepEqual(plain.adapters, []);
          assert.equal((await runtime.worker.json("/v1/info")).loaded_bundle.adapters.length, 0);
          const reader = new MediaRuntimeClient(client.url, config.tokens[1].token);
          await assert.rejects(
            () => reader.json("/v1/adapters", "POST", { adapter_id: "x", artifact_handle: "fixture-lora" }),
            (e) => e.status === 403,
          );
        },
      );
      await t.test(
        "protocol negotiation serves a 1.0 worker without image-to-image",
        async () => {
          assert.equal((await client.health()).worker.protocol, "media-worker/1.1");
          const original = runtime.worker.probe.bind(runtime.worker);
          const originalJson = runtime.worker.json.bind(runtime.worker);
          runtime.worker.json = async (path, method, body, type) => {
            const result = await originalJson(path, method, body, type);
            if (path === "/v1/info" || path === "/v1/capabilities") {
              delete result.supported_protocols;
              result.protocol = "media-worker/1.0";
            }
            return result;
          };
          try {
            assert.equal((await client.health()).worker.protocol, "media-worker/1.0");
            const record = await client.uploadInput(makePng(64, 64));
            await assert.rejects(
              () =>
                client.submit({
                  recipe_id: "variation",
                  model_id: "fixture",
                  inputs: { prompt: "test", image: record.input_id },
                }),
              (e) => e.status === 400 && e.code === "unsupported",
            );
            await assert.rejects(
              () => client.submit({ ...submit, adapters: [{ adapter_id: "fixture-lora" }] }),
              (e) => e.status === 400 && e.code === "unsupported",
            );
            const legacy = await client.submit(submit);
            assert.equal((await wait(legacy.job_id)).state, "succeeded");
            assert.equal(
              (await client.manifest(legacy.job_id)).backend.protocol,
              "media-worker/1.0",
            );
            await client.deleteInput(record.input_id);
          } finally {
            runtime.worker.json = originalJson;
            runtime.worker.probe = original;
          }
        },
      );
      await t.test(
        "catalog inspection and model verification guide installation",
        async () => {
          const entries = await client.catalog();
          const fixture = entries.find((e) => e.artifact_handle === "fixture");
          assert.equal(fixture.complete, true);
          assert.deepEqual(fixture.registration, {
            model_id: "fixture",
            artifact_handle: "fixture",
          });
          const absent = entries.find((e) => e.artifact_handle === "absent");
          assert.equal(absent.present, false);
          assert.equal(absent.complete, false);
          assert.ok(!JSON.stringify(entries).includes(root));
          const reader = new MediaRuntimeClient(client.url, config.tokens[1].token);
          await assert.rejects(() => reader.catalog(), (e) => e.status === 403);
          const detail = await client.model("fixture", true);
          assert.equal(detail.verification.valid, true);
          assert.equal(detail.verification.memory_estimate.source, "safetensors_headers");
          assert.equal(detail.components.length, 7);
          const newest = await client.jobs({ limit: 2, newest: true });
          assert.equal(newest.jobs.length, 2);
          assert.equal(newest.next_cursor, null);
          assert.ok(newest.jobs[0].created_at >= newest.jobs[1].created_at);
        },
      );
      await t.test(
        "failure is private and lost session never regenerates",
        async () => {
          const failed = await client.submit({
            ...submit,
            inputs: { prompt: "fail" },
          });
          const result = await wait(failed.job_id);
          assert.equal(result.state, "failed");
          assert.equal(result.error.code, "execution_failed");
          assert.ok(!JSON.stringify(result.error).includes("secret"));
          const accepted = await client.submit({
            ...submit,
            parameters: { ...submit.parameters, steps: 100 },
          });
          await wait(accepted.job_id, (j) => !!j.execution_id);
          // Simulate the discovery of a replacement session while preserving the worker itself for cleanup.
          const original = runtime.worker.probe.bind(runtime.worker);
          runtime.worker.probe = async () => {
            const r = await original();
            r.info.session_id = "replacement";
            r.caps.session_id = "replacement";
            return r;
          };
          assert.equal(
            (await wait(accepted.job_id)).error.code,
            "execution_interrupted",
          );
          runtime.worker.probe = original;
        },
      );
    } finally {
      if (server) await shutdown();
      child.kill("SIGTERM");
      await new Promise((r) => child.once("exit", r));
      await rm(root, { recursive: true, force: true });
    }
  },
);

test(
  "owned worker crashes and cancellation deadlines create fresh sessions",
  { timeout: 20000 },
  async () => {
    const root = await mkdtemp(join(tmpdir(), "media-owned-"));
    const previousPythonPath = process.env.PYTHONPATH;
    process.env.PYTHONPATH = join(engine, "src");
    const config = {
      directory: join(root, "runtime"),
      port: 0,
      queue_capacity: 2,
      cancellation_grace_ms: 80,
      poll_interval_ms: 20,
      worker_timeout_ms: 500,
      max_output_bytes: 33554432,
      output_retention_days: null,
      artifact_catalog: { fixture: join(root, "model") },
      worker: {
        url: "http://127.0.0.1:7811",
        token_env: "IGNORED_OWNED_TOKEN",
        command: ["python", join(engine, "tests/serve_fixture.py"), root],
      },
      credentials: [],
      tokens: [
        { token, namespace: "owner", operator: true, allowed_namespaces: [] },
      ],
    };
    const runtime = new Runtime(config);
    let server;
    try {
      await runtime.start();
      server = createRuntimeServer(runtime);
      await new Promise((r) => server.listen(0, "127.0.0.1", r));
      const client = new MediaRuntimeClient(
        `http://127.0.0.1:${server.address().port}`,
        token,
      );
      await client.json("/v1/models", "POST", {
        model_id: "fixture",
        artifact_handle: "fixture",
      });
      async function wait(id, predicate) {
        for (let i = 0; i < 300; i++) {
          const j = await client.status(id);
          if (predicate(j)) return j;
          await sleep(20);
        }
        throw new Error("owned job timed out");
      }
      const first = await client.submit({
        ...submit,
        parameters: { ...submit.parameters, steps: 100 },
      });
      const active = await wait(first.job_id, (j) => !!j.execution_id);
      runtime.worker.child.kill("SIGKILL");
      assert.equal(
        (await wait(first.job_id, (j) => j.state === "failed")).error.code,
        "execution_interrupted",
      );
      for (let i = 0; i < 100; i++) {
        if (
          runtime.worker.child?.exitCode === null &&
          runtime.worker.discoveredSession !== active.session_id
        )
          break;
        await sleep(20);
      }
      const next = await client.submit({
        ...submit,
        inputs: { prompt: "uncancellable" },
      });
      await wait(next.job_id, (j) => !!j.execution_id);
      await client.cancel(next.job_id);
      const stopped = await wait(next.job_id, (j) => j.state === "failed");
      assert.equal(stopped.error.code, "execution_interrupted");
      assert.equal(stopped.error.forced_termination, true);
    } finally {
      if (server) await new Promise((r) => server.close(r));
      await runtime.stop();
      if (previousPythonPath === undefined) delete process.env.PYTHONPATH;
      else process.env.PYTHONPATH = previousPythonPath;
      await rm(root, { recursive: true, force: true });
    }
  },
);
