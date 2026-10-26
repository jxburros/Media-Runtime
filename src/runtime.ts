import { randomUUID, createHash } from "node:crypto";
import {
  mkdir,
  open,
  rename,
  statfs,
  access,
  unlink,
  readFile,
} from "node:fs/promises";
import { constants } from "node:fs";
import { join } from "node:path";
import {
  ApiError,
  hash,
  seed,
  submission,
  parameters,
  loadOptions,
  VERSION,
  PROTOCOLS,
  terminal,
  effectiveSteps,
  newInputId,
  pngDimensions,
  type InputRecord,
  type Job,
} from "./contracts.js";
import { Store } from "./store.js";
import { WorkerClient } from "./worker.js";
import type { Config } from "./config.js";
export class Runtime {
  store: Store;
  worker: WorkerClient;
  timer?: NodeJS.Timeout;
  busy = false;
  stopping = false;
  constructor(public config: Config) {
    this.store = new Store(config.directory);
    this.worker = new WorkerClient(config);
  }
  async start() {
    await this.worker.start();
    await mkdir(join(this.config.directory, "inputs"), {
      recursive: true,
      mode: 0o700,
    });
    this.timer = setInterval(
      () => void this.tick(),
      this.config.poll_interval_ms,
    );
    await this.tick();
  }
  async stop() {
    this.stopping = true;
    if (this.timer) clearInterval(this.timer);
    while (this.busy) await new Promise((r) => setTimeout(r, 20));
    await this.worker.stop();
    this.store.db.close();
  }
  owned(job_id: string, namespace: string) {
    const job = this.store.get(job_id, namespace);
    if (!job) throw new ApiError(404, "not_found", "Job not found");
    return job;
  }
  inputPath(input_id: string) {
    return join(this.config.directory, "inputs", input_id + ".png");
  }
  expiry() {
    return this.config.output_retention_days
      ? new Date(
          Date.now() + this.config.output_retention_days * 86400000,
        ).toISOString()
      : null;
  }
  /** Durable, namespace-owned PNG input. The worker only ever receives bytes the runtime verified. */
  async storeInput(bytes: Buffer, namespace: string): Promise<InputRecord> {
    if (bytes.length > this.config.max_input_bytes)
      throw new ApiError(413, "input_too_large", "Input exceeds max_input_bytes");
    const { width, height } = pngDimensions(bytes);
    const record: InputRecord = {
      input_id: newInputId(),
      namespace,
      mime_type: "image/png",
      size: bytes.length,
      sha256: createHash("sha256").update(bytes).digest("hex"),
      width,
      height,
      created_at: new Date().toISOString(),
      expires_at: this.expiry(),
    };
    await this.atomicFile(this.inputPath(record.input_id), bytes);
    this.store.insertInput(record);
    return record;
  }
  ownedInput(input_id: string, namespace: string) {
    const record = this.store.input(input_id, namespace);
    if (!record) throw new ApiError(404, "not_found", "Input not found");
    return record;
  }
  async deleteInput(input_id: string, namespace: string) {
    this.ownedInput(input_id, namespace);
    if (
      this.store
        .all()
        .some((j) => !terminal(j.state) && j.input?.input_id === input_id)
    )
      throw new ApiError(
        409,
        "input_in_use",
        "An active job references this input",
      );
    await unlink(this.inputPath(input_id)).catch((e: any) => {
      if (e.code !== "ENOENT") throw e;
    });
    this.store.deleteInput(input_id);
  }
  async resolve(body: unknown, namespace: string) {
    const request = submission.parse(body);
    const model = this.store.registry("models", request.model_id);
    const recipe = this.store.registry("recipes", request.recipe_id);
    if (!model)
      throw new ApiError(
        400,
        "unknown_model",
        "Register the selected model before submitting",
        "model_id",
      );
    if (!recipe)
      throw new ApiError(
        400,
        "unknown_recipe",
        "Recipe not found",
        "recipe_id",
      );
    if (request.retry_of) this.owned(request.retry_of, namespace);
    const task: string = recipe.pipeline;
    const { info, caps, protocol } = await this.worker.probe();
    if (!caps.ready || !info.service_ready)
      throw new ApiError(
        503,
        "backend_unavailable",
        "Worker dependencies or selected device are not ready",
      );
    if (request.backend_id && request.backend_id !== caps.backend_id)
      throw new ApiError(
        400,
        "unsupported_backend",
        "Selected backend is not available",
        "backend_id",
      );
    const tasks: string[] = Array.isArray(caps.tasks)
      ? caps.tasks
      : ["text-to-image"];
    if (
      !tasks.includes(task) ||
      (task !== "text-to-image" && protocol === PROTOCOLS[0])
    )
      throw new ApiError(
        400,
        "unsupported",
        `The connected worker does not advertise ${task}`,
        "recipe_id",
      );
    let input: InputRecord | undefined;
    const imageDefaults: Record<string, number> = {};
    if (task === "image-to-image") {
      if (!request.inputs.image)
        throw new ApiError(
          400,
          "invalid_request",
          "This recipe requires an uploaded image input",
          "inputs.image",
        );
      input = this.ownedInput(request.inputs.image, namespace);
      if (input.expires_at && Date.parse(input.expires_at) <= Date.now())
        throw new ApiError(
          410,
          "input_expired",
          "Input is no longer on local storage",
          "inputs.image",
        );
      imageDefaults.width = input.width;
      imageDefaults.height = input.height;
    } else if (request.inputs.image || request.parameters.strength !== undefined)
      throw new ApiError(
        400,
        "invalid_request",
        "Image and strength apply to image-to-image recipes only",
        request.inputs.image ? "inputs.image" : "parameters.strength",
      );
    const effective = parameters.parse({
      ...caps.defaults,
      ...(task === "image-to-image" ? caps.task_defaults?.[task] : {}),
      ...recipe.defaults,
      ...imageDefaults,
      ...request.parameters,
      seed: request.parameters.seed ?? recipe.defaults.seed ?? seed(),
    });
    if (input) {
      if (effective.width !== input.width || effective.height !== input.height)
        throw new ApiError(
          400,
          "invalid_request",
          "Width and height must equal the uploaded image size; nothing is resized",
          effective.width !== input.width ? "parameters.width" : "parameters.height",
        );
      if (effective.strength === undefined)
        throw new ApiError(
          400,
          "invalid_request",
          "Strength is required for image-to-image",
          "parameters.strength",
        );
      if (effectiveSteps(effective) < 1)
        throw new ApiError(
          400,
          "invalid_request",
          "steps × strength must denoise for at least one step",
          "parameters.strength",
        );
    }
    const uses: { adapter_id: string; scale: number }[] =
      request.adapters ?? recipe.adapters ?? [];
    const adapters = uses.map((use) => {
      const adapter = this.store.registry("adapters", use.adapter_id);
      if (!adapter)
        throw new ApiError(
          400,
          "unknown_adapter",
          `Register adapter ${use.adapter_id} before using it`,
          "adapters",
        );
      return { adapter, scale: use.scale };
    });
    if (new Set(uses.map((u) => u.adapter_id)).size !== uses.length)
      throw new ApiError(400, "invalid_request", "Adapters must be distinct", "adapters");
    if (adapters.length && (!caps.adapters || protocol === PROTOCOLS[0]))
      throw new ApiError(
        400,
        "unsupported",
        "The connected worker does not advertise LoRA adapters",
        "adapters",
      );
    if (adapters.length > (caps.adapters?.max_adapters ?? 0))
      throw new ApiError(400, "unsupported", "Too many adapters for this worker", "adapters");
    const backendId: string = caps.backend_id;
    const options = loadOptions.parse({
      ...caps.load_defaults,
      ...(request.backend_options as any)?.[backendId],
    });
    for (const other of Object.keys(request.backend_options ?? {}))
      if (other !== backendId && (request.backend_options as any)[other])
        throw new ApiError(
          400,
          "unsupported_backend",
          `Options for ${other} do not apply to the ${backendId} worker`,
          "backend_options",
        );
    if (!caps.precisions.includes(options.precision))
      throw new ApiError(
        400,
        "unsupported",
        "Requested precision is unavailable",
        "precision",
      );
    const validation = await this.worker.json("/v1/validate", "POST", {
      protocol,
      bundle: model.bundle,
      options,
      ...(adapters.length ? { adapters } : {}),
    });
    return {
      task,
      request: {
        ...request,
        parameters: effective,
        adapters: uses,
        backend_id: backendId,
        backend_options: { [backendId]: options },
      },
      input,
      adapters,
      model,
      recipe,
      backend: {
        backend_id: backendId,
        ownership: caps.ownership ?? "reference",
        worker_version: caps.worker_version,
        protocol,
        device: caps.device,
        options,
        memory_budget_mb: caps.memory_budget_mb,
        memory_estimate: validation?.memory_estimate ?? null,
      },
    };
  }
  async submit(body: unknown, namespace: string, key?: string) {
    const parsed = submission.parse(body);
    const digest = hash(parsed);
    if (key !== undefined && (!key.length || key.length > 200))
      throw new ApiError(
        400,
        "invalid_idempotency_key",
        "Idempotency-Key must contain 1–200 characters",
      );
    const existing = () => {
      if (key === undefined) return;
      const row = this.store.db
        .prepare("SELECT * FROM idempotency WHERE namespace=? AND key=?")
        .get(namespace, key);
      if (!row) return;
      if (row.digest !== digest)
        throw new ApiError(
          409,
          "idempotency_conflict",
          "Idempotency key was already used for a different request",
        );
      return this.store.get(row.job_id as string)!;
    };
    const retry = existing();
    if (retry) return retry;
    const resolved = await this.resolve(parsed, namespace);
    return this.store.transaction(() => {
      const raced = existing();
      if (raced) return raced;
      if (this.store.all("queued").length >= this.config.queue_capacity)
        throw new ApiError(429, "queue_full", "Queue is full; retry later");
      const now = new Date().toISOString();
      const job: Job = {
        job_id: "job_" + randomUUID(),
        namespace,
        state: "queued",
        stage: null,
        cancel_requested: false,
        created_at: now,
        updated_at: now,
        ...resolved,
        worker_sequence: 0,
        outputs: [],
        retry_of: parsed.retry_of,
      };
      this.store.insert(job);
      this.store.event(job, "job_state", { state: "queued" });
      if (key !== undefined)
        this.store.db
          .prepare("INSERT INTO idempotency VALUES (?,?,?,?)")
          .run(namespace, key, digest, job.job_id);
      return job;
    });
  }
  cancel(job_id: string, namespace: string) {
    const job = this.owned(job_id, namespace);
    if (terminal(job.state)) return job;
    job.cancel_requested = true;
    job.cancel_at ??= Date.now();
    this.store.transaction(() => {
      this.store.save(job);
      this.store.event(job, "job_state", {
        state: job.state,
        cancel_requested: true,
      });
    });
    if (job.state === "queued") this.store.finish(job, "cancelled");
    return this.store.get(job_id)!;
  }
  async tick() {
    if (this.busy || this.stopping) return;
    this.busy = true;
    try {
      if (
        this.worker.child &&
        (this.worker.child.exitCode !== null ||
          this.worker.child.signalCode !== null)
      ) {
        const interrupted = this.store.all("running")[0];
        if (interrupted)
          this.store.finish(interrupted, "failed", {
            code: "execution_interrupted",
            message: "Owned worker exited; this job was not regenerated",
          });
        this.worker.child = undefined;
        await this.worker.start();
      }
      const { info, caps, protocol } = await this.worker.probe();
      // Release only after durable terminal commit. Retry collection acknowledgements after disconnects.
      for (const done of this.store
        .all()
        .filter(
          (j) =>
            terminal(j.state) &&
            !j.released &&
            (j.execution_id || j.worker_input_id) &&
            j.session_id === info.session_id,
        )) {
        try {
          if (done.execution_id)
            await this.worker.json(
              `/v1/executions/${done.execution_id}`,
              "DELETE",
            );
          if (done.worker_input_id)
            await this.worker
              .json(`/v1/inputs/${done.worker_input_id}`, "DELETE")
              .catch((e) => {
                if (!(e instanceof ApiError) || e.status !== 404) throw e;
              });
          done.released = true;
          this.store.save(done);
        } catch (e) {
          if (e instanceof ApiError && e.status === 404) {
            done.released = true;
            this.store.save(done);
          }
        }
      }
      let job = this.store.all("running")[0];
      if (!job) {
        if (info.state !== "ready" || !caps.ready) return;
        job = this.store.all("queued")[0];
        if (!job) return;
        this.store.transaction(() => {
          job.state = "running";
          job.backend.worker_version = caps.worker_version;
          job.stage = null;
          job.attempt_id = "attempt_" + randomUUID();
          job.session_id = info.session_id;
          job.dispatch_phase = "loading";
          this.store.save(job);
          this.store.event(job, "job_state", { state: "running" });
        });
      }
      if (job.session_id !== info.session_id) {
        this.store.finish(job, "failed", {
          code: "execution_interrupted",
          message: "Worker session ended; this job was not regenerated",
        });
        return;
      }
      // Reload cancellation intent after every asynchronous boundary before writing the job.
      const refresh = () => {
        const latest = this.store.get(job.job_id)!;
        job.cancel_requested = latest.cancel_requested;
        job.cancel_at = latest.cancel_at;
      };
      refresh();
      if (
        job.cancel_requested &&
        Date.now() - (job.cancel_at || Date.now()) >
          this.config.cancellation_grace_ms &&
        this.worker.child
      ) {
        await this.worker.stop();
        refresh();
        this.store.finish(job, "failed", {
          code: "execution_interrupted",
          message:
            "Owned worker was forcibly stopped after the cancellation deadline",
          forced_termination: true,
        });
        await this.worker.start();
        return;
      }
      if (job.dispatch_phase === "loading") {
        if (job.load_operation_id) {
          const operation = await this.worker.json(
            `/v1/operations/${job.load_operation_id}`,
          );
          refresh();
          if (operation.state === "running") return;
          if (operation.state === "failed") {
            await this.worker.json(
              `/v1/operations/${job.load_operation_id}`,
              "DELETE",
            );
            job.load_operation_id = undefined;
            this.store.finish(job, "failed", {
              code: operation.error?.code || "model_load_failed",
              message: "Selected model could not be loaded",
            });
            return;
          }
          await this.worker.json(
            `/v1/operations/${job.load_operation_id}`,
            "DELETE",
          );
          job.load_operation_id = undefined;
          if (operation.memory_estimate)
            job.backend.memory_estimate = operation.memory_estimate;
          this.store.save(job);
          return;
        }
        if (info.state === "loading" || info.state === "unloading") return;
        refresh();
        if (job.cancel_requested) {
          this.store.finish(job, "cancelled");
          return;
        }
        const desired = job.request.backend_options[job.backend.backend_id];
        const identity = (job.adapters ?? []).map((a) => ({
          adapter_id: a.adapter.adapter_id,
          digest: a.adapter.digest,
          scale: a.scale,
        }));
        const loadedIdentity = (info.loaded_bundle?.adapters ?? []).map(
          (a: any) => ({ adapter_id: a.adapter_id, digest: a.digest, scale: a.scale }),
        );
        if (
          info.loaded_bundle?.bundle_digest !==
            job.model.bundle.bundle_digest ||
          hash(info.loaded_bundle.options) !== hash(desired) ||
          hash(loadedIdentity) !== hash(identity)
        ) {
          const op = await this.worker.json("/v1/model/load", "POST", {
            protocol,
            bundle: job.model.bundle,
            options: desired,
            ...(identity.length ? { adapters: job.adapters } : {}),
          });
          refresh();
          job.load_operation_id = op.operation_id;
          job.stage = "loading";
          this.store.transaction(() => {
            this.store.save(job);
            this.store.event(job, "stage", { stage: "loading" });
          });
          return;
        }
        job.dispatch_phase = "executing";
        refresh();
        this.store.save(job);
      }
      let snapshot: any;
      if (!job.execution_id) {
        // Query the same session before resending an ambiguous acceptance. A retry uses the SAME attempt.
        try {
          snapshot = await this.worker.json(
            `/v1/executions?attempt_id=${encodeURIComponent(job.attempt_id!)}`,
          );
        } catch (e) {
          if (!(e instanceof ApiError) || e.status !== 404) throw e;
          refresh();
          if (job.cancel_requested) {
            this.store.finish(job, "cancelled");
            return;
          }
          const inputs: Record<string, string> = { ...job.request.inputs };
          if (job.task === "image-to-image") {
            if (!job.worker_input_id) {
              // Hand the worker the verified bytes under a session-scoped handle, never a path.
              const bytes = await readFile(this.inputPath(job.input!.input_id));
              if (
                createHash("sha256").update(bytes).digest("hex") !==
                job.input!.sha256
              )
                throw new ApiError(
                  410,
                  "input_unavailable",
                  "Input bytes changed or disappeared before dispatch",
                );
              const uploaded = await this.worker.json(
                "/v1/inputs",
                "POST",
                bytes,
                "image/png",
              );
              if (uploaded.sha256 !== job.input!.sha256)
                throw new ApiError(
                  503,
                  "protocol_mismatch",
                  "Worker input digest mismatch",
                );
              refresh();
              job.worker_input_id = uploaded.input_id;
              this.store.save(job);
            }
            inputs.image = job.worker_input_id!;
          }
          snapshot = await this.worker.json("/v1/executions", "POST", {
            protocol,
            attempt_id: job.attempt_id,
            bundle_digest: job.model.bundle.bundle_digest,
            task: job.task,
            inputs,
            parameters: job.request.parameters,
          });
        }
        if (
          snapshot.session_id !== job.session_id ||
          snapshot.attempt_id !== job.attempt_id
        )
          throw new ApiError(
            503,
            "protocol_mismatch",
            "Worker execution identity mismatch",
          );
        refresh();
        job.execution_id = snapshot.execution_id;
        this.store.save(job);
      } else
        snapshot = await this.worker.json(`/v1/executions/${job.execution_id}`);
      refresh();
      if (job.cancel_requested && !terminal(snapshot.state))
        snapshot = await this.worker.json(
          `/v1/executions/${job.execution_id}/cancel`,
          "POST",
          {},
        );
      refresh();
      this.store.transaction(() => {
        for (const event of snapshot.events) {
          if (event.sequence <= job.worker_sequence) continue;
          if (event.sequence !== job.worker_sequence + 1)
            throw new ApiError(
              503,
              "event_gap",
              "Worker event history has a gap",
            );
          if (event.type === "stage") {
            job.stage = event.data.stage;
            job.progress = undefined;
            this.store.event(job, "stage", event.data);
          }
          if (event.type === "progress") {
            job.progress = event.data;
            this.store.event(job, "progress", event.data);
          }
          job.worker_sequence = event.sequence;
        }
        this.store.save(job);
      });
      if (snapshot.state === "succeeded") {
        await this.commitOutputs(job, snapshot);
        return;
      }
      if (snapshot.state === "failed")
        this.store.finish(job, "failed", {
          code: snapshot.error?.code || "execution_failed",
          message: "Worker execution failed",
          restart_required: !!snapshot.error?.restart_required,
        });
      if (snapshot.state === "cancelled") this.store.finish(job, "cancelled");
    } catch (e) {
      const job = this.store.all("running")[0];
      // Transient transport errors preserve dispatch identity for reconciliation; never regenerate.
      if (job && e instanceof ApiError && e.code !== "backend_unavailable") {
        this.store.finish(job, "failed", {
          code: e.status === 404 ? "execution_interrupted" : e.code,
          message:
            e.status === 404
              ? "Worker record is no longer available"
              : e.message,
        });
      }
    } finally {
      this.busy = false;
    }
  }
  async atomicFile(path: string, data: Uint8Array | string) {
    const temp = path + ".tmp";
    const file = await open(temp, "w", 0o600);
    try {
      await file.writeFile(data);
      await file.sync();
    } finally {
      await file.close();
    }
    await rename(temp, path);
  }
  async commitOutputs(job: Job, snapshot: any) {
    const directory = join(this.config.directory, "outputs", job.job_id);
    await mkdir(directory, { recursive: true, mode: 0o700 });
    try {
      if (snapshot.outputs.length !== 1) throw new Error("invalid outputs");
      const output = snapshot.outputs[0];
      if (
        output.output_id !== "image" ||
        output.mime_type !== "image/png" ||
        !Number.isInteger(output.size) ||
        output.size < 1 ||
        output.size > this.config.max_output_bytes
      )
        throw new Error("invalid output");
      const response = await this.worker.response(
        `/v1/executions/${job.execution_id}/outputs/image`,
      );
      const chunks: Uint8Array[] = [];
      let size = 0;
      for await (const chunk of response.body! as any) {
        size += chunk.length;
        if (size > this.config.max_output_bytes)
          throw new Error("output too large");
        chunks.push(chunk);
      }
      const bytes = Buffer.concat(chunks);
      const digest = createHash("sha256").update(bytes).digest("hex");
      if (
        size !== output.size ||
        digest !== output.sha256 ||
        bytes.subarray(0, 8).toString("hex") !== "89504e470d0a1a0a" ||
        bytes.length < 24 ||
        bytes.readUInt32BE(16) !== output.width ||
        bytes.readUInt32BE(20) !== output.height
      )
        throw new Error("output integrity failure");
      if (
        output.width !== job.request.parameters.width ||
        output.height !== job.request.parameters.height
      )
        throw new Error("output dimensions changed");
      await this.atomicFile(join(directory, "image.png"), bytes);
      job.outputs = [{ ...output, expires_at: this.expiry() }];
      const manifest = {
        schema_version: 2,
        runtime_version: VERSION,
        job_id: job.job_id,
        task: job.task,
        recipe: job.recipe,
        model: job.model.bundle,
        request: job.request,
        inputs: job.input
          ? [
              {
                input_id: job.input.input_id,
                mime_type: job.input.mime_type,
                size: job.input.size,
                sha256: job.input.sha256,
                width: job.input.width,
                height: job.input.height,
              },
            ]
          : [],
        adapters: (job.adapters ?? []).map((a) => ({
          adapter_id: a.adapter.adapter_id,
          digest: a.adapter.digest,
          scale: a.scale,
          source: a.adapter.source,
        })),
        backend: job.backend,
        worker: snapshot.implementation,
        effective_settings: snapshot.effective_settings,
        measurements: snapshot.measurements,
        attempt_id: job.attempt_id,
        session_id: job.session_id,
        execution_id: job.execution_id,
        outputs: job.outputs,
        determinism:
          "Seed reproducibility depends on hardware, software and dependency versions; matching bytes are not guaranteed.",
      };
      job.manifest = manifest;
      await this.atomicFile(
        join(directory, "manifest.json"),
        JSON.stringify(manifest, null, 2),
      );
      for (const path of [
        directory,
        join(this.config.directory, "outputs"),
        this.config.directory,
      ]) {
        const dir = await open(path, "r");
        try {
          await dir.sync();
        } finally {
          await dir.close();
        }
      }
      const latest = this.store.get(job.job_id)!;
      job.cancel_requested = latest.cancel_requested;
      job.cancel_at = latest.cancel_at;
      this.store.transaction(() => {
        job.state = "succeeded";
        job.stage = null;
        this.store.save(job);
        this.store.event(job, "output", job.outputs[0]);
        this.store.event(job, "job_state", { state: "succeeded" });
      });
    } catch (e) {
      if (e instanceof ApiError && e.code === "backend_unavailable") throw e;
      this.store.finish(job, "failed", {
        code: "output_unavailable",
        message:
          "Output could not be verified and committed to durable storage",
      });
    }
  }
  async doctor() {
    let worker: any;
    try {
      const { info, caps, protocol } = await this.worker.probe();
      worker = {
        protocol,
        supported_protocols: info.supported_protocols ?? [info.protocol],
        worker_version: info.worker_version,
        backend_id: caps.backend_id,
        ownership: caps.ownership ?? "reference",
        owned_process: !!this.config.worker.command,
        session_id: info.session_id,
        state: info.state,
        service_ready: info.service_ready,
        device: caps.device,
        device_ready: caps.ready,
        missing_dependencies: caps.missing_dependencies ?? [],
        dependencies: caps.dependencies ?? {},
        tasks: caps.tasks ?? ["text-to-image"],
        adapters: caps.adapters ?? null,
        precisions: caps.precisions,
        load_defaults: caps.load_defaults,
        memory_budget_mb: caps.memory_budget_mb ?? null,
        memory_estimate: caps.memory_estimate ?? null,
        loaded_bundle_digest: info.loaded_bundle?.bundle_digest ?? null,
      };
    } catch (e) {
      worker = {
        error: e instanceof ApiError ? e.code : "backend_unavailable",
        message: e instanceof ApiError ? e.message : "Worker is unavailable",
      };
    }
    const disk = await statfs(this.config.directory);
    let writable = true;
    try {
      await access(this.config.directory, constants.W_OK);
    } catch {
      writable = false;
    }
    const models = await Promise.all(
      this.store.registry("models").map(async (model: any) => {
        const missing_roles = new Set<string>();
        const root = this.config.artifact_catalog[model.artifact_handle];
        for (const relative of Object.keys(model.bundle.configuration.files)) {
          try {
            if (!root) throw new Error();
            await access(join(root, relative));
          } catch {
            missing_roles.add(relative.split("/")[0]);
          }
        }
        return {
          model_id: model.model_id,
          artifact_handle: model.artifact_handle,
          bundle_digest: model.bundle.bundle_digest,
          available: missing_roles.size === 0,
          missing_roles: [...missing_roles],
          loaded: worker.loaded_bundle_digest === model.bundle.bundle_digest,
        };
      }),
    );
    const adapters = await Promise.all(
      this.store.registry("adapters").map(async (adapter: any) => {
        const root = this.config.artifact_catalog[adapter.artifact_handle];
        let available = true;
        for (const relative of Object.keys(adapter.files)) {
          try {
            if (!root) throw new Error();
            await access(join(root, relative));
          } catch {
            available = false;
          }
        }
        return { adapter_id: adapter.adapter_id, digest: adapter.digest, available };
      }),
    );
    const jobs = this.store.all();
    return {
      runtime_version: VERSION,
      protocols: [...PROTOCOLS],
      worker,
      models,
      adapters,
      recipes: this.store
        .registry("recipes")
        .map((r: any) => ({ recipe_id: r.recipe_id, pipeline: r.pipeline })),
      storage: {
        writable,
        available_bytes: Number(disk.bavail) * Number(disk.bsize),
        output_retention_days: this.config.output_retention_days,
      },
      queue: {
        capacity: this.config.queue_capacity,
        queued: jobs.filter((j) => j.state === "queued").length,
        running: jobs.filter((j) => j.state === "running").length,
      },
      queue_capacity: this.config.queue_capacity,
    };
  }
  async cleanup() {
    for (const job of this.store.all("succeeded"))
      for (const output of job.outputs) {
        if (output.expires_at && Date.parse(output.expires_at) <= Date.now())
          await unlink(
            join(
              this.config.directory,
              "outputs",
              job.job_id,
              output.output_id + ".png",
            ),
          ).catch((e: any) => {
            if (e.code !== "ENOENT") throw e;
          });
      }
    for (const input of this.store.inputs())
      if (
        input.expires_at &&
        Date.parse(input.expires_at) <= Date.now() &&
        !this.store
          .all()
          .some((j) => !terminal(j.state) && j.input?.input_id === input.input_id)
      )
        await unlink(this.inputPath(input.input_id)).catch((e: any) => {
          if (e.code !== "ENOENT") throw e;
        });
  }
}
