# Media Runtime

A standalone, authenticated local media service with a Go CLI/API, terminal dashboard,
SQLite FIFO jobs, resumable job events, cancellation, durable PNG inputs, immutable PNG outputs and
provenance. Media Engine is its separately installed SDXL inference worker. Studio connects to this
service through its gateway.

This release implements the **first complete path**, the **operator experience** stage and
image-to-image from the **additional media operations** stage in [runtime-spec.md](runtime-spec.md),
and accepts Engine's **owned pipeline** backend behind the unchanged worker protocol. Release 0.4.0
is the Go rewrite of the 0.3.0 TypeScript service at exact feature parity: the API, configuration,
state directory, manifests and CLI are unchanged, and a 0.3.0 state directory is served as is.
Release 0.5.0 speaks `media-worker/1.2` and adds multiple outputs per job, upscaling, the
conversion provenance workflow, and automatic settings with authorized fallbacks. Release 0.6.0
adds linear typed recipes (schema 2): several operations, each its own worker execution, chained
through durable intermediate outputs. Release 0.7.0 drives a pool of workers, one Engine process
per GPU, with one execution per worker. Release 0.8.0 adds remote workers over mutual TLS (Engine
0.5.0's network mode) and a network mode for the runtime's own API. Release 0.9.0 speaks
`media-worker/1.3` (Engine 0.6.0) and adds [video](#video): image-to-video on the `svd` family,
committed as a lossless frame archive and a poster image. Audio and native inference are later
stages, specified in the same document and sequenced in
[docs/implementation-plan.md](docs/implementation-plan.md).

## Install and run

Use Go **1.24** or later (no cgo), Python **3.12.14**, Linux x64 and the companion
[Media Engine](https://github.com/jxburros/Media-Engine). Build the single static binary:

```sh
CGO_ENABLED=0 go build -o media-runtime ./cmd/media-runtime
```

1. Prepare an already-downloaded, complete SDXL base Diffusers directory with safetensors weights.
   No command here downloads weights or converts checkpoints. All seven component directories,
   model_index.json and the Euler scheduler configuration must be present; remove Python/pickle
   weights and symlinks. Keep the approved directory read-only while serving.
2. Copy `examples/runtime.config.json` to `runtime.config.json`. Set the approved artifact path.
   Give Engine the same handle/path mapping using its catalog file. A model registration uses an
   opaque handle, never a caller-supplied filesystem path.
3. Set separate random credentials through protected environment variables. For example, generate
   a value with `openssl rand -hex 32` and store it in your service manager's protected
   environment file. The sample config names
   `MEDIA_RUNTIME_TOKEN`, `MEDIA_STUDIO_TOKEN`, and `MEDIA_ENGINE_TOKEN`; no credential ships here.
   Remove the Studio credential entry when using standalone mode only.
4. Start Engine according to its README, then keep Runtime running as a service:

```sh
media-runtime serve --config runtime.config.json
# In another terminal, with MEDIA_RUNTIME_TOKEN configured:
media-runtime doctor
media-runtime models inspect            # what each catalog handle is missing, before registering
media-runtime models register examples/model.json
media-runtime models show studio-sdxl --verify   # re-hash through the worker; memory estimate
media-runtime run portrait --model studio-sdxl --prompt "Soft window light" --seed 42 --wait
media-runtime run variation --model studio-sdxl --prompt "Softer light" --image ./source.png --strength 0.5 --wait
media-runtime adapters register examples/adapter.json
media-runtime run portrait --model studio-sdxl --prompt "Soft window light" --adapter studio-style:0.8 --wait
media-runtime models register examples/model.svd.json        # an svd bundle, Engine 0.6.0 or later
media-runtime run animate --model svd-xt --image ./frame.png --frames 25 --fps 7 --wait
media-runtime jobs list
media-runtime outputs export JOB_ID --directory ./exports
media-runtime tui
```

Install the binary anywhere on PATH; it has no runtime dependencies. `run` without `--wait` returns
immediately; closing a CLI, dashboard or browser never cancels an accepted job. `jobs watch`,
`jobs cancel`, `jobs retry`, `recipes list`, `recipes inspect`, `recipes register`, `inputs upload`,
`inputs list`, `inputs delete` and `workers list` use the same public API. `--json` produces machine-readable
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

`MEDIA_RUNTIME_URL` defaults to `http://127.0.0.1:7810`; it must be `https://` for a runtime beyond
loopback (see [API network mode](#api-network-mode)), and `MEDIA_RUNTIME_CA` names a PEM CA when the
runtime's certificate comes from a private CA. `MEDIA_RUNTIME_NAMESPACE` supplies a namespace
only for a credential with a matching `allowed_namespaces` grant. Never put an integration token
in a browser. All routes except coarse `/health` require bearer authentication; browser origins
are refused unless `allowed_origins` lists them, and the server binds loopback unless configured
for network mode with TLS.

## Multiple outputs

`run portrait ... --count 3` (or `parameters.count`, text-to-image only, up to the worker's
advertised limit) asks one execution for a batch. The outputs are `image`, `image_2`, `image_3`;
output `k` used seed `seed + k − 1` and records it, so `--count 1 --seed S+k−1` reproduces one image
alone. A job succeeds only when every output the worker advertises for the task is verified (size,
SHA-256, PNG signature and dimensions) and committed; a missing or extra output fails the job with
`output_unavailable`. `max_output_bytes` is a per-MIME map (a single number is still accepted as the
PNG limit), `GET /v1/jobs/{id}/outputs/{output_id}` serves any committed output, and `outputs export`
writes each one with an extension derived from its MIME type. Workers before 1.2 refuse `count`.

## Upscaling

The `enlarge` recipe (pipeline `upscale`, family `esrgan`) enlarges an uploaded PNG by the
registered upscaler's fixed factor. Register the network like any bundle, naming its family:
`models register examples/model.upscaler.json`; the directory holds one `.safetensors` network file
and its `config.json`, and the worker reports the factor (`models show` and `doctor` print it). Then
`run enlarge --model upscaler --image in.png [--tile 512] [--tile-overlap 16]`. Width and height are
the input's and cannot be overridden; prompts, seeds, steps, guidance, `count` and adapters are
refused, no seed is resolved or recorded, and a result larger than the worker's `output_side` limit
is refused before admission. Progress reports tiles. Real-ESRGAN `.pth` releases must be converted to
safetensors by the operator first; Runtime never converts weights.

## Video

The `animate` recipe (pipeline `image-to-video`, family `svd`, protocol 1.3; Engine 0.6.0 or later)
animates an uploaded PNG. Register a Diffusers `StableVideoDiffusionPipeline` directory naming its
family (`models register examples/model.svd.json`); the worker reads the sizes the bundle serves
(from its optional `video.json`, else 1024×576 and 576×1024) and its default frame count (the
unet's `num_frames`), and both are recorded with the model (`models show`, `doctor`). Then:

```sh
media-runtime run animate --model svd-xt --image ./frame.png --frames 25 --fps 7 --wait
media-runtime outputs export JOB_ID --directory ./exports   # JOB_ID-frames.zip, -poster.png, -manifest.json
```

- Width and height are the input's and must be one of the model's sizes; the input is never resized
  or cropped, and another size is refused before admission (field `video_size`).
- `frames` defaults to the model's, `fps`, `motion`, `noise_augmentation` and `decode_chunk` to the
  worker's task defaults, and `steps` to the recipe's (25); every value and the seed are resolved
  and recorded before dispatch. Values outside the worker's advertised limits are refused, never
  clamped. A prompt, `guidance`, `sampler`, `scheduler`, `strength`, `count`, tiling and adapters
  are refused by name. Workers before 1.3 refuse the recipe and the family.
- The job commits `frames` (`application/zip`: `frames.json` plus one lossless PNG per frame) and
  `poster` (`image/png`, frame 1). The archive is checked by size, SHA-256, its ZIP structure and a
  `frames.json` that must match the worker's record and the request (frame count, rate, size; one
  PNG per frame), so frames are never dropped or re-timed. When a worker advertises an encoded
  `video` output (`video/mp4`) it is required and committed too and exported as `.mp4`; the runtime
  never encodes video, and Engine 0.6.0 and 0.7.0 do not produce one.
- The job, each operation and the manifest record the frame count, rate and duration
  (`frames / fps`) from the verified record (`media`); jobs with a video operation write manifest
  schema 5. The per-MIME `max_output_bytes` limit for the archive defaults to 256 MiB, and outputs
  stream to disk rather than through memory. Progress reports decoding in frames; the dashboard
  shows frames and duration. Frames and videos open in external tools.
- In a [linear recipe](#linear-recipes) a video operation may take its image from an earlier
  operation; the poster (a PNG) can feed a later image input, the archive cannot.

## Linear recipes

A schema-2 recipe declares typed inputs (`string`, `image`, `number` with an optional range) and an
ordered list of operations. Each operation names its task and model family, binds its inputs from a
declared input (`input:<name>`) or from an earlier operation's output (`op:<op_id>/<output_id>`), and
may carry defaults and adapters; `publish` lists the outputs the job exposes (default: the last
operation's). A `number` input binds to a parameter of the task (for example `strength`).
[`examples/recipe.portrait-4k.json`](examples/recipe.portrait-4k.json) generates an image and
enlarges it:

```sh
media-runtime recipes register examples/recipe.portrait-4k.json
media-runtime recipes inspect portrait-4k          # typed declarations, operations, bindings
media-runtime run portrait-4k --model studio-sdxl --model-for enlarge=upscaler \
  --prompt "a lighthouse at dusk" --wait
```

`--model` (`model_id`) selects the first operation's model and `--model-for OP=MODEL` (`models`)
each later one; every operation must resolve to a registered model of its family and to a task the
worker advertises, and the whole job is resolved before admission, so a problem in any operation
refuses the submission with the operation named in `error.field` (`operations.<op_id>.…`). The
submission's `parameters` and `adapters` apply to the first operation, like `model_id`; later
operations take their recipe defaults, bound number inputs and worker defaults. Named inputs other
than `prompt`, `negative_prompt` and `image` are given with `--input NAME=VALUE`.

Operations run in order, each as its own worker execution with its own attempt. An operation's
outputs are verified and committed to durable storage before the next one is dispatched; an output
feeding a later operation is uploaded to the worker from that durable copy, and the worker reloads
only when the next operation's bundle, adapters or options differ. `stage` and `progress` events
carry `op_id`, `operation_index` and `operation_count`; there is no percentage across operations,
and `jobs watch` and the dashboard show `op_id/stage`. The job lists `operations` with each one's
state, model, parameters, worker identities and outputs. Published outputs appear under `outputs`
(under the worker's ID, or `<op_id>__<id>` when two published outputs share one) and are what
`outputs export` writes; unpublished intermediates are listed under `intermediate_outputs` as
`<op_id>__<id>`, are served by the same outputs route, and follow the same retention. Cancellation
between operations stops the job before the next dispatch; a failed operation fails the job with
`error.operation`; a retry runs every operation again. The manifest (schema 4) lists every
operation and output. Schema-1 recipes run exactly as before, as one operation.

## Converted checkpoints

A single-file SDXL checkpoint is not a bundle. Convert it offline with Engine
(`media-engine convert --input model.safetensors --reference-handle sdxl-base --catalog catalog.json
--output /approved/converted`), add the output directory to `artifact_catalog`, then run
`models inspect`: it reports the source checkpoint digest, the reference bundle digest and the
converter versions from `conversion.json`, and prints a registration document carrying them as
`source.conversion`. `models register` records that provenance in the bundle, and every manifest of
a job that used it carries it. Registering an unconverted checkpoint directory fails with
`unsupported`, field `format`.

## Automatic settings and fallbacks

Against a 1.2 worker that advertises it, `--precision auto` (or `backend_options.<backend>.precision:
"auto"`) lets the worker choose the precision; the job and manifest record `resolved_options` and the
worker's `resolutions` with their reasons, and an identical request resolves identically without a
reload. `--fallback cpu` and `--fallback lower_precision` (`authorized_fallbacks`) permit the worker to
load on the host CPU or at a lower-memory precision when the selected device is unavailable or too
small. Only credentials configured with `"allow_fallbacks": true` may authorize them (others get
`403` with field `authorized_fallbacks`). An applied fallback is recorded in the job
(`applied_fallbacks`), announced as a `stage` event, shown by `jobs watch` and the dashboard, and kept
in the manifest. A fallback never changes the model, recipe or generation parameters. Without an
authorization nothing changes: an unavailable device is still refused and an over-budget load still
fails with the worker's code.

## Worker ownership

By default the configured Engine is externally managed: Runtime never kills it. To supervise a
private worker instead, set `worker.command` (or a pool entry's `command`) to an argument array, for example:

```json
["/absolute/Media-Engine/.venv/bin/media-engine", "--catalog", "/absolute/catalog.json", "--directory", "/absolute/worker-data", "--port", "0", "--device", "cuda:0"]
```

Runtime generates a per-launch worker token, reads the startup record, checks session/protocol
identity and probes readiness. Engine's `--backend owned-sdxl` runs the project-owned pipeline;
set `worker.backend_id` to pin which backend Runtime expects, and pass `--backend` on `run` to
state the choice explicitly. A request for a backend other than the running one is rejected. Stdout is drained and stderr is not retained, so worker logs cannot
leak prompts or grow without bound. Only this child can be terminated after the cancellation grace
period; forced termination settles the job as `execution_interrupted`, not confirmed cancellation.

The worker contract is [media-worker/1.2](https://github.com/jxburros/Media-Engine/blob/main/docs/protocol.md).
Runtime 0.5.0 negotiates the highest minor the worker lists: Engine 0.4.0 gives `media-worker/1.2`
with multiple outputs, upscaling, loaders, automatic settings, fallbacks and device identity;
Engine 0.2.0 and later give `media-worker/1.1` with image-to-image and (Engine 0.3.0) LoRA adapters;
Engine 0.1.0 gives `media-worker/1.0` with text-to-image only. Every 1.2 feature is refused with
`unsupported` naming the field against an older worker. There is one
active execution per worker and at most 32 queued jobs by default; all operational limits are in the config.
Requested model, backend, seed, dimensions, precision and scheduler are never silently changed.

## Worker pool

To use several GPUs, list one Engine process per device under `workers` instead of `worker`
([`examples/runtime.pool.config.json`](examples/runtime.pool.config.json)). Each entry has a
`worker_id`, a loopback `url` and `token_env` or an owned `command` (or a [remote](#remote-workers)
`https://` URL with `tls`), and optionally the expected
`backend_id`, `device` and `memory_budget_mb`; `device` and the budget are the operator's
expectations, shown beside what the worker reports, and a worker whose reported device differs from
`device` is unavailable. The single `worker` object keeps working as a pool of one named `default`.
Every worker must serve the same artifact catalog (same handles, same bytes).

Each worker runs one job at a time. The oldest queued job goes to an idle, ready worker that already
has its model, adapters and options loaded, else to the first idle worker (in configuration order)
that advertises its tasks, model families and protocol minor; list the preferred device first.
`run --worker ID` (`worker_id`) pins a job to one worker, and it waits for that worker. FIFO is
literal: while the oldest job waits (for a pinned or unsuitable worker), no later job jumps ahead
even if another worker is free; set `"allow_reordering": true` to let later jobs use the free
workers. A linear recipe runs every operation on the worker its first operation started on. Queue
capacity is shared by the pool.

Two workers on the same device (the same host, device and index in their 1.2 device identity)
refuse the start; with `"strict_devices": false` the service starts and marks both unavailable.
Jobs, the `running` event and manifests record `worker_id` and `device_identity`;
`media-runtime workers list` (`GET /v1/workers`) shows each worker's device identity, readiness,
loaded model, whether it is busy and, for your namespace's job, its ID; `jobs list --worker ID`
(`GET /v1/jobs?worker_id=`) filters by worker; `doctor` prints a section per worker and the
dashboard a line per worker and a worker column. Each worker reconciles its own job after a
restart, and a crashed owned worker interrupts only the job it was running.

## Remote workers

A pool entry may name a worker on another host by an `https://` URL; the runtime then speaks mutual
TLS to Engine's network mode (Engine 0.5.0 or later, started with `--bind`, `--tls-cert`,
`--tls-key`, `--tls-client-ca` and `--no-parent-watch`; see Engine's README, "Network mode").
[`examples/runtime.remote.config.json`](examples/runtime.remote.config.json) shows one local and
one remote worker:

```json
{
  "worker_id": "render-host",
  "url": "https://render-host.internal:7811",
  "token_env": "MEDIA_RENDER_HOST_TOKEN",
  "tls": {
    "ca_file": "/etc/media-runtime/workers-ca.pem",
    "cert_file": "/etc/media-runtime/supervisor.pem",
    "key_file": "/etc/media-runtime/supervisor.key",
    "pinned_sha256": "<64 hex digits, optional>"
  },
  "remote_timeout_ms": 60000
}
```

A remote entry never has a `command` (configuration validation refuses one), and `tls` and
`remote_timeout_ms` are refused on a loopback `http://` entry. `remote_timeout_ms` is the request
timeout for that worker (default `worker_timeout_ms`). Every other field, scheduling rule and job
semantic is that of a [pool](#worker-pool) entry; a remote worker can be the only worker.

**Trust model.**

- The remote worker's catalog is its own. A registration's artifact handle must resolve on every
  worker, and the bundle digests a job carries prove the remote bundle is the same bytes; inputs
  and outputs cross the connection as verified bytes (digest, size, PNG checks), exactly as with a
  local worker.
- Identity is proven twice: the worker's certificate must chain to `ca_file` and name the URL's
  host, and its SHA-256 (of the DER, as Engine reports `certificate_sha256` in `/v1/info`) must
  equal `pinned_sha256` when set. Without a pin, the fingerprint seen on first contact is recorded
  in the state directory and every later connection, including each reconnect, must present it;
  `/v1/info` must also report the certificate actually presented. A different certificate is
  refused before any request is sent (`certificate_mismatch` in `doctor` and `workers list`),
  even when the same CA issued it, and a running job waits for reconciliation as after a lost
  connection.
- No ownership. The runtime never starts, stops or signals a remote worker; stopping the runtime
  leaves it running. A cancellation past `cancellation_grace_ms` is not forced: the job stays
  running until the worker itself reports a terminal state. A confirmed cancellation is
  `cancelled`; any other terminal state reported after the deadline settles the job as
  `execution_interrupted` with `forced_termination: false` and commits nothing. A new worker
  session interrupts the job, as for any worker.
- The bearer credential (`token_env`) still authorizes every request; the client certificate only
  authenticates the connection. Deliver the worker's per-launch token over a channel you already
  trust.

**Certificates.** The operator owns them; the runtime never generates, fetches or rotates any.

- `ca_file`: PEM CA certificate(s) that issued the worker's server certificate; it replaces the
  system roots for this worker. The worker's certificate must carry the URL's host name or IP
  address in its subject alternative names and `serverAuth`.
- `cert_file`, `key_file`: the runtime's client certificate (`clientAuth`, issued by the CA given
  to the worker as `--tls-client-ca`) and its unencrypted PEM key. The key must not be readable by
  group or others (`chmod 600`), and certificate and CA files must not be writable by them
  (`chmod 644`); the runtime refuses to start otherwise. Files are read once at start.
- Transport is TLS 1.3 (Engine's minimum); revocation lists are not consulted.
- Rotation: after replacing a worker's certificate, verify the new fingerprint out of band (Engine
  prints `certificate_sha256` in its startup record; `doctor` shows the presented one beside the
  expected one) and set `pinned_sha256` to it. The pin replaces the recorded fingerprint, so the
  pin may be removed again afterwards. Rotating the client certificate needs only a restart.

`doctor` prints a transport line per worker: loopback HTTP, or mutual TLS with the fingerprint and
whether it is pinned or recorded on first contact, plus the last `/v1/info` round trip.
`GET /v1/runtime` and `GET /v1/workers` report `transport`, `remote`, `certificate_sha256` and
`latency_ms` (and, in the runtime report, `certificate_source`, `presented_sha256` and the worker's
`ownership_available`).

## API network mode

The runtime's own API listens on `127.0.0.1` by default. To serve other hosts, give an IP address
as `bind` and server TLS material:

```json
{
  "bind": "0.0.0.0",
  "port": 7810,
  "tls": { "cert_file": "/etc/media-runtime/runtime.pem", "key_file": "/etc/media-runtime/runtime.key" },
  "allowed_origins": []
}
```

A `bind` that is not loopback without `tls` is refused at startup: the runtime never serves plain
HTTP beyond loopback. TLS on loopback is accepted. The key follows the same mode rules as above;
the certificate must name every address clients use. `serve` reports `scheme`, `bind` and the
certificate's `certificate_sha256` in its startup record. Credentials, namespaces and operator
separation are unchanged, and clients authenticate with the bearer credential (no client
certificate). Browser origins stay refused unless `allowed_origins` lists them exactly
(`https://studio.example`, scheme, host and port); a listed origin gets CORS headers and still needs
a credential. From the CLI, set `MEDIA_RUNTIME_URL=https://host:7810` and, for a private CA,
`MEDIA_RUNTIME_CA=/path/ca.pem`; the CLI refuses a plain `http://` URL to a non-loopback host.

## Persistence and recovery

SQLite WAL/FULL synchronization stores normalized requests, recipe/model identities, dispatch
attempts, session IDs and every runtime event. Idempotency keys are namespace scoped; identical
retries return the existing job even after restart. Conflicting reuse returns 409. FIFO admission
is atomic, and excess jobs receive 429 plus Retry-After.

A surviving worker is reconciled by session and attempt before any ambiguous dispatch is retried.
A replacement session fails interrupted work without regenerating it. An unreachable worker leaves
work pending reconciliation; queued jobs stay durable. Successful output bytes are streamed to a
temporary file and checked against size, SHA-256 and their MIME type's signature (PNG dimensions;
for a frame archive its ZIP structure and `frames.json`), then fsynced with the manifest before the
database records success.
The worker's copy is discarded only after that commit. This is not an exactly-once guarantee across
process crashes. Keep one Runtime per state directory; the CLI uses an exclusive process lock.

Outputs, inputs and events have no automatic expiry by default. Set `output_retention_days`
explicitly to expire outputs and inputs; manifests and authorized metadata remain, and retrieval
returns 410. Active jobs, referenced inputs and uncollected Engine outputs are preserved. Exports create copies and refuse to overwrite files.
Prompts remain private job data. Local state uses directory mode 0700 and file mode 0600.

## Studio

The companion Studio integration adds **Media Runtime (local SDXL)** to Settings → Runtimes.
Connect to `http://127.0.0.1:7810` (or the runtime's `https://` address in network mode) with the dedicated Studio access key, then select the registered
model in Create. Its gateway derives `studio:<scope digest>` namespaces from authenticated user
and project context, preserves model routing, displays actual stage/step status, sends Stop to the
runtime and imports verified output copies with provenance. Personal and project paths are the same.

## Gateway client

Studio's typed entry point is the TypeScript package in [`gateway-client/`](gateway-client/README.md)
(`@media-runtime/gateway-client`, no runtime dependencies). It is versioned with the service and
speaks only the public API.

## Development

Keep Media-Engine beside this checkout (or set `MEDIA_ENGINE_REPO`; `MEDIA_ENGINE_PYTHON` selects
the interpreter, default `python3`) and run `go test ./...`.
The integration tests in `internal/integration` drive the real worker HTTP server using an explicitly
identified test backend located only in Engine's tests, covering text-to-image, image-to-image, 1.0
negotiation, restart reconciliation, owned-worker crashes and forced termination, catalog
inspection, verification and LoRA adapters, the 1.2 surface (multiple outputs, upscaling,
conversion provenance, automatic precision, authorized fallbacks and a 1.1-pinned fixture's
refusals), linear recipes and worker pools of several fixture processes with distinct device
indexes (concurrency, pinning, affinity, a crashed worker, duplicate devices); these skip against an
Engine fixture older than 0.4.0, and the legacy single-worker configuration test runs everywhere.
The network-mode tests launch the fixture with `--bind 127.0.0.1 --tls-*` and the test
certificates in [`testdata/certs`](testdata/certs/README.md) (generated once, never at test time)
and run a job over mutual TLS, a reconnect, a cancellation past the deadline, fingerprint
mismatches and re-pinning, and the API over TLS; they skip against an Engine fixture older than
0.5.0. The video tests run the fixture's two-frame `svd` bundle end to end (archive and poster
verification, refusals, a forged archive, the size limit, an injected optional video, a linear
generate-then-animate recipe and a 1.2-pinned fixture's refusals); they skip against an Engine
fixture older than 0.6.0. `cmd/media-runtime` tests build the binary and drive
the CLI end to end, including a state directory written by the TypeScript 0.3.0 service
(`testdata/state-0.3.0`); `internal/tui` and the doctor tests compare rendered text with the
0.3.0 output. The fixture is not an inference fallback. GPU image quality and hardware
compatibility require the selected real bundle; see [implementation decisions](docs/decisions.md)
and the [compatibility record](docs/compatibility.md).
