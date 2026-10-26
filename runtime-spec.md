---
document_type: product_specification
status: draft_proposal
spec_revision: 0.1.0
created_at: 2026-10-26
working_name: Media Runtime
---

# Media Runtime Specification

Media Runtime is a standalone local media service operated through a CLI, an optional terminal dashboard, or an application API. It turns supported generation recipes into durable jobs and supervises replaceable inference workers. AI Server Studio connects through its existing media-runtime abstraction.

This specification describes proposed behavior. `MUST` defines a required behavior within its stated delivery stage; later stages are not requirements for the first increment. The companion [Media Engine Specification](engine-spec.md) owns the worker protocol.

## Product sheet

| Property                 | Proposed specification                                                            |
| ------------------------ | --------------------------------------------------------------------------------- |
| Primary users            | Local AI server owners, SSH operators, application developers                     |
| Initial task             | Text-to-image, one explicitly supported SDXL base pipeline                        |
| Interfaces               | CLI and HTTP API first; terminal dashboard next                                   |
| Runtime implementation   | Small TypeScript service on a supported Node.js LTS release                       |
| First inference backend  | Isolated Python worker using pinned Diffusers and PyTorch                         |
| Future inference backend | Project-owned Media Engine implementing the same worker protocol                  |
| Persistence              | Local SQLite job metadata; local files for assets and run manifests               |
| Initial scheduling       | One active generation on one configured GPU; bounded FIFO queue                   |
| Default connectivity     | Loopback, authenticated local clients; no telemetry                               |
| Integration              | Optional Studio engine; standalone use requires no Studio account or project      |
| Packaging                | Separate runtime and backend dependency bundles with explicit compatible versions |

## Responsibilities and boundaries

| Concern          | Runtime owns                                                             | Other owner                                                                        |
| ---------------- | ------------------------------------------------------------------------ | ---------------------------------------------------------------------------------- |
| Recipe execution | Validate inputs, resolve explicit settings, schedule operations          | Studio owns broader Plan orchestration                                             |
| Model choice     | Honor the requested model; report compatibility and availability         | Studio selects through its media model routing; standalone caller selects directly |
| Models           | Registry, component identity, configured paths, compatibility metadata   | Engine loads components and executes them                                          |
| Jobs             | Durable runtime job ID, queue, state, event history, cancellation intent | Engine supplies a session-specific execution ID                                    |
| Assets           | Durable outputs, hashes, run manifests, retention configuration          | Studio imports copies into its scoped file library                                 |
| GPU use          | Admission, worker ownership, configured memory budget                    | Engine implements allocation and reports estimates and measurements                |
| Access           | Runtime credentials and namespaces                                       | Studio enforces user/project permissions and approvals at its gateway              |

The runtime MUST NOT own conversations, RAG, agent loops, task-based model routing, or Studio project membership. It MUST NOT expose Diffusers or PyTorch objects in its public API. It MUST NOT silently change a selected model, backend, precision, image size or recipe to make a job fit.

## Initial scope

The first increment includes an explicit local model registration, one text-to-image recipe, one output image per job, queue and job commands, genuine execution events, output retrieval, cancellation, process recovery and Studio Create integration. Closing the CLI or browser MUST leave accepted jobs running in the service.

Image-to-image, LoRA, upscaling, multiple GPUs, distributed workers, video, audio and a terminal dashboard are later capabilities. Arbitrary ComfyUI graph import, executable recipe code and a plugin marketplace are outside the initial design. Existing Studio ComfyUI support remains a separate option.

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
| `GET /v1/models`                            | Registered model bundles and current availability                               |
| `POST /v1/models`                           | Operator-only registration of an existing local bundle                          |
| `GET /v1/recipes`                           | Recipe identities, inputs and supported tasks                                   |
| `POST /v1/recipes`                          | Operator-only registration of declarative recipes                               |
| `POST /v1/jobs/validate`                    | Resolve and validate a submission without enqueuing or loading a model          |
| `POST /v1/jobs`                             | Durable acceptance, `202` with `job_id` and current state                       |
| `GET /v1/jobs`                              | Paginated jobs visible to the caller namespace                                  |
| `GET /v1/jobs/{job_id}`                     | Durable state, stage, effective settings and outputs                            |
| `GET /v1/jobs/{job_id}/events`              | SSE events with persistent sequence IDs and `Last-Event-ID` resume              |
| `POST /v1/jobs/{job_id}/cancel`             | Record cancellation intent or return the existing terminal state                |
| `GET /v1/jobs/{job_id}/outputs/{output_id}` | Stream an owned output by opaque ID                                             |
| `GET /v1/jobs/{job_id}/manifest`            | Export provenance without credentials or absolute private paths                 |

Submission carries `recipe_id`, `model_id`, `inputs`, `parameters`, and an optional explicit `backend_id`. A credential has a default namespace; only a privileged integration credential may supply an authorized caller namespace. A body-supplied user or namespace ID is not authentication. Standalone mode uses a default owner namespace without requiring projects.

Job events use `job_state`, `stage`, `progress`, `output` and `error`, each with `job_id`, monotonic `sequence` and timestamp. This is a new runtime event vocabulary; it does not rename Studio's existing SSE events. After event retention expires, the server explicitly requires a fresh state snapshot rather than silently skipping history.

Errors return `error.code`, a readable `error.message`, optional `error.field`, and `request_id`. Invalid requests use `400`, authorization uses `401/403`, inaccessible resources use `404`, conflicts use `409`, and unavailable backends use `503`. A returned error MUST NOT include secrets or uncontrolled worker stderr.

## Storage and access

Runtime metadata, active models and working files stay on local hot storage. Outputs are immutable after publication and include MIME type, size and digest. Export does not remove originals. Cleanup policies are explicit and configurable, preserve active jobs and undelivered worker outputs, and expose expiry in metadata. A formerly available output returns `410` when its authorized metadata survives but its bytes no longer do.

The runtime binds loopback by default, requires credentials, rejects browser origins by default, and keeps operator routes separate from generation permission. Prompts, outputs and credentials are excluded from normal logs; local state files receive restrictive permissions. Future network modes require explicit configuration and authenticated encrypted transport. No inference operation initiates a model download or telemetry request.

Each backend runs in its own pinned environment or package. The runtime supervises owned child processes, probes protocol/capability compatibility, tracks process identity, bounds logs and handles clean shutdown. Externally managed services are never killed or reconfigured as though the runtime owned them. Python dependencies do not enter Studio's application environment.

## AI Server Studio integration

Add one Studio `MediaRuntime` adapter for this service. It maps health, model discovery, request validation, generation, status, progress, cancellation, output retrieval and provenance through the existing interface. The new runtime kind and registry entry require corresponding backend/frontend capability handling; configuration alone is not integration.

Studio keeps its own job identity and stores the runtime job ID as its native handle. It enforces permissions before dispatch and uses namespace isolation to keep direct runtime credentials from exposing other users' jobs. Outputs imported into Studio become Studio-owned file objects with provenance; runtime retention cannot delete those copies.

Personal and project use MUST have equivalent generation, status, cancellation and output behavior. The browser talks only to Studio's gateway. Existing model routing chooses the requested model; this adapter does not add a second picker. Native recipes may later use Studio's sanctioned native-workflow path, with declared input mappings and the same job lifecycle.

The first Studio increment must be runnable from Create with honest setup, loading, failure, Stop and output states. The standalone runtime is independently useful; its CLI milestone alone does not complete a Studio implementation phase.

## Development sequence

| Stage                       | Delivered behavior                                                                                                                        |
| --------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------- |
| First complete path         | CLI/API, one registered bundle and recipe, Diffusers worker, durable queue, cancellation, saved image/manifest, Studio Create integration |
| Operator experience         | Terminal dashboard, improved diagnostics, explicit package/model installation workflows                                                   |
| Additional media operations | Image-to-image, LoRA or upscaling as separately advertised capabilities; linear typed recipes where needed                                |
| Owned inference engine      | Additional backend behind the unchanged worker protocol; caller explicitly chooses it                                                     |
| Broader execution           | More model families, devices or media types according to actual use                                                                       |

## Decisions before implementation

Record final product/repository names, the exact reference model and hardware, dependency pins, supported Node/Python versions and distribution licenses. Use the proposed TypeScript control plane and isolated Python worker unless implementation evidence supports a smaller arrangement. Select concrete queue, storage and cancellation defaults and publish them as configuration rather than unexplained hard limits.

The next implementation step is the worker contract and the first complete path above. Future engine development follows [the engine migration sequence](engine-spec.md#progressive-inference-ownership).
