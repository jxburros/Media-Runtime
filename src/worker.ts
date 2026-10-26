import { spawn, type ChildProcess } from "node:child_process";
import { randomBytes } from "node:crypto";
import { ApiError, PROTOCOL } from "./contracts.js";
import type { Config } from "./config.js";
export class WorkerClient {
  child?: ChildProcess;
  url: string;
  token: string;
  discoveredSession?: string;
  constructor(public config: Config) {
    this.url = config.worker.url.replace(/\/$/, "");
    this.token = process.env[config.worker.token_env] || "";
  }
  async start() {
    const command = this.config.worker.command;
    if (!command) {
      if (this.token.length < 32)
        throw new Error("Set the configured worker token environment variable");
      return;
    }
    this.token = randomBytes(32).toString("hex");
    const child = spawn(command[0], command.slice(1), {
      stdio: ["pipe", "pipe", "ignore"],
      env: {
        ...process.env,
        MEDIA_ENGINE_TOKEN: this.token,
        MEDIA_ENGINE_SUPERVISED: "1",
      },
    });
    this.child = child;
    const record: any = await new Promise((resolve, reject) => {
      let buffer = "";
      const timer = setTimeout(
        () => done(new Error("Worker startup timed out")),
        this.config.worker_timeout_ms,
      );
      const done = (err?: Error, value?: any) => {
        clearTimeout(timer);
        child.stdout?.off("data", onData);
        err ? reject(err) : resolve(value);
      };
      const onData = (data: Buffer) => {
        buffer += data.toString();
        if (buffer.length > 8192)
          return done(new Error("Worker startup record too large"));
        if (buffer.includes("\n")) {
          try {
            done(undefined, JSON.parse(buffer.split("\n")[0]));
          } catch {
            done(new Error("Invalid worker startup record"));
          }
        }
      };
      child.stdout?.on("data", onData);
      child.once("error", (err) => done(err));
      child.once("exit", () => done(new Error("Worker exited before startup")));
    }).catch((e) => {
      child.kill("SIGTERM");
      throw e;
    });
    child.stdout?.resume();
    if (
      record.protocol !== PROTOCOL ||
      record.pid !== child.pid ||
      !Number.isInteger(record.port) ||
      record.port < 1 ||
      record.port > 65535 ||
      typeof record.session_id !== "string"
    ) {
      child.kill("SIGTERM");
      throw new Error("Incompatible worker startup record");
    }
    this.url = `http://127.0.0.1:${record.port}`;
    this.discoveredSession = record.session_id;
  }
  async response(
    path: string,
    method = "GET",
    body?: unknown,
  ): Promise<Response> {
    let response: Response;
    try {
      response = await fetch(this.url + path, {
        method,
        headers: {
          Authorization: `Bearer ${this.token}`,
          ...(body === undefined ? {} : { "Content-Type": "application/json" }),
        },
        body: body === undefined ? undefined : JSON.stringify(body),
        signal: AbortSignal.timeout(this.config.worker_timeout_ms),
        redirect: "error",
      });
    } catch {
      throw new ApiError(
        503,
        "backend_unavailable",
        "Worker is unavailable; check its process and configuration",
      );
    }
    if (!response.ok) {
      let data: any;
      try {
        data = await response.json();
      } catch {}
      const code =
        typeof data?.error?.code === "string" &&
        /^[a-z_]{1,60}$/.test(data.error.code)
          ? data.error.code
          : "worker_error";
      throw new ApiError(
        response.status,
        code,
        `Worker reported ${code.replaceAll("_", " ")}`,
      );
    }
    return response;
  }
  async json(path: string, method = "GET", body?: unknown): Promise<any> {
    const r = await this.response(path, method, body);
    return r.json();
  }
  async probe() {
    const [info, caps] = await Promise.all([
      this.json("/v1/info"),
      this.json("/v1/capabilities"),
    ]);
    if (
      info.protocol !== PROTOCOL ||
      caps.protocol !== PROTOCOL ||
      info.session_id !== caps.session_id ||
      (this.discoveredSession && this.discoveredSession !== info.session_id)
    )
      throw new ApiError(
        503,
        "protocol_mismatch",
        "Requires media-worker/1.0 in the discovered worker session",
      );
    if (caps.backend_id !== "diffusers-sdxl")
      throw new ApiError(
        503,
        "unsupported_backend",
        "This release supports the explicit diffusers-sdxl backend",
      );
    return { info, caps };
  }
  async stop() {
    const child = this.child;
    if (!child || child.exitCode !== null) return;
    child.kill("SIGTERM");
    await new Promise<void>((resolve) => {
      const timer = setTimeout(() => {
        child.kill("SIGKILL");
        resolve();
      }, 6000);
      child.once("exit", () => {
        clearTimeout(timer);
        resolve();
      });
    });
    this.child = undefined;
  }
}
