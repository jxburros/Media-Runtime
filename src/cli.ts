#!/usr/bin/env node
import { parseArgs } from "node:util";
import { readFile, mkdir, writeFile, open, unlink } from "node:fs/promises";
import { resolve, join } from "node:path";
import { MediaRuntimeClient } from "./client.js";
import { readConfig } from "./config.js";
import { Runtime } from "./runtime.js";
import { createRuntimeServer } from "./server.js";
import { terminal } from "./contracts.js";
process.umask(0o077);
const help = `Media Runtime 0.1.0 (Node 24)\n\nmedia-runtime serve --config runtime.config.json\nmedia-runtime doctor\nmedia-runtime models register model.json | models list\nmedia-runtime recipes register recipe.json | recipes list | recipes inspect portrait\nmedia-runtime run portrait --model MODEL --prompt TEXT [--seed N] [--wait] [--json]\nmedia-runtime jobs list | jobs watch JOB | jobs cancel JOB | jobs retry JOB\nmedia-runtime outputs export JOB --directory DIR\n\nClient configuration: MEDIA_RUNTIME_URL, MEDIA_RUNTIME_TOKEN, MEDIA_RUNTIME_NAMESPACE.\nExit codes: 0 success, 2 invalid input, 3 unavailable service, 4 failed/cancelled waited job.\n`;
async function main() {
  const { values, positionals: args } = parseArgs({
    allowPositionals: true,
    options: {
      config: { type: "string" },
      model: { type: "string" },
      prompt: { type: "string" },
      "negative-prompt": { type: "string" },
      seed: { type: "string" },
      width: { type: "string" },
      height: { type: "string" },
      steps: { type: "string" },
      guidance: { type: "string" },
      precision: { type: "string" },
      wait: { type: "boolean" },
      json: { type: "boolean" },
      directory: { type: "string" },
      help: { type: "boolean" },
      version: { type: "boolean" },
      "idempotency-key": { type: "string" },
    },
  });
  if (values.version) {
    console.log("0.1.0");
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
        version: "0.1.0",
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
    print(await client.health());
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
  if (args[0] === "run" && args[1]) {
    const parameters: Record<string, number> = {};
    for (const field of [
      "seed",
      "width",
      "height",
      "steps",
      "guidance",
    ] as const)
      if (values[field] !== undefined)
        parameters[field] = Number(values[field]);
    const accepted = await client.submit(
      {
        recipe_id: args[1],
        model_id: values.model,
        inputs: {
          prompt: values.prompt,
          ...(values["negative-prompt"]
            ? { negative_prompt: values["negative-prompt"] }
            : {}),
        },
        parameters,
        ...(values.precision
          ? {
              backend_options: {
                "diffusers-sdxl": { precision: values.precision },
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
      print(await client.json("/v1/jobs"));
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
