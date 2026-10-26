# Media Runtime

A standalone, authenticated local media service with a TypeScript CLI/API, terminal dashboard,
SQLite FIFO jobs, resumable job events, cancellation, durable PNG inputs, immutable PNG outputs and
provenance. Media Engine is its separately installed SDXL inference worker. Studio connects to this
service through its gateway.

This release implements the **first complete path**, the **operator experience** stage and
image-to-image from the **additional media operations** stage in [runtime-spec.md](runtime-spec.md),
and accepts Engine's **owned pipeline** backend behind the unchanged worker protocol. LoRA,
upscaling, video, multi-GPU and native inference are later stages.

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
node dist/cli.js models inspect            # what each catalog handle is missing, before registering
node dist/cli.js models register examples/model.json
node dist/cli.js models show studio-sdxl --verify   # re-hash through the worker; memory estimate
node dist/cli.js run portrait --model studio-sdxl --prompt "Soft window light" --seed 42 --wait
node dist/cli.js run variation --model studio-sdxl --prompt "Softer light" --image ./source.png --strength 0.5 --wait
node dist/cli.js adapters register examples/adapter.json
node dist/cli.js run portrait --model studio-sdxl --prompt "Soft window light" --adapter studio-style:0.8 --wait
node dist/cli.js jobs list
node dist/cli.js outputs export JOB_ID --directory ./exports
node dist/cli.js tui
```

Use `npm link` if you want the `media-runtime` executable on PATH. `run` without `--wait` returns
immediately; closing a CLI, dashboard or browser never cancels an accepted job. `jobs watch`,
`jobs cancel`, `jobs retry`, `recipes list`, `recipes inspect`, `recipes register`, `inputs upload`,
`inputs list` and `inputs delete` use the same public API. `--json` produces machine-readable
records. Exit codes: 0 success, 2 invalid input (and an incomplete `models inspect`), 3 unavailable
service (and a `doctor` that found problems), 4 a failed/cancelled job when waiting. Explicit retry
creates a new linked job.

`doctor` explains the negotiated protocol, backend and ownership stage, device readiness, missing
worker dependencies, load defaults, the memory estimate with its basis, registered models and their
missing components, recipes, writable storage and free space, and the queue. `tui` is a terminal
dashboard over the public API: queue, stage and step progress, model, backend, seed, output
location and errors, with `j`/`k` to select, `c` to cancel and `q` to quit. It adds no execution
behavior; images open in an external viewer after `outputs export`.

## Model installation workflow

Nothing here downloads weights. Download the complete SDXL base Diffusers bundle out of band,
place it in a directory the service may read, add the directory to `artifact_catalog` under an
opaque handle, then run `models inspect`. It lists each handle's completeness, missing roles,
forbidden Python/pickle files and symlinks without hashing or registering, and prints the
registration document for a complete bundle. `models register` hashes every file through the
worker and records the bundle identity; `models show MODEL --verify` repeats that check later and
returns the worker's memory estimate for its load defaults.

## LoRA adapters

An adapter is an approved directory in `artifact_catalog` holding one PEFT-layout safetensors
file. `adapters register` hashes it and validates the layout through the worker; `adapters list`
shows registrations. `run --adapter ID[:SCALE]` (repeatable, up to four) or a recipe's `adapters`
list selects them. The adapter set is part of the worker's load identity: changing adapters or
scales reloads the model, and the job and manifest record each adapter's digest and scale. Workers
that do not advertise adapters, or that speak only `media-worker/1.0`, reject such requests.
Kohya-format files are refused until converted to the PEFT layout.

## Image-to-image

The `variation` recipe (pipeline `image-to-image`) takes an uploaded PNG input. `run --image FILE`
uploads the file first (`inputs upload` does the same) and submits the returned `input_id`; the
service never accepts a client path. Inputs are durable, namespace-owned and listed by
`inputs list`. The resolved width and height are the image's own dimensions, and overriding them to
other values is an error: nothing is resized. `strength` (request, then recipe, then worker default)
controls how many of `steps` are actually sampled; the job records `effective_steps`, progress
reports the real total and the manifest lists the input's digest. Inputs follow
`output_retention_days` and cannot be deleted while a job references them.

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
identity and probes readiness. Engine's `--backend owned-sdxl` runs the project-owned pipeline;
set `worker.backend_id` to pin which backend Runtime expects, and pass `--backend` on `run` to
state the choice explicitly. A request for a backend other than the running one is rejected. Stdout is drained and stderr is not retained, so worker logs cannot
leak prompts or grow without bound. Only this child can be terminated after the cancellation grace
period; forced termination settles the job as `execution_interrupted`, not confirmed cancellation.

The worker contract is [media-worker/1.1](https://github.com/jxburros/Media-Engine/blob/main/docs/protocol.md).
Runtime 0.3.0 negotiates the highest minor the worker lists: Engine 0.2.0 and later give
`media-worker/1.1` with image-to-image and (Engine 0.3.0) LoRA adapters; Engine 0.1.0 gives `media-worker/1.0` with text-to-image only. There is one
active execution and at most 32 queued jobs by default; all operational limits are in the config.
Requested model, backend, seed, dimensions, precision and scheduler are never silently changed.

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

Outputs, inputs and events have no automatic expiry by default. Set `output_retention_days`
explicitly to expire outputs and inputs; manifests and authorized metadata remain, and retrieval
returns 410. Active jobs, referenced inputs and uncollected Engine outputs are preserved. Exports create copies and refuse to overwrite files.
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
located only in Engine's tests, covering text-to-image, image-to-image, 1.0 negotiation, catalog
inspection, verification and LoRA adapters; `test/tui.test.mjs` renders the dashboard and doctor output. The
fixture is not an inference fallback. GPU image quality and hardware
compatibility require the selected real bundle; see [implementation decisions](docs/decisions.md).
