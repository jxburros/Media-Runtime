#!/usr/bin/env node
import { parseArgs } from "node:util";
import { readFile, mkdir, writeFile, open, unlink } from "node:fs/promises";
import { resolve, join } from "node:path";
import { MediaRuntimeClient } from "./client.js";
import { readConfig } from "./config.js";
import { Runtime } from "./runtime.js";
import { createRuntimeServer } from "./server.js";
import { VERSION, terminal } from "./contracts.js";
import { runDashboard } from "./tui.js";
process.umask(0o077);
const help = `Media Runtime ${VERSION} (Node 24)

media-runtime serve --config runtime.config.json
media-runtime doctor [--json]
media-runtime models inspect [HANDLE] | models register model.json | models list | models show MODEL [--verify]
media-runtime recipes register recipe.json | recipes list | recipes inspect portrait
media-runtime inputs upload image.png | inputs list | inputs delete INPUT
media-runtime run RECIPE --model MODEL --prompt TEXT [--image FILE|INPUT] [--strength S] [--seed N] [--wait] [--json]
media-runtime jobs list | jobs watch JOB | jobs cancel JOB | jobs retry JOB
media-runtime outputs export JOB --directory DIR
media-runtime tui

Client configuration: MEDIA_RUNTIME_URL, MEDIA_RUNTIME_TOKEN, MEDIA_RUNTIME_NAMESPACE.
Exit codes: 0 success, 2 invalid input, 3 unavailable service, 4 failed/cancelled waited job.
`;
export function explainDoctor(report: any): { lines: string[]; healthy: boolean } {
  const lines: string[] = [];
  let healthy = true;
  const w = report.worker ?? {};
  lines.push(`Media Runtime ${report.runtime_version}; protocols ${report.protocols.join(", ")}`);
  if (w.error) {
    healthy = false;
    lines.push(`Worker: UNAVAILABLE (${w.error}) — ${w.message ?? ""}`);
    lines.push("  Start Media Engine (or set worker.command) and check its token and port.");
  } else {
    lines.push(
      `Worker: ${w.backend_id} ${w.worker_version} (${w.ownership} stage), negotiated ${w.protocol}, ${w.owned_process ? "owned child process" : "externally managed"}`,
    );
    lines.push(
      `  Device ${w.device}: ${w.device_ready ? "ready" : "NOT READY"}; service state ${w.state}; tasks ${(w.tasks ?? []).join(", ")}`,
    );
    if (!w.device_ready) healthy = false;
    if (w.missing_dependencies?.length) {
      healthy = false;
      lines.push(`  Missing worker dependencies: ${w.missing_dependencies.join(", ")} — install the inference extra in Engine's environment.`);
    }
    const deps = Object.entries(w.dependencies ?? {})
      .map(([k, v]) => `${k}=${v ?? "missing"}`)
      .join(" ");
    if (deps) lines.push(`  Dependencies: ${deps}`);
    lines.push(
      `  Load defaults: ${JSON.stringify(w.load_defaults)}; precisions ${(w.precisions ?? []).join("/")}; memory budget ${w.memory_budget_mb ?? "not set"} MiB`,
    );
    const estimate = w.memory_estimate;
    lines.push(
      estimate?.value_mb != null
        ? `  Memory estimate: ${estimate.value_mb} MiB parameters at ${estimate.precision} (source: ${estimate.source}; ${estimate.uncertainty})`
        : `  Memory estimate: unknown (${estimate?.uncertainty ?? "no bundle validated yet"}); run "models show MODEL --verify" for a per-model estimate.`,
    );
  }
  if (!report.models?.length) {
    lines.push("Models: none registered — run \"models inspect\" then \"models register\".");
    healthy = false;
  }
  for (const m of report.models ?? []) {
    if (!m.available) healthy = false;
    lines.push(
      `Model ${m.model_id} (${m.artifact_handle}): ${m.available ? "all registered files present" : `MISSING ${m.missing_roles.join(", ")}`}${m.loaded ? "; loaded in the worker" : ""}`,
    );
  }
  lines.push(`Recipes: ${(report.recipes ?? []).map((r: any) => `${r.recipe_id} (${r.pipeline})`).join(", ")}`);
  const s = report.storage ?? {};
  if (!s.writable) healthy = false;
  lines.push(
    `Storage: ${s.writable ? "writable" : "NOT WRITABLE"}, ${((s.available_bytes ?? 0) / 1024 ** 3).toFixed(1)} GiB free; output retention ${s.output_retention_days ?? "unlimited"}${s.output_retention_days ? " days" : ""}`,
  );
  const q = report.queue ?? {};
  lines.push(`Queue: ${q.running ?? 0} running, ${q.queued ?? 0} queued, capacity ${q.capacity ?? report.queue_capacity}`);
  lines.push(healthy ? "Result: ready to generate." : "Result: attention needed (see lines marked NOT/MISSING/UNAVAILABLE).");
  return { lines, healthy };
}
export function explainCatalog(entries: any[]): string[] {
  const lines: string[] = [];
  for (const e of entries) {
    lines.push(
      `${e.artifact_handle}: ${e.complete ? "complete SDXL base bundle" : "NOT READY"}${e.present ? ` (${e.file_count} files, ${(e.total_bytes / 1024 ** 3).toFixed(2)} GiB)` : ""}`,
    );
    for (const problem of e.problems) lines.push(`  - ${problem}`);
    if (e.complete)
      lines.push(
        `  register with: media-runtime models register <file containing ${JSON.stringify(e.registration)}>`,
      );
  }
  if (!entries.length) lines.push("No artifact handles are configured in artifact_catalog.");
  return lines;
}
async function main() {
  const { values, positionals: args } = parseArgs({
    allowPositionals: true,
    options: {
      config: { type: "string" },
      model: { type: "string" },
      prompt: { type: "string" },
      "negative-prompt": { type: "string" },
      image: { type: "string" },
      strength: { type: "string" },
      seed: { type: "string" },
      width: { type: "string" },
      height: { type: "string" },
      steps: { type: "string" },
      guidance: { type: "string" },
      precision: { type: "string" },
      backend: { type: "string" },
      wait: { type: "boolean" },
      json: { type: "boolean" },
      verify: { type: "boolean" },
      directory: { type: "string" },
      help: { type: "boolean" },
      version: { type: "boolean" },
      "idempotency-key": { type: "string" },
    },
  });
  if (values.version) {
    console.log(VERSION);
    return;
  }
  if (values.help || !args.length) {
    console.log(help);
    return;
  }
  if (args[0] === "serve") {
    const config = readConfig(values.config);
    await mkdir(config.directory, { recursive: true, mode: 0o700 });
    const lockPath = join(config.directory, "service.lock");
    try {
      const lock = await open(lockPath, "wx", 0o600);
      await lock.writeFile(String(process.pid));
      await lock.close();
    } catch (e: any) {
      if (e.code !== "EEXIST") throw e;
      let live = true;
      const pid = Number(await readFile(lockPath, "utf8"));
      try {
        process.kill(pid, 0);
      } catch (error: any) {
        if (error.code === "ESRCH") live = false;
      }
      if (live) throw new Error("Another runtime owns this state directory");
      await unlink(lockPath);
      const lock = await open(lockPath, "wx", 0o600);
      await lock.writeFile(String(process.pid));
      await lock.close();
    }
    const runtime = new Runtime(config);
    const server = createRuntimeServer(runtime);
    let closing = false;
    const close = async () => {
      if (closing) return;
      closing = true;
      server.closeAllConnections();
      await new Promise<void>((r) => server.close(() => r()));
      await runtime.stop();
      await unlink(lockPath).catch(() => {});
    };
    try {
      await runtime.start();
      await new Promise<void>((r, j) => {
        server.once("error", j);
        server.listen(config.port, "127.0.0.1", r);
      });
    } catch (e) {
      await close();
      throw e;
    }
    console.log(
      JSON.stringify({
        service: "media-runtime",
        version: VERSION,
        port: (server.address() as any).port,
      }),
    );
    const cleanup = setInterval(
      () => void runtime.cleanup().catch(() => {}),
      60000,
    );
    for (const signal of ["SIGINT", "SIGTERM"] as const)
      process.on(signal, () => {
        clearInterval(cleanup);
        void close();
      });
    return;
  }
  const token = process.env.MEDIA_RUNTIME_TOKEN;
  if (!token) throw new Error("Set MEDIA_RUNTIME_TOKEN");
  const client = new MediaRuntimeClient(
    process.env.MEDIA_RUNTIME_URL || "http://127.0.0.1:7810",
    token,
    process.env.MEDIA_RUNTIME_NAMESPACE,
  );
  const print = (value: unknown) =>
    console.log(JSON.stringify(value, null, values.json ? undefined : 2));
  async function watch(job_id: string) {
    let job: any;
    let prior = "";
    do {
      job = await client.status(job_id);
      const stamp = JSON.stringify([
        job.state,
        job.stage,
        job.cancel_requested,
        job.progress?.completed_steps,
      ]);
      if (stamp !== prior) {
        print(job);
        prior = stamp;
      }
      if (!terminal(job.state)) await new Promise((r) => setTimeout(r, 500));
    } while (!terminal(job.state));
    if (job.state !== "succeeded") process.exitCode = 4;
    return job;
  }
  if (args[0] === "doctor") {
    const report = await client.health();
    if (values.json) {
      print(report);
      return;
    }
    const { lines, healthy } = explainDoctor(report);
    console.log(lines.join("\n"));
    if (!healthy) process.exitCode = 3;
    return;
  }
  if (args[0] === "tui") {
    await runDashboard(client, { stdin: process.stdin, stdout: process.stdout });
    return;
  }
  if (args[0] === "models" && args[1] === "inspect") {
    const entries = await client.catalog(args[2]);
    if (values.json) print(entries);
    else console.log(explainCatalog(entries).join("\n"));
    if (!entries.every((e: any) => e.complete)) process.exitCode = 2;
    return;
  }
  if (args[0] === "models" && args[1] === "show" && args[2]) {
    print(await client.model(args[2], !!values.verify));
    return;
  }
  if (["models", "recipes"].includes(args[0])) {
    if (args[1] === "register" && args[2]) {
      print(
        await client.json(
          `/v1/${args[0]}`,
          "POST",
          JSON.parse(await readFile(args[2], "utf8")),
        ),
      );
      return;
    }
    const list = await client.json(`/v1/${args[0]}`);
    if (args[1] === "list") {
      print(list);
      return;
    }
    if (args[0] === "recipes" && args[1] === "inspect") {
      const recipe = list.find((r: any) => r.recipe_id === args[2]);
      if (!recipe) throw new Error("Recipe not found");
      print(recipe);
      return;
    }
  }
  if (args[0] === "inputs") {
    if (args[1] === "upload" && args[2]) {
      print(await client.uploadInput(await readFile(args[2])));
      return;
    }
    if (args[1] === "list") {
      print(await client.inputs());
      return;
    }
    if (args[1] === "delete" && args[2]) {
      print(await client.deleteInput(args[2]));
      return;
    }
  }
  if (args[0] === "run" && args[1]) {
    const parameters: Record<string, number> = {};
    for (const field of [
      "seed",
      "width",
      "height",
      "steps",
      "guidance",
      "strength",
    ] as const)
      if (values[field] !== undefined)
        parameters[field] = Number(values[field]);
    let image = values.image;
    if (image && !/^input_[a-f0-9]{32}$/.test(image)) {
      // A local file is uploaded first; the service never receives a client path.
      const record = await client.uploadInput(await readFile(image));
      image = record.input_id;
      if (!values.json) console.error(JSON.stringify({ uploaded: record }));
    }
    const accepted = await client.submit(
      {
        recipe_id: args[1],
        model_id: values.model,
        inputs: {
          prompt: values.prompt,
          ...(values["negative-prompt"]
            ? { negative_prompt: values["negative-prompt"] }
            : {}),
          ...(image ? { image } : {}),
        },
        parameters,
        ...(values.backend ? { backend_id: values.backend } : {}),
        ...(values.precision
          ? {
              backend_options: {
                [values.backend || "diffusers-sdxl"]: {
                  precision: values.precision,
                },
              },
            }
          : {}),
      },
      values["idempotency-key"],
    );
    print(accepted);
    if (values.wait) await watch(accepted.job_id);
    return;
  }
  if (args[0] === "jobs") {
    if (args[1] === "list") {
      print(await client.jobs());
      return;
    }
    if (args[1] === "watch" && args[2]) {
      await watch(args[2]);
      return;
    }
    if (args[1] === "cancel" && args[2]) {
      print(await client.cancel(args[2]));
      return;
    }
    if (args[1] === "retry" && args[2]) {
      const old = await client.status(args[2]);
      print(await client.submit({ ...old.request, retry_of: old.job_id }));
      return;
    }
  }
  if (
    args[0] === "outputs" &&
    args[1] === "export" &&
    args[2] &&
    values.directory
  ) {
    const job = await client.status(args[2]);
    const directory = resolve(values.directory);
    await mkdir(directory, { recursive: true, mode: 0o700 });
    if (job.state !== "succeeded")
      throw new Error("Job has no committed outputs");
    for (const output of job.outputs) {
      const response = await client.output(job.job_id, output.output_id);
      await writeFile(
        join(directory, `${job.job_id}-${output.output_id}.png`),
        Buffer.from(await response.arrayBuffer()),
        { flag: "wx", mode: 0o600 },
      );
    }
    await writeFile(
      join(directory, `${job.job_id}-manifest.json`),
      JSON.stringify(await client.manifest(job.job_id), null, 2),
      { flag: "wx", mode: 0o600 },
    );
    print({ directory });
    return;
  }
  throw new Error("Unknown or incomplete command. Use --help.");
}
const invoked =
  process.argv[1] &&
  /(^|[\\/])(cli\.js|media-runtime)$/.test(process.argv[1]);
if (invoked)
  main().catch((error: any) => {
    console.error(
      JSON.stringify({
        error: error.status
          ? error.message
          : error.message === "fetch failed"
            ? "Runtime is unavailable"
            : error.message,
      }),
    );
    process.exitCode =
      error.status >= 500 || error.message === "fetch failed" ? 3 : 2;
  });
