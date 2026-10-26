---
document_type: product_specification
status: draft_proposal
spec_revision: 0.2.0
created_at: 2026-10-26
updated_at: 2026-10-26
working_name: Media Runtime
---

# Media Runtime Specification

Media Runtime is a standalone local media service operated through a CLI, an optional terminal dashboard, or an application API. It turns supported generation recipes into durable jobs and supervises replaceable inference workers. AI Server Studio connects through its existing media-runtime abstraction.

This specification describes proposed behavior. `MUST` defines a required behavior within its stated delivery stage; later stages are not requirements for the first increment. The companion [Media Engine Specification](engine-spec.md) owns the worker protocol.

## Delivery status

Revision 0.1.0 described the first complete path and listed image-to-image, LoRA, upscaling, multiple GPUs, distributed workers, video, audio and a terminal dashboard as later capabilities. Runtime 0.3.0 delivers the first complete path, the operator-experience stage (dashboard, diagnostics, installation workflow) and image-to-image and LoRA from the additional-operations stage. Revision 0.2.0 specifies every remaining deferred capability with its stage, worker-protocol dependency and acceptance rules. The [implementation plan](docs/implementation-plan.md) orders the work.

| Capability                                        | Stage                       | Needs protocol | Status in Runtime 0.3.0 |
| ------------------------------------------------- | --------------------------- | -------------- | ----------------------- |
| CLI/API, durable FIFO jobs, Diffusers worker      | First complete path         | 1.0            | Delivered               |
| Terminal dashboard, doctor, installation workflow | Operator experience         | 1.1            | Delivered               |
| Image-to-image, LoRA adapters, owned backend      | Additional media operations | 1.1            | Delivered               |
| Multiple outputs per job                          | Additional media operations | 1.2            | Specified below         |
| Upscaling                                         | Additional media operations | 1.2            | Specified below         |
| Single-file checkpoint conversion workflow        | Operator experience         | 1.2            | Specified below         |
| Automatic settings and authorized fallbacks       | Additional media operations | 1.2            | Specified below         |
| Linear typed recipes                              | Additional media operations | 1.2            | Specified below         |
| Multiple GPUs through a worker pool               | Broader execution           | 1.2            | Specified below         |
| Distributed workers and network modes             | Broader execution           | 1.2            | Specified below         |
| Video                                             | Broader execution           | 1.3            | Specified below         |
| Audio                                             | Broader execution           | 1.3            | Specified below         |
| Owned native engine as a selectable backend       | Owned inference engine      | 1.x            | Specified below         |
| Go rewrite of the service at 0.3.0 parity         | Operator experience         | 1.1            | Specified below         |

Arbitrary ComfyUI graph import, executable recipe code and a plugin marketplace remain outside the design. Existing Studio ComfyUI support remains a separate option.

The service implementation language changes before the deferred capabilities are built: Runtime 0.4.0 rewrites the 0.3.0 TypeScript service in Go at exact parity, and every later stage is implemented in Go. The public API, configuration, state directory, manifests and CLI are unchanged by the rewrite; see [Implementation language](#implementation-language).

## Product sheet

| Property                 | Proposed specification                                                                       |
| ------------------------ | -------------------------------------------------------------------------------------------- |
| Primary users            | Local AI server owners, SSH operators, application developers                                |
| Delivered tasks          | Text-to-image and image-to-image on one explicitly supported SDXL base pipeline, with LoRA   |
| Planned tasks            | Upscaling, image-to-video, text-to-audio and linear recipes that chain them                  |
| Interfaces               | CLI, HTTP API and terminal dashboard                                                         |
| Runtime implementation   | Small Go service shipped as one static binary; the 0.3.0 TypeScript service is its parity oracle |
| First inference backend  | Isolated Python worker using pinned Diffusers and PyTorch                                    |
| Future inference backend | Project-owned Media Engine implementing the same worker protocol                             |
| Persistence              | Local SQLite job metadata; local files for assets and run manifests                          |
| Scheduling               | One active generation per configured worker; bounded FIFO queue; one worker per device       |
| Default connectivity     | Loopback, authenticated local clients; no telemetry; explicit TLS network mode               |
| Integration              | Optional Studio engine; standalone use requires no Studio account or project                 |
| Packaging                | Separate runtime and backend dependency bundles with explicit compatible versions            |

## Responsibilities and boundaries

| Concern          | Runtime owns                                                             | Other owner                                                                        |
| ---------------- | ------------------------------------------------------------------------ | ---------------------------------------------------------------------------------- |
| Recipe execution | Validate inputs, resolve explicit settings, schedule operations          | Studio owns broader Plan orchestration                                             |
| Model choice     | Honor the requested model; report compatibility and availability         | Studio selects through its media model routing; standalone caller selects directly |
| Models           | Registry, component identity, configured paths, compatibility metadata   | Engine loads components and executes them                                          |
| Jobs             | Durable runtime job ID, queue, state, event history, cancellation intent | Engine supplies a session-specific execution ID                                    |
| Assets           | Durable outputs, hashes, run manifests, retention configuration          | Studio imports copies into its scoped file library                                 |
| GPU use          | Admission, worker ownership, configured memory budget, worker pool       | Engine implements allocation and reports estimates and measurements                |
| Access           | Runtime credentials and namespaces                                       | Studio enforces user/project permissions and approvals at its gateway              |

The runtime MUST NOT own conversations, RAG, agent loops, task-based model routing, or Studio project membership. It MUST NOT expose Diffusers or PyTorch objects in its public API. It MUST NOT silently change a selected model, backend, precision, image size or recipe to make a job fit.

## Initial scope

The first increment includes an explicit local model registration, one text-to-image recipe, one output image per job, queue and job commands, genuine execution events, output retrieval, cancellation, process recovery and Studio Create integration. Closing the CLI or browser MUST leave accepted jobs running in the service.

Image-to-image, LoRA and the terminal dashboard are delivered. Upscaling, multiple outputs, linear recipes, multiple GPUs, distributed workers, video and audio are specified in [Deferred capabilities](#deferred-capabilities). Arbitrary ComfyUI graph import, executable recipe code and a plugin marketplace are outside the design.

## CLI and terminal experience

`media-runtime` is a placeholder executable name. Representative proposed commands are:

```bash
media-runtime serve
media-runtime doctor
media-runtime models register ./models/studio-sdxl.json
media-runtime models list
media-runtime recipes list
media-runtime recipes inspect portrait
media-runtime run portrait --model studio-sdxl --prompt "Soft window light" --seed 42
media-runtime jobs list
media-runtime jobs watch job_123
media-runtime jobs cancel job_123
media-runtime outputs export job_123 --directory ./exports
media-runtime tui
```

- `run` returns a durable job ID; `--wait` attaches until terminal state, and `--json` gives machine-readable output. Defaults and overrides have identical meaning through CLI and API.
- CLI exit codes distinguish success, invalid input, unavailable service, and a failed or cancelled job when waiting. Disconnecting from `--wait` detaches; cancellation is an explicit operation.
- `doctor` explains worker compatibility, selected device, missing model components, writable storage and available space. Memory estimates carry their source and uncertainty.
- The dashboard presents queue, stage, model, backend, seed, output location and errors. It consumes the public API and adds no execution behavior. Images open in an external viewer initially.
- Downloads and installation are explicit operator actions. Registering a manifest or running a recipe MUST NOT fetch model weights or execute remote model code.

Later stages add `run --count`, `run --worker`, `run --model-for OP=MODEL`, `workers list`, `outputs export` of every output of a job by its ID and MIME-derived extension, and dashboard columns per worker. Every addition reuses the public API; the CLI gains no private path.

## Recipes and model bundles

A recipe is versioned declarative data. Its identity includes a schema version, recipe ID, revision and content digest. The initial recipe names one supported pipeline, declared inputs and default generation parameters. The initial resolver produces one inference operation; a later recipe may contain a linear sequence of typed media operations.

```yaml
schema_version: 1
recipe_id: portrait
revision: 1
pipeline: text-to-image
model_family: sdxl-base
inputs:
  prompt:
    type: string
    required: true
defaults:
  width: 1024
  height: 1024
  steps: 30
  guidance: 6
```

The model is selected explicitly at submission. Effective values resolve in this order: request overrides, recipe defaults, then advertised backend defaults. The runtime MUST persist the resulting request before execution; unsupported or unknown fields are errors, not discarded options. A random seed is resolved and recorded before submission to the worker.

A model registration describes a bundle of components, not merely a checkpoint filename. It includes family, component roles, local artifact paths and digests, configuration/tokenizer identity, compatible tasks, and available source/revision and license metadata. [The engine specification](engine-spec.md#model-bundles) defines the resolved form sent to a worker. Initial support targets a complete local Diffusers-format bundle; single-file checkpoint conversion is a separate capability.

Recipes MUST NOT contain arbitrary Python, shell commands, templates that execute code, or hidden network access. Backend-specific options belong in an explicitly named, advertised option namespace. An option offered by one backend does not imply support in another.

## Durable jobs and scheduling

Each job records its runtime namespace, immutable job ID, normalized request, recipe snapshot/digest, model component identities, backend and worker versions, device settings, resolved seed, timestamps, events, output records and error details. Prompt content is private local job data; operational logs and audit records omit it by default.

The public states are `queued`, `running`, `succeeded`, `failed` and `cancelled`. Running jobs additionally report a stage such as `loading`, `encoding`, `sampling`, `decoding` or `saving`. Cancellation intent is a separate `cancel_requested` flag, not proof of cancellation. Terminal states are immutable.

1. Validate the request, caller namespace, recipe and available capabilities before admitting it.
2. Atomically persist the job and an optional namespace-scoped idempotency key, then return acceptance. Reusing a key with a different request fails; an identical retry returns the same job.
3. Dispatch FIFO when the configured device worker is free. Queue capacity is configurable; excess submissions receive `429` with `Retry-After`.
4. Persist dispatch intent and a unique attempt ID before sending the worker request. Bind the accepted execution to its worker session ID. The engine has no second waiting queue.
5. Persist execution events and copy successful outputs into durable runtime storage. Mark success only after verified bytes and the run manifest are committed.

Worker events and state snapshots are authoritative for inference progress. The runtime MUST distinguish stage progress from total progress; it MUST NOT invent percentages for loading or decoding. Transient client disconnects do not affect execution.

Cancelling a queued job is immediate. Cancelling a running job sets intent and asks the worker to stop. If it exceeds a configurable grace period, the runtime may terminate only a worker it owns, record the forced termination, and reload before admitting another job. An acknowledged request alone does not settle the job. If completion already committed, cancellation returns that terminal result.

On restart, queued jobs remain queued. For dispatched jobs, reconcile the saved attempt with the same surviving worker session before taking action. A lost event connection can resume from the worker snapshot/event sequence. A job whose execution or outputs cannot be recovered becomes `failed` with `execution_interrupted` or `output_unavailable`; it is never automatically regenerated. Explicit retry creates a linked new job. Do not claim exactly-once generation across crashes.

Default dispatch allows one active generation on one configured GPU. Additional devices and concurrency require backend support and explicit resource configuration. GPU memory estimates belong to the backend; do not reuse language-model KV-cache formulas. Coexistence with Ollama initially uses operator-set budgets and worker unloading. Global GPU coordination requires a separate shared agreement; this runtime does not unilaterally control unrelated services.

## Application API

These are proposed runtime endpoints, independent of the engine endpoints. All except coarse `/health` require a bearer credential; credentials and ownership are checked on every referenced resource.

| Endpoint                                    | Meaning                                                                         |
| ------------------------------------------- | ------------------------------------------------------------------------------- |
| `GET /health`                               | Coarse service liveness, no model or filesystem details                         |
| `GET /v1/runtime`                           | Runtime/API versions, backend readiness, limits and supported protocol versions |
| `GET /v1/workers`                           | Configured workers, device identity, readiness and loaded identity              |
| `GET /v1/catalog`                           | Operator-only inspection of approved artifact handles                           |
| `GET /v1/models`                            | Registered model bundles and current availability                               |
| `POST /v1/models`                           | Operator-only registration of an existing local bundle                          |
| `GET /v1/adapters`, `POST /v1/adapters`     | Registered LoRA adapters; operator-only registration                            |
| `GET /v1/recipes`                           | Recipe identities, inputs and supported tasks                                   |
| `GET /v1/recipes/{recipe_id}`               | One recipe with its typed input declarations and operations                     |
| `POST /v1/recipes`                          | Operator-only registration of declarative recipes                               |
| `POST /v1/inputs`, `GET/DELETE /v1/inputs/…`| Durable, namespace-owned uploaded inputs                                        |
| `POST /v1/jobs/validate`                    | Resolve and validate a submission without enqueuing or loading a model          |
| `POST /v1/jobs`                             | Durable acceptance, `202` with `job_id` and current state                       |
| `GET /v1/jobs`                              | Paginated jobs visible to the caller namespace                                  |
| `GET /v1/jobs/{job_id}`                     | Durable state, stage, operations, effective settings and outputs                |
| `GET /v1/jobs/{job_id}/events`              | SSE events with persistent sequence IDs and `Last-Event-ID` resume              |
| `POST /v1/jobs/{job_id}/cancel`             | Record cancellation intent or return the existing terminal state                |
| `GET /v1/jobs/{job_id}/outputs/{output_id}` | Stream an owned output by opaque ID                                             |
| `GET /v1/jobs/{job_id}/manifest`            | Export provenance without credentials or absolute private paths                 |

Submission carries `recipe_id`, `model_id`, `inputs`, `parameters`, and an optional explicit `backend_id`. A credential has a default namespace; only a privileged integration credential may supply an authorized caller namespace. A body-supplied user or namespace ID is not authentication. Standalone mode uses a default owner namespace without requiring projects.

Job events use `job_state`, `stage`, `progress`, `output` and `error`, each with `job_id`, monotonic `sequence` and timestamp. This is a new runtime event vocabulary; it does not rename Studio's existing SSE events. After event retention expires, the server explicitly requires a fresh state snapshot rather than silently skipping history.

Errors return `error.code`, a readable `error.message`, optional `error.field`, and `request_id`. Invalid requests use `400`, authorization uses `401/403`, inaccessible resources use `404`, conflicts use `409`, and unavailable backends use `503`. A returned error MUST NOT include secrets or uncontrolled worker stderr.

## Dependency policy

The runtime relies on other libraries and services as little as is practical. Standard library first. A third-party dependency is accepted only when it does something unsafe or impractical to own (SQLite durability, TLS), is pinned by checksum, and is isolated in one package so it can be replaced. Adding one is a recorded decision in [docs/decisions.md](docs/decisions.md), which keeps a ledger of every dependency with its justification. From 0.4.0 the service binary carries one third-party module, the SQLite driver; HTTP, SSE, TLS, JSON, the CLI and the terminal dashboard are standard library and owned code. No external service is used: no telemetry, no downloads, no registries.

## Storage and access

Runtime metadata, active models and working files stay on local hot storage. Outputs are immutable after publication and include MIME type, size and digest. Export does not remove originals. Cleanup policies are explicit and configurable, preserve active jobs and undelivered worker outputs, and expose expiry in metadata. A formerly available output returns `410` when its authorized metadata survives but its bytes no longer do.

The runtime binds loopback by default, requires credentials, rejects browser origins by default, and keeps operator routes separate from generation permission. Prompts, outputs and credentials are excluded from normal logs; local state files receive restrictive permissions. Future network modes require explicit configuration and authenticated encrypted transport. No inference operation initiates a model download or telemetry request.

Each backend runs in its own pinned environment or package. The runtime supervises owned child processes, probes protocol/capability compatibility, tracks process identity, bounds logs and handles clean shutdown. Externally managed services are never killed or reconfigured as though the runtime owned them. Python dependencies do not enter Studio's application environment.

## Deferred capabilities

Each capability below is a separately advertised feature with its own stage. A capability is available to a caller only when the configured worker advertises it on the negotiated protocol minor; otherwise the submission fails with `unsupported` naming the field, exactly as image-to-image and adapters do today. None of them relaxes a rule above.

### Multiple outputs per job

Protocol 1.2 lets an execution finalize several outputs. The runtime commits every output the worker advertises for the task, verifying each by size, digest and format signature before success, and publishes them under opaque output IDs (`image`, `image_2`, …, `video`, `poster`, `audio`). A job succeeds only when every advertised output is committed; a partial set is `output_unavailable`.

- `parameters.count` (text-to-image, 1 to the worker's advertised limit) requests a batch in one execution. The job records every output's seed (`seed + k − 1`) so one image can be reproduced alone with `count: 1`.
- `max_output_bytes` becomes a per-MIME map with explicit defaults; a worker output larger than its limit fails the job rather than being truncated.
- `outputs export` writes every output with an extension derived from its MIME type; the manifest lists every output with its digest.
- Studio import receives every output as a separate Studio-owned file with provenance; the gateway decides what Create shows.

### Upscaling

`upscale` is a recipe pipeline that enlarges an uploaded PNG by the registered model's fixed factor using a bundle of the `esrgan` family. The registration carries `family: esrgan` and its `scale`; the runtime validates it through the worker's advertised loaders and refuses a model whose family does not serve the recipe's pipeline.

- The default recipe `enlarge` takes `image` and accepts `tile`/`tile_overlap` overrides. Width and height resolve to the input's dimensions and MUST NOT be overridden; the output is exactly input × scale, bounded by the worker's `output_side` limit, checked at validation.
- No seed is resolved or recorded; the manifest records the model digest and precision instead.
- `doctor` and `models show` report the factor and the per-tile memory estimate basis.
- The single-file conversion workflow below covers converting upscaler releases to safetensors; the runtime never converts weights.

### Single-file checkpoint conversion workflow

Conversion is performed by Engine's offline `convert` subcommand; the runtime adds the operator workflow around it. `models inspect` recognizes a bundle directory containing `conversion.json`, reports the source checkpoint digest, reference bundle digest and converter versions, and prints the registration document. `models register` records that provenance in the model's `source`, and manifests carry it for every job that used the bundle. The runtime MUST NOT invoke Python or perform conversion itself, and registration of an unconverted single-file checkpoint fails with `unsupported` naming the format.

### Automatic settings and authorized fallbacks

Where the worker advertises `auto_options`, a submission's `backend_options` may set an option to `auto`. The runtime passes it through, persists the worker's `resolved_options` and `resolutions` in the job before dispatch, and the manifest records them; a retry re-resolves rather than copying resolved values.

A submission may carry `authorized_fallbacks` drawn from the worker's advertised `fallbacks` (`cpu`, `lower_precision`). The runtime forwards them on the load body only when the credential's configuration allows fallbacks (`allow_fallbacks`, default false), records `applied_fallbacks` from the load operation in the job, emits a `stage` event naming the fallback, and shows it in `jobs watch`, the dashboard and the manifest. A fallback never changes the model, recipe or generation parameters, and an unauthorized failure still settles the job as `failed` with the worker's code.

### Linear typed recipes

Recipe schema version 2 declares an ordered list of typed operations. Each operation names its task, model family, inputs bound from the submission or from an earlier operation's output, defaults and optional adapters. The runtime resolves every operation before admission and runs them in order, each as its own worker execution.

```json
{
  "schema_version": 2,
  "recipe_id": "portrait-4k",
  "revision": 1,
  "inputs": { "prompt": { "type": "string", "required": true } },
  "operations": [
    { "op_id": "generate", "task": "text-to-image", "model_family": "sdxl-base",
      "inputs": { "prompt": { "from": "input:prompt" } },
      "defaults": { "width": 1024, "height": 1024, "steps": 30, "guidance": 6 } },
    { "op_id": "enlarge", "task": "upscale", "model_family": "esrgan",
      "inputs": { "image": { "from": "op:generate/image" } } }
  ],
  "publish": ["enlarge/image"]
}
```

- Submission `model_id` selects the model for the first operation; `models: {op_id: model_id}` selects the others. Every operation MUST resolve to a registered model of its declared family and to a task the worker advertises; otherwise admission fails with the operation named in `error.field`.
- The job records `operations[]` with per-operation state, attempt, session, execution, effective settings and outputs. `stage` events carry `op_id`; `progress` carries `operation_index` and `operation_count` beside the worker's stage progress, and the runtime MUST NOT compute a single percentage across operations.
- An operation's outputs are committed durably before the next operation starts. An image output feeding the next operation is uploaded to the worker as an input under the job's ownership; the worker never sees a runtime path. The worker is reloaded when the next operation's bundle, adapters or options differ, using the existing load-identity rules.
- `publish` lists the outputs exposed by the job's output endpoints and export; unlisted intermediate outputs are retained with the job and follow the same retention. The manifest lists every operation and output.
- Cancellation between operations is honored before dispatching the next; a failed operation fails the job with `error.operation`. Retry creates a new job that runs every operation again; partial resume is not offered.
- Schema version 1 recipes remain valid and are treated as a single operation; `recipes inspect` and `GET /v1/recipes/{id}` return the typed declarations Studio's native-workflow path needs for input mapping.

### Multiple GPUs through a worker pool

The configuration lists workers instead of one worker. Each entry has a `worker_id`, a loopback URL or an owned command, its credential, expected `backend_id`, `device` and an optional per-worker memory budget. Every worker is one Engine process on one device; a device is listed once, and the runtime refuses a configuration or a probe result where two workers report the same device identity.

- The scheduler keeps one active execution per worker. The FIFO head is dispatched to an idle, ready worker whose loaded identity (bundle, adapters, options) matches when one exists, else to any idle worker, so that FIFO order is preserved per worker without reordering by model.
- A submission may pin `worker_id`; an unknown or unsuitable worker fails admission. Without pinning, a job may run on any worker that advertises its task, family and protocol minor.
- Jobs, events, manifests, `jobs list`, the dashboard and `doctor` record and display the worker ID and device identity. Restart reconciliation runs per worker session; a lost worker interrupts only its own job.
- Queue capacity stays global; `GET /v1/runtime` reports per-worker readiness and `GET /v1/workers` lists the pool. A single `worker` entry remains valid and is treated as a pool of one.

### Distributed workers and network modes

A worker entry may name a remote `https://` URL when it also names a client certificate, private key and CA file; the runtime then speaks mutual TLS and pins the worker's certificate fingerprint reported in `/v1/info`. A remote entry MUST NOT carry a `command`: a remote worker is never owned, so the cancellation deadline records interruption only after the worker itself reports a terminal state or the session changes, never through termination.

- The remote worker's catalog is its own; a registration's artifact handle must resolve there and digests prove the bundle is the same. Inputs and outputs already cross the connection as verified bytes; nothing else changes in the job model.
- Remote entries have separate `remote_timeout_ms` and reconnection settings; a lost connection resumes from the worker snapshot and event sequence like a local one.
- The runtime's own API gains a network mode: `bind` other than loopback requires server TLS material and keeps browser origins refused unless `allowed_origins` lists them explicitly. Credentials, namespaces and operator separation are unchanged; a plain-HTTP non-loopback bind MUST be refused at startup.

### Video

`image-to-video` is a recipe pipeline on the `svd` family (protocol 1.3). The default recipe `animate` takes an uploaded PNG whose dimensions MUST match the input and fall within the model's advertised sizes, and resolves `frames`, `fps`, `motion`, `noise_augmentation`, `steps`, `seed` and `decode_chunk` from request, recipe and worker defaults.

- Outputs are `frames` (`application/zip`, one lossless PNG per frame plus `frames.json`) and `poster` (`image/png`); both are verified by size and digest, the archive by its ZIP signature and the presence of `frames.json`, before success. The job and manifest record frame count, frame rate and duration from the worker's output record.
- When the worker advertises `video/mp4` in its output formats, the job additionally commits `video`; the runtime never encodes video itself and never requires it.
- The per-MIME output limit for the frame archive defaults to 256 MiB; export writes `.zip`, `.png` and, when present, `.mp4`. The dashboard shows frames and duration; frames and videos open in external tools.
- Studio imports the archive (and the video when present) as Studio-owned files with provenance and the poster as the preview.

### Audio

`text-to-audio` is a recipe pipeline on the `stable-audio-open` family (protocol 1.3). The default recipe `soundscape` takes `prompt` and optional `negative_prompt` and resolves `duration_seconds`, `steps`, `guidance` and `seed`. The output is `audio` (`audio/wav`); the job and manifest record sample rate, channels and actual duration. The runtime verifies the RIFF/WAVE header and size before success and exports `.wav`. The dashboard shows duration; playback happens in an external player.

### Implementation language

Runtime 0.4.0 is a Go rewrite of the 0.3.0 service, delivered before any other deferred capability so that the parity target is as small as it will ever be and so that the worker pool and network-mode stages are built on Go's concurrency and TLS primitives rather than ported later.

- The rewrite MUST preserve the HTTP API, error codes, SSE semantics, configuration file, environment variables, SQLite schema and state directory layout, manifest schema, CLI commands, flags, output and exit codes of 0.3.0. A 0.3.0 state directory MUST be served by 0.4.0 without migration.
- Parity is proven by running the 0.3.0 integration and dashboard test suites against the Go binary with Engine 0.3.0 before those suites are ported to Go.
- The service ships as one static binary with no cgo and exactly one third-party module, a pure-Go SQLite driver. The dashboard renders with ANSI escapes as the 0.3.0 dashboard does, and the CLI uses the standard flag package; dependencies are pinned in the module files and the toolchain version is recorded with the release.
- The typed gateway client used by Studio remains a TypeScript package maintained in this repository; it is the only TypeScript retained, and it gains every later output, recipe and worker type.
- 0.4.0 MUST NOT include any capability beyond 0.3.0. The TypeScript service is frozen when the rewrite starts and retired after the first Go feature release.

### Owned native engine as a selectable backend

When Engine ships its native backend, the runtime accepts `backend_id: native-sdxl` beside `diffusers-sdxl` and `owned-sdxl`, records `ownership: engine` from the worker's `/v1/info`, and otherwise treats it identically: the caller chooses the backend explicitly, a request for a backend other than the running one is rejected, and no recipe, scheduler or parameter is reinterpreted. The runtime's integration suite runs unchanged against the native worker and is part of its acceptance.

## AI Server Studio integration

Add one Studio `MediaRuntime` adapter for this service. It maps health, model discovery, request validation, generation, status, progress, cancellation, output retrieval and provenance through the existing interface. The new runtime kind and registry entry require corresponding backend/frontend capability handling; configuration alone is not integration.

Studio keeps its own job identity and stores the runtime job ID as its native handle. It enforces permissions before dispatch and uses namespace isolation to keep direct runtime credentials from exposing other users' jobs. Outputs imported into Studio become Studio-owned file objects with provenance; runtime retention cannot delete those copies.

Personal and project use MUST have equivalent generation, status, cancellation and output behavior. The browser talks only to Studio's gateway. Existing model routing chooses the requested model; this adapter does not add a second picker. Native recipes may later use Studio's sanctioned native-workflow path, with declared input mappings and the same job lifecycle.

The first Studio increment must be runnable from Create with honest setup, loading, failure, Stop and output states. The standalone runtime is independently useful; its CLI milestone alone does not complete a Studio implementation phase.

Later stages extend the adapter rather than the gateway contract: multiple outputs and new media types import as additional Studio file objects; linear recipes expose their typed input declarations through `GET /v1/recipes/{id}` for the native-workflow path; worker pools and remote workers are invisible to Studio beyond the worker ID in provenance. Studio work is tracked in its own repository and is not a deliverable of this runtime's plan.

## Development sequence

| Stage                       | Delivered behavior                                                                                                                                      |
| --------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------- |
| First complete path         | CLI/API, one registered bundle and recipe, Diffusers worker, durable queue, cancellation, saved image/manifest, Studio Create integration (delivered)    |
| Operator experience         | Terminal dashboard, improved diagnostics, explicit package/model installation workflows (delivered); Go rewrite at parity; conversion provenance workflow |
| Additional media operations | Image-to-image and LoRA (delivered); multiple outputs, upscaling, automatic settings with authorized fallbacks, linear typed recipes                    |
| Owned inference engine      | Owned pipeline backend (delivered); native backend behind the unchanged worker protocol; caller explicitly chooses it                                    |
| Broader execution           | Worker pool for multiple GPUs, remote workers over mutual TLS with API network mode, video, audio                                                        |

## Decisions before implementation

Record final product/repository names, the exact reference model and hardware, dependency pins, supported Go/Python versions and distribution licenses. The control plane is Go from 0.4.0 (the 0.1.0 proposal of a TypeScript control plane was implemented through 0.3.0 and is superseded); the isolated Python worker remains until Engine's native backend exists. Select concrete queue, storage and cancellation defaults and publish them as configuration rather than unexplained hard limits.

For the deferred stages, record before each starts: the per-MIME output limits, the pool scheduling tie-break when several idle workers match, the TLS material format and rotation expectations for remote workers and API network mode, the video and audio reference bundles and their licenses, and the retention rule for intermediate operation outputs. The next implementation step is the Go rewrite at 0.3.0 parity, then protocol 1.2 support (multiple outputs, upscaling, conversion provenance, automatic settings and fallbacks) followed by linear typed recipes; the [implementation plan](docs/implementation-plan.md) sequences the rest.
