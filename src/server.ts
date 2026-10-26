import {
  createServer,
  type IncomingMessage,
  type ServerResponse,
} from "node:http";
import { timingSafeEqual, randomUUID } from "node:crypto";
import { createReadStream, existsSync } from "node:fs";
import { stat, access } from "node:fs/promises";
import { join } from "node:path";
import { ZodError } from "zod";
import {
  ApiError,
  hash,
  recipeSchema,
  PROTOCOLS,
  terminal,
} from "./contracts.js";
import { Runtime } from "./runtime.js";
import { buildAdapter, buildBundle, inspectCatalog } from "./models.js";
export function createRuntimeServer(runtime: Runtime) {
  const streams = new Set<ServerResponse>();
  const server = createServer(async (req, res) => {
    const request_id = randomUUID();
    res.setHeader("Cache-Control", "no-store");
    res.setHeader("X-Content-Type-Options", "nosniff");
    function send(status: number, body: unknown) {
      res.writeHead(status, { "Content-Type": "application/json" });
      res.end(JSON.stringify(body));
    }
    async function raw(limit: number) {
      const chunks: Buffer[] = [];
      let size = 0;
      for await (const chunk of req) {
        size += chunk.length;
        if (size > limit)
          throw new ApiError(413, "invalid_body", "Request body is too large");
        chunks.push(chunk);
      }
      return Buffer.concat(chunks);
    }
    async function body() {
      const text = (await raw(4 * 1024 * 1024)).toString("utf8");
      try {
        return JSON.parse(text);
      } catch {
        throw new ApiError(400, "invalid_json", "Expected a JSON body");
      }
    }
    try {
      const url = new URL(req.url!, "http://127.0.0.1");
      const path = url.pathname;
      const method = req.method;
      if (req.headers.origin !== undefined)
        throw new ApiError(
          403,
          "browser_origin_denied",
          "Use an authenticated application gateway",
        );
      if (path === "/health" && method === "GET")
        return send(200, { status: "ok" });
      const supplied = Buffer.from(req.headers.authorization || "");
      const credential = runtime.config.tokens.find((c) => {
        const expected = Buffer.from("Bearer " + c.token);
        return (
          expected.length === supplied.length &&
          timingSafeEqual(expected, supplied)
        );
      });
      if (!credential)
        throw new ApiError(
          401,
          "unauthorized",
          "A valid bearer credential is required",
        );
      let namespace = credential.namespace;
      const suppliedNamespace = req.headers["x-media-namespace"];
      if (suppliedNamespace !== undefined) {
        if (
          typeof suppliedNamespace !== "string" ||
          !/^[A-Za-z0-9_:.-]{1,200}$/.test(suppliedNamespace)
        )
          throw new ApiError(
            400,
            "invalid_namespace",
            "Invalid caller namespace",
          );
        if (
          !credential.allowed_namespaces.some(
            (n) =>
              n === suppliedNamespace ||
              (n.endsWith("*") && suppliedNamespace.startsWith(n.slice(0, -1))),
          )
        )
          throw new ApiError(
            403,
            "forbidden_namespace",
            "Credential cannot use this namespace",
          );
        namespace = suppliedNamespace;
      }
      const operator = () => {
        if (!credential.operator)
          throw new ApiError(
            403,
            "operator_required",
            "Operator permission is required",
          );
      };
      if (path === "/v1/runtime" && method === "GET") {
        const diagnostics = await runtime.doctor();
        return send(200, {
          ...diagnostics,
          api_version: "1",
          limits: {
            queue_capacity: runtime.config.queue_capacity,
            max_input_bytes: runtime.config.max_input_bytes,
            max_output_bytes: runtime.config.max_output_bytes,
          },
          protocols: [...PROTOCOLS],
        });
      }
      if (path === "/v1/catalog" && method === "GET") {
        operator();
        return send(
          200,
          await inspectCatalog(
            runtime.config.artifact_catalog,
            url.searchParams.get("handle") || undefined,
          ),
        );
      }
      if (path === "/v1/inputs" && method === "POST") {
        if (req.headers["content-type"] !== "image/png")
          throw new ApiError(
            400,
            "unsupported",
            "Upload raw PNG bytes with Content-Type image/png",
            "image",
          );
        const record = await runtime.storeInput(
          await raw(runtime.config.max_input_bytes),
          namespace,
        );
        return send(201, publicInput(record));
      }
      if (path === "/v1/inputs" && method === "GET")
        return send(200, runtime.store.inputs(namespace).map(publicInput));
      const inputMatch = path.match(/^\/v1\/inputs\/(input_[a-f0-9]{32})$/);
      if (inputMatch && method === "GET")
        return send(200, publicInput(runtime.ownedInput(inputMatch[1], namespace)));
      if (inputMatch && method === "DELETE") {
        await runtime.deleteInput(inputMatch[1], namespace);
        return send(200, { deleted: true });
      }
      if (path === "/v1/models" && method === "GET") {
        const models = await Promise.all(
          runtime.store.registry("models").map(async (model: any) => {
            let available = true;
            try {
              await access(
                runtime.config.artifact_catalog[model.artifact_handle],
              );
            } catch {
              available = false;
            }
            return {
              model_id: model.model_id,
              family: model.bundle.family,
              tasks: ["text-to-image", "image-to-image"],
              bundle_digest: model.bundle.bundle_digest,
              source: model.bundle.source,
              available,
            };
          }),
        );
        return send(200, models);
      }
      const modelMatch = path.match(/^\/v1\/models\/([a-zA-Z0-9_-]{1,80})$/);
      if (modelMatch && method === "GET") {
        const model = runtime.store.registry("models", modelMatch[1]);
        if (!model) throw new ApiError(404, "not_found", "Model not found");
        const detail: any = {
          model_id: model.model_id,
          artifact_handle: model.artifact_handle,
          family: model.bundle.family,
          bundle_digest: model.bundle.bundle_digest,
          source: model.bundle.source,
          components: model.bundle.components.map((c: any) => ({
            role: c.role,
            digest: c.digest,
            tensor_format: c.tensor_format,
            files: Object.keys(model.bundle.configuration.files).filter((f) =>
              f.startsWith(c.role + "/"),
            ).length,
          })),
          file_count: Object.keys(model.bundle.configuration.files).length,
        };
        if (url.searchParams.get("verify") === "1") {
          // Re-verify every byte through the worker and report the load estimate.
          operator();
          const { caps, protocol } = await runtime.worker.probe();
          try {
            const result = await runtime.worker.json("/v1/validate", "POST", {
              protocol,
              bundle: model.bundle,
              options: caps.load_defaults,
            });
            detail.verification = {
              valid: true,
              memory_estimate: result.memory_estimate ?? null,
              load_defaults: caps.load_defaults,
            };
          } catch (e) {
            if (!(e instanceof ApiError) || e.status >= 500) throw e;
            detail.verification = { valid: false, error: e.code };
          }
        }
        return send(200, detail);
      }
      if (path === "/v1/models" && method === "POST") {
        operator();
        const model = await buildBundle(
          await body(),
          runtime.config.artifact_catalog,
        );
        const { caps, protocol } = await runtime.worker.probe();
        const validation = await runtime.worker.json("/v1/validate", "POST", {
          protocol,
          bundle: model.bundle,
          options: caps.load_defaults,
        });
        runtime.store.register("models", model.model_id, model);
        return send(201, {
          model_id: model.model_id,
          bundle_digest: model.bundle.bundle_digest,
          memory_estimate: validation?.memory_estimate ?? null,
        });
      }
      if (path === "/v1/adapters" && method === "GET")
        return send(
          200,
          runtime.store.registry("adapters").map((a: any) => ({
            adapter_id: a.adapter_id,
            artifact_handle: a.artifact_handle,
            digest: a.digest,
            source: a.source,
            files: Object.keys(a.files).length,
          })),
        );
      if (path === "/v1/adapters" && method === "POST") {
        operator();
        const adapter = await buildAdapter(await body(), runtime.config.artifact_catalog);
        const { caps, protocol } = await runtime.worker.probe();
        if (!caps.adapters || protocol === PROTOCOLS[0])
          throw new ApiError(
            503,
            "unsupported",
            "The connected worker does not advertise LoRA adapters",
          );
        const validation = await runtime.worker.json("/v1/validate", "POST", {
          protocol,
          adapters: [{ adapter, scale: 1 }],
        });
        runtime.store.register("adapters", adapter.adapter_id, adapter);
        return send(201, {
          adapter_id: adapter.adapter_id,
          digest: adapter.digest,
          layout: validation?.adapters?.[0]?.layout ?? null,
        });
      }
      if (path === "/v1/recipes" && method === "GET")
        return send(200, runtime.store.registry("recipes"));
      if (path === "/v1/recipes" && method === "POST") {
        operator();
        const recipe = recipeSchema.parse(await body());
        const value = { ...recipe, digest: hash(recipe) };
        const existing = runtime.store.registry("recipes", recipe.recipe_id);
        if (
          existing &&
          existing.digest !== value.digest &&
          recipe.revision <= existing.revision
        )
          throw new ApiError(
            409,
            "recipe_revision_conflict",
            "Increase revision when changing a recipe",
          );
        runtime.store.register("recipes", recipe.recipe_id, value);
        return send(201, value);
      }
      if (path === "/v1/jobs/validate" && method === "POST") {
        const result = await runtime.resolve(await body(), namespace);
        return send(200, {
          valid: true,
          request: result.request,
          recipe_digest: result.recipe.digest,
          bundle_digest: result.model.bundle.bundle_digest,
        });
      }
      if (path === "/v1/jobs" && method === "POST") {
        const key = req.headers["idempotency-key"];
        if (Array.isArray(key))
          throw new ApiError(400, "invalid_idempotency_key", "Use one key");
        const job = await runtime.submit(await body(), namespace, key);
        return send(202, { job_id: job.job_id, state: job.state });
      }
      if (path === "/v1/jobs" && method === "GET") {
        const limit = Number(url.searchParams.get("limit") || 50),
          after = Number(url.searchParams.get("after") || 0);
        if (
          !Number.isInteger(limit) ||
          limit < 1 ||
          limit > 200 ||
          !Number.isInteger(after) ||
          after < 0
        )
          throw new ApiError(
            400,
            "invalid_pagination",
            "Use limit 1–200 and a nonnegative cursor",
          );
        const newest = url.searchParams.get("order") === "newest";
        const rows = runtime.store.db
          .prepare(
            newest
              ? "SELECT ordinal,data FROM jobs WHERE namespace=? ORDER BY ordinal DESC LIMIT ?"
              : "SELECT ordinal,data FROM jobs WHERE namespace=? AND ordinal>? ORDER BY ordinal LIMIT ?",
          )
          .all(...(newest ? [namespace, limit] : [namespace, after, limit]));
        return send(200, {
          jobs: rows.map((r: any) => publicJob(JSON.parse(r.data))),
          next_cursor:
            !newest && rows.length === limit ? rows.at(-1)!.ordinal : null,
        });
      }
      const match = path.match(
        /^\/v1\/jobs\/(job_[a-f0-9-]+)(?:\/(events|cancel|manifest|outputs)(?:\/(image))?)?$/,
      );
      if (!match) throw new ApiError(404, "not_found", "Endpoint not found");
      const [, job_id, action, output_id] = match;
      const job = runtime.owned(job_id, namespace);
      if (!action && method === "GET") return send(200, publicJob(job));
      if (action === "cancel" && method === "POST")
        return send(
          terminal(job.state) ? 200 : 202,
          publicJob(runtime.cancel(job_id, namespace)),
        );
      if (action === "manifest" && method === "GET") {
        if (!job.manifest)
          throw new ApiError(
            409,
            "manifest_not_ready",
            "Manifest is available after durable output publication",
          );
        return send(200, job.manifest);
      }
      if (action === "outputs" && method === "GET" && output_id) {
        const output = job.outputs.find((o) => o.output_id === output_id);
        if (!output) throw new ApiError(404, "not_found", "Output not found");
        const file = join(
          runtime.config.directory,
          "outputs",
          job_id,
          output_id + ".png",
        );
        if (
          (output.expires_at && Date.parse(output.expires_at) <= Date.now()) ||
          !existsSync(file)
        )
          throw new ApiError(
            410,
            "output_expired",
            "Output is no longer on local storage",
          );
        const metadata = await stat(file);
        res.writeHead(200, {
          "Content-Type": output.mime_type,
          "Content-Length": metadata.size,
          ETag: `"${output.sha256}"`,
        });
        const stream = createReadStream(file);
        stream.on("error", () => res.destroy());
        res.on("close", () => stream.destroy());
        stream.pipe(res);
        return;
      }
      if (action === "events" && method === "GET") {
        let cursor = Number(req.headers["last-event-id"] || 0);
        const events = runtime.store.events(job_id);
        const last = events.at(-1)?.sequence || 0;
        if (!Number.isInteger(cursor) || cursor < 0 || cursor > last)
          throw new ApiError(
            409,
            "resume_required",
            "Fetch a fresh job snapshot before resuming events",
          );
        res.writeHead(200, {
          "Content-Type": "text/event-stream",
          Connection: "keep-alive",
        });
        res.flushHeaders();
        streams.add(res);
        const poll = () => {
          try {
            for (const event of runtime.store.events(job_id, cursor)) {
              if (res.writableLength > 1024 * 1024) {
                res.destroy();
                return;
              }
              res.write(
                `id: ${event.sequence}\nevent: ${event.type}\ndata: ${JSON.stringify(event)}\n\n`,
              );
              cursor = event.sequence;
            }
            if (terminal(runtime.store.get(job_id)!.state)) res.end();
            else res.write(": heartbeat\n\n");
          } catch {
            res.destroy();
          }
        };
        const timer = setInterval(poll, 500);
        res.on("close", () => {
          clearInterval(timer);
          streams.delete(res);
        });
        poll();
        return;
      }
      throw new ApiError(404, "not_found", "Endpoint not found");
    } catch (error) {
      if (res.headersSent) {
        res.destroy();
        return;
      }
      const e =
        error instanceof ApiError
          ? error
          : error instanceof ZodError
            ? new ApiError(
                400,
                "invalid_request",
                "Invalid or unsupported request field",
                error.issues[0]?.path.join("."),
              )
            : new ApiError(
                500,
                "internal_error",
                "The request could not be completed",
              );
      if (e.status === 429) res.setHeader("Retry-After", "1");
      send(e.status, {
        error: {
          code: e.code,
          message: e.message,
          ...(e.field ? { field: e.field } : {}),
        },
        request_id,
      });
    }
  });
  server.requestTimeout = 30000;
  server.headersTimeout = 10000;
  const close = server.close.bind(server);
  server.close = ((callback?: any) => {
    for (const stream of streams) stream.end();
    return close(callback);
  }) as typeof server.close;
  return server;
}
export function publicInput(record: any) {
  const { namespace, ...rest } = record;
  return rest;
}
export function publicJob(job: any) {
  const { namespace, model, backend, recipe, manifest, input, adapters, ...rest } = job;
  return {
    ...rest,
    input: input ? publicInput(input) : undefined,
    adapters: (adapters ?? []).map((a: any) => ({
      adapter_id: a.adapter.adapter_id,
      digest: a.adapter.digest,
      scale: a.scale,
    })),
    model: {
      model_id: model.model_id,
      bundle_digest: model.bundle.bundle_digest,
    },
    recipe: {
      recipe_id: recipe.recipe_id,
      revision: recipe.revision,
      digest: recipe.digest,
    },
    backend,
  };
}
