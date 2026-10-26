# Media Runtime

A standalone, authenticated local media service with a TypeScript CLI/API, SQLite FIFO jobs,
resumable job events, cancellation, immutable PNG outputs and provenance. Media Engine is its
separately installed SDXL inference worker. Studio connects to this service through its gateway.

This release implements the **first increment** in [runtime-spec.md](runtime-spec.md).
The terminal dashboard, img2img, LoRA, video, multi-GPU and owned native inference are later stages.

## Install and run

Use Node **24.19.0** (24 LTS), Python **3.12.14**, Linux x64 and the companion
[Media Engine](https://github.com/jxburros/Media-Engine). Install dependencies explicitly:

```sh
npm ci
npm run build
```

1. Prepare an already-downloaded, complete SDXL base Diffusers directory with safetensors weights.
   No command here downloads weights or converts checkpoints. All seven component directories,
   model_index.json and the Euler scheduler configuration must be present; remove Python/pickle
   weights and symlinks. Keep the approved directory read-only while serving.
2. Copy `examples/runtime.config.json` to `runtime.config.json`. Set the approved artifact path.
   Give Engine the same handle/path mapping using its catalog file. A model registration uses an
   opaque handle, never a caller-supplied filesystem path.
3. Set separate random credentials through protected environment variables. For example, generate
   a value with `node -e "console.log(require('crypto').randomBytes(32).toString('hex'))"` and store it
   in your service manager's protected environment file. The sample config names
   `MEDIA_RUNTIME_TOKEN`, `MEDIA_STUDIO_TOKEN`, and `MEDIA_ENGINE_TOKEN`; no credential ships here.
   Remove the Studio credential entry when using standalone mode only.
4. Start Engine according to its README, then keep Runtime running as a service:

```sh
node dist/cli.js serve --config runtime.config.json
# In another terminal, with MEDIA_RUNTIME_TOKEN configured:
node dist/cli.js doctor
node dist/cli.js models register examples/model.json
node dist/cli.js run portrait --model studio-sdxl --prompt "Soft window light" --seed 42 --wait
node dist/cli.js jobs list
node dist/cli.js outputs export JOB_ID --directory ./exports
```

Use `npm link` if you want the `media-runtime` executable on PATH. `run` without `--wait` returns
immediately; closing a CLI or browser never cancels an accepted job. `jobs watch`, `jobs cancel`,
`jobs retry`, `recipes list`, `recipes inspect`, and `recipes register` use the same public API.
`--json` produces machine-readable records. Exit codes: 0 success, 2 invalid input, 3 unavailable
service, 4 a failed/cancelled job when waiting. Explicit retry creates a new linked job.

`MEDIA_RUNTIME_URL` defaults to `http://127.0.0.1:7810`. `MEDIA_RUNTIME_NAMESPACE` supplies a namespace
only for a credential with a matching `allowed_namespaces` grant. Never put an integration token
in a browser. All routes except coarse `/health` require bearer authentication; browser origins
are refused, and the server binds loopback only.

## Worker ownership

By default the configured Engine is externally managed: Runtime never kills it. To supervise a
private worker instead, set `worker.command` to an argument array, for example:

```json
["/absolute/Media-Engine/.venv/bin/media-engine", "--catalog", "/absolute/catalog.json", "--directory", "/absolute/worker-data", "--port", "0", "--device", "cuda:0"]
```

Runtime generates a per-launch worker token, reads the startup record, checks session/protocol
identity and probes readiness. Stdout is drained and stderr is not retained, so worker logs cannot
leak prompts or grow without bound. Only this child can be terminated after the cancellation grace
period; forced termination settles the job as `execution_interrupted`, not confirmed cancellation.

The worker contract is [media-worker/1.0](https://github.com/jxburros/Media-Engine/blob/main/docs/protocol.md).
Versions 0.1.0 of both apps are compatible. There is one active execution and at most 32 queued
jobs by default; all operational limits are in the config. Requested model, seed, dimensions,
precision and scheduler are never silently changed.

## Persistence and recovery

SQLite WAL/FULL synchronization stores normalized requests, recipe/model identities, dispatch
attempts, session IDs and every runtime event. Idempotency keys are namespace scoped; identical
retries return the existing job even after restart. Conflicting reuse returns 409. FIFO admission
is atomic, and excess jobs receive 429 plus Retry-After.

A surviving worker is reconciled by session and attempt before any ambiguous dispatch is retried.
A replacement session fails interrupted work without regenerating it. An unreachable worker leaves
work pending reconciliation; queued jobs stay durable. Successful output bytes are checked against
size, SHA-256 and PNG dimensions, then fsynced with the manifest before the database records success.
The worker's copy is discarded only after that commit. This is not an exactly-once guarantee across
process crashes. Keep one Runtime per state directory; the CLI uses an exclusive process lock.

Outputs/events have no automatic expiry by default. Set `output_retention_days` explicitly to expire
outputs; manifests and authorized metadata remain, and retrieval returns 410. Active jobs and
uncollected Engine outputs are preserved. Exports create copies and refuse to overwrite files.
Prompts remain private job data. Local state uses directory mode 0700 and file mode 0600.

## Studio

The companion Studio integration adds **Media Runtime (local SDXL)** to Settings → Runtimes.
Connect to `http://127.0.0.1:7810` with the dedicated Studio access key, then select the registered
model in Create. Its gateway derives `studio:<scope digest>` namespaces from authenticated user
and project context, preserves model routing, displays actual stage/step status, sends Stop to the
runtime and imports verified output copies with provenance. Personal and project paths are the same.

## Development

Keep Media-Engine beside this checkout (or set `MEDIA_ENGINE_REPO`) and run `npm test`.
The integration suite drives the real worker HTTP server using an explicitly identified test backend
located only in Engine's tests. It is not an inference fallback. GPU image quality and hardware
compatibility require the selected real bundle; see [implementation decisions](docs/decisions.md).
