/**
 * Terminal dashboard. It consumes the public API only and adds no execution behavior:
 * every action it offers (cancel, export hints) is an existing API operation.
 */
import type { MediaRuntimeClient } from "./client.js";
import { terminal } from "./contracts.js";

export type DashboardState = {
  runtime: any;
  jobs: any[];
  selected: number;
  message: string;
  error?: string;
  updated_at: string;
};

const pad = (text: string, width: number) =>
  text.length > width
    ? text.slice(0, Math.max(0, width - 1)) + "…"
    : text.padEnd(width);
const num = (value: number | null | undefined, unit = "") =>
  value === null || value === undefined ? "unknown" : `${value}${unit}`;

export function describeStage(job: any) {
  if (job.state === "running") {
    if (job.progress?.stage === "sampling" && job.progress.total_steps)
      return `sampling ${job.progress.completed_steps}/${job.progress.total_steps}`;
    return job.stage || "dispatching";
  }
  if (job.state === "queued") return job.cancel_requested ? "cancelling" : "queued";
  return job.state;
}

export function outputLocation(job: any) {
  if (job.state !== "succeeded" || !job.outputs?.length) return "";
  const output = job.outputs[0];
  if (output.expires_at && Date.parse(output.expires_at) <= Date.now())
    return "expired";
  return `/v1/jobs/${job.job_id}/outputs/${output.output_id}`;
}

/** Pure renderer: state in, screen lines out. The test suite calls this directly. */
export function renderDashboard(
  state: DashboardState,
  columns = 100,
  rows = 30,
): string[] {
  const lines: string[] = [];
  const worker = state.runtime?.worker ?? {};
  const queue = state.runtime?.queue ?? {};
  lines.push(
    pad(
      `Media Runtime ${state.runtime?.runtime_version ?? "?"}  ${state.updated_at}  [q] quit  [j/k] select  [c] cancel  [r] refresh`,
      columns,
    ),
  );
  lines.push(
    pad(
      worker.error
        ? `worker: ${worker.error} — ${worker.message ?? ""}`
        : `worker: ${worker.backend_id} (${worker.ownership}) ${worker.worker_version}  ${worker.protocol}  device ${worker.device}  ${worker.device_ready ? "ready" : "not ready"}  state ${worker.state}`,
      columns,
    ),
  );
  lines.push(
    pad(
      `queue: ${num(queue.running)} running, ${num(queue.queued)} queued of ${num(queue.capacity)}  models: ${(state.runtime?.models ?? [])
        .map((m: any) => `${m.model_id}${m.loaded ? "*" : ""}${m.available ? "" : "(missing)"}`)
        .join(", ") || "none"}  memory: ${num(worker.memory_estimate?.value_mb, " MiB")} (${worker.memory_estimate?.source ?? "unknown"})`,
      columns,
    ),
  );
  lines.push("".padEnd(columns, "─"));
  const widths = { id: 14, state: 18, task: 14, model: 12, seed: 11, created: 15 };
  lines.push(
    pad(
      [
        pad("job", widths.id),
        pad("stage", widths.state),
        pad("task", widths.task),
        pad("model", widths.model),
        pad("seed", widths.seed),
        pad("created", widths.created),
        "error / output",
      ].join(" "),
      columns,
    ),
  );
  const available = Math.max(1, rows - lines.length - 4);
  const start = Math.max(0, Math.min(state.selected - available + 1, state.jobs.length - available));
  const visible = state.jobs.slice(start, start + available);
  visible.forEach((job, index) => {
    const marker = start + index === state.selected ? ">" : " ";
    const location = outputLocation(job);
    const tail = job.error
      ? `${job.error.code}${job.error.forced_termination ? " (forced)" : ""}`
      : location === "expired"
        ? "output expired"
        : location
          ? "output ready"
          : "";
    lines.push(
      pad(
        marker +
          [
            pad(job.job_id.slice(4, 4 + widths.id - 1), widths.id - 1),
            pad(describeStage(job), widths.state),
            pad(job.task ?? "text-to-image", widths.task),
            pad(job.model?.model_id ?? "", widths.model),
            pad(String(job.request?.parameters?.seed ?? ""), widths.seed),
            pad(job.created_at?.slice(5, 19).replace("T", " ") ?? "", widths.created),
            tail,
          ].join(" "),
        columns,
      ),
    );
  });
  if (!state.jobs.length) lines.push(pad("  no jobs in this namespace", columns));
  while (lines.length < rows - 3) lines.push("".padEnd(columns));
  const selected = state.jobs[state.selected];
  lines.push("".padEnd(columns, "─"));
  lines.push(
    pad(
      selected
        ? `${selected.job_id}  ${selected.backend?.backend_id ?? ""}  ${selected.request?.parameters?.width}×${selected.request?.parameters?.height} steps ${selected.request?.parameters?.steps}${selected.request?.parameters?.strength !== undefined ? ` strength ${selected.request.parameters.strength}` : ""}  ${outputLocation(selected) ? `output ${outputLocation(selected)} (export: media-runtime outputs export ${selected.job_id} --directory DIR)` : terminal(selected.state) ? "no output" : "running jobs survive closing this dashboard"}`
        : "select a job to see its details",
      columns,
    ),
  );
  lines.push(pad(state.error ? `error: ${state.error}` : state.message, columns));
  return lines;
}

export async function fetchDashboard(
  client: MediaRuntimeClient,
  previous: DashboardState,
): Promise<DashboardState> {
  try {
    const [runtime, listing] = await Promise.all([
      client.health(),
      client.jobs({ limit: 100, newest: true }),
    ]);
    const jobs = listing.jobs as any[];
    const current = previous.jobs[previous.selected]?.job_id;
    const index = jobs.findIndex((j) => j.job_id === current);
    return {
      runtime,
      jobs,
      selected: index >= 0 ? index : Math.min(previous.selected, Math.max(0, jobs.length - 1)),
      message: previous.message,
      updated_at: new Date().toISOString().slice(11, 19) + "Z",
    };
  } catch (e: any) {
    return {
      ...previous,
      error: e.status ? e.message : "Runtime is unavailable",
      updated_at: new Date().toISOString().slice(11, 19) + "Z",
    };
  }
}

export async function runDashboard(
  client: MediaRuntimeClient,
  io: { stdin: NodeJS.ReadStream; stdout: NodeJS.WriteStream },
  intervalMs = 1000,
) {
  let state: DashboardState = {
    runtime: null,
    jobs: [],
    selected: 0,
    message: "",
    updated_at: "",
  };
  const draw = () => {
    const lines = renderDashboard(state, io.stdout.columns || 100, io.stdout.rows || 30);
    io.stdout.write("\x1b[H\x1b[2J" + lines.join("\n") + "\n");
  };
  const refresh = async () => {
    state = await fetchDashboard(client, state);
    draw();
  };
  if (io.stdin.isTTY) io.stdin.setRawMode(true);
  io.stdin.resume();
  io.stdin.setEncoding("utf8");
  io.stdout.write("\x1b[?25l");
  await refresh();
  const timer = setInterval(() => void refresh(), intervalMs);
  await new Promise<void>((resolve) => {
    io.stdin.on("data", async (key: string) => {
      if (key === "q" || key === "\x03") return resolve();
      if (key === "j" || key === "\x1b[B")
        state.selected = Math.min(state.selected + 1, Math.max(0, state.jobs.length - 1));
      if (key === "k" || key === "\x1b[A") state.selected = Math.max(state.selected - 1, 0);
      if (key === "r") return void refresh();
      if (key === "c") {
        const job = state.jobs[state.selected];
        if (job && !terminal(job.state)) {
          try {
            await client.cancel(job.job_id);
            state.message = `cancellation requested for ${job.job_id}`;
          } catch (e: any) {
            state.error = e.message;
          }
          return void refresh();
        }
      }
      draw();
    });
  });
  clearInterval(timer);
  io.stdout.write("\x1b[?25h\x1b[2J\x1b[H");
  if (io.stdin.isTTY) io.stdin.setRawMode(false);
  io.stdin.pause();
}
