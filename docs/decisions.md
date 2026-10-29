# Implementation decisions — 0.1.0

- Product/repository names: Media Runtime / Media-Runtime and Media Engine / Media-Engine.
- Runtime: Node 24.19.0 LTS, TypeScript 5.8.3, SQLite via Node's built-in `node:sqlite` API.
  npm dependencies are pinned with package-lock.json. Source is MIT licensed.
- Reference model: stabilityai/stable-diffusion-xl-base-1.0, complete Diffusers safetensors bundle,
  EulerDiscreteScheduler; no refiner. The exact deployed identity is the registration's SHA-256
  digest of every local component/configuration/tokenizer file. Source revision is operator metadata;
  this project distributes no weights and makes no claim to have downloaded an upstream revision.
- Reference hardware target: Linux x64, NVIDIA RTX 4090 24 GiB, CUDA-enabled PyTorch 2.6.0,
  float16, SDPA attention, no offloading, 1024×1024, 30 steps, guidance 6. These are explicit defaults,
  not hardware certification. CPU float32 is an explicitly selectable diagnostic platform.
- Worker: Python 3.12.14; independently locked inference environment. Protocol `media-worker/1.0`.
  No Python or tensor dependencies enter Studio or Runtime.
- Queue: one active job, 32 waiting jobs; 250 ms polling; 10 s cancellation grace for owned children;
  30 s worker HTTP timeout; 32 MiB per published output. Infinite output retention by default;
  operators can set expiry. Event history is retained with job metadata and never silently pruned.
- Public recipes accept JSON declarative data; the spec's YAML example expresses the same structure.
  One operation, one PNG, seed resolved before admission. Euler is the only advertised sampler/scheduler.
- Model registration and admission verify all content through the worker. This deliberately favors
  correctness over startup latency. Approved model directories must be immutable during use.
- Backend estimates remain explicitly unknown until measured; no language-model memory formula is used.
- Source licenses (MIT), Diffusers/PyTorch dependency licenses, and model weights' CreativeML Open
  RAIL++-M terms are separate. Operators supply and license their own weights.
- Implemented and tested control-plane behavior is distinct from real GPU inference validation;
  this workspace has neither CUDA hardware nor model weights. Later roadmap stages remain future work.

# Implementation decisions — 0.2.0

- Protocol minor negotiation: the worker's `supported_protocols` list is authoritative; Runtime
  speaks the highest minor it also implements and records it per job. A 1.0 worker still serves
  text-to-image; image-to-image requires 1.1 and is refused with `unsupported` otherwise.
- Inputs: PNG only, verified by signature/IHDR/IEND and size limits (`max_input_bytes`, default
  32 MiB), stored durably under the state directory with namespace ownership. The worker receives
  bytes under a session-scoped handle at dispatch, never a path. Inputs follow
  `output_retention_days` and are never removed while a job references them.
- Image-to-image never resizes: the resolved width/height default to the image's dimensions and
  differing overrides are errors. `strength` resolves request → recipe → worker task default
  (0.75 in Engine, 0.6 in the default `variation` recipe); `floor(steps × strength)` is recorded
  as `effective_steps` and drives progress totals.
- Backends: `diffusers-sdxl` (reference) and `owned-sdxl` (owned pipeline) are both accepted.
  The launched worker decides; `worker.backend_id` in the config pins expectations and a request's
  `backend_id` must match. Backend options are keyed by the running backend only.
- Dashboard: consumes `/v1/runtime` and `/v1/jobs?order=newest` once per second and offers only
  existing API operations (cancel). Images open externally via `outputs export`.
- Diagnostics: `doctor` exits 3 when the worker is unavailable, a device is not ready, a registered
  model is missing files, storage is unwritable or no model is registered. Memory estimates come
  from Engine's safetensors-header count and always carry their basis and exclusions.
- Installation workflow: operators download bundles out of band, map them in `artifact_catalog`,
  run `models inspect` (no hashing, no registration, no paths in the output), then `models register`.
  Registration and `models show --verify` hash every byte through the worker.

# Implementation decisions — 0.3.0

- LoRA adapters are registry objects like models: an opaque catalog handle to a directory with one
  safetensors file, hashed per file, validated by the worker (PEFT layout only). Requests name
  `adapters: [{adapter_id, scale}]`; a recipe may carry a default list that a request replaces
  wholesale. Scales are 0–2; at most four distinct adapters.
- Adapters are part of the worker load identity, not the execution: the runtime reloads when the
  adapter set or any scale differs, exactly as for precision. This keeps the owned pipeline free to
  merge deltas in place and keeps per-execution cost equal to the base model.
- Kohya-format files are refused by the worker rather than silently converted; operators convert
  to the PEFT layout first. The runtime surfaces the worker's `unsupported` error unchanged.

# Implementation decisions — 0.4.0

- The service is rewritten in Go at exact 0.3.0 parity before any deferred capability is built.
  Reasons: the parity target is smallest now (about 4,000 lines, one dependency); the worker pool
  and mutual-TLS stages are simpler on goroutines and `crypto/tls` than ported afterwards; a
  static binary removes the Node installation from operator hosts; the maintainer's language
  preference. The "same language as Studio" argument from 0.1.0 is retained only where it matters:
  the typed gateway client stays TypeScript.
- Parity oracle: the 0.3.0 `test/integration.test.mjs` and `test/tui.test.mjs` suites run against
  the Go binary with Engine 0.3.0's fixture before being ported to Go tests.
- Pure-Go SQLite (`modernc.org/sqlite`), same schema and pragmas, so state directories migrate
  without conversion. No cgo anywhere in the binary.
- The TypeScript service is frozen at 0.3.0 when the rewrite starts and archived after 0.5.0.
- Language split across the project: Go for the runtime, Python for the reference and owned
  Engine backends, Rust recommended for Engine's native backend (decided in Engine phase E6).
- Toolchain as built and tested: Go 1.24.7 linux/amd64, `CGO_ENABLED=0`-compatible; `go.mod`
  declares `go 1.24`. `modernc.org/sqlite` v1.38.2 is pinned in `go.mod`/`go.sum`.
- Parity method: the 0.3.0 suites exercised the TypeScript classes in process (they replaced
  `worker.json` and `worker.probe` at run time), so they could not launch a separate binary
  unchanged. They were ported case for case to Go tests that construct the runtime in process the
  same way, with explicit test hooks on the worker client (`SetHooks`) standing in for monkey
  patching, plus binary-level CLI tests. Both 0.3.0 Node suites passed against Engine 0.3.0
  immediately before the port, and every ported case passes against the same fixture.
- Byte-level parity: records are built in an ordered JSON model (`internal/jsonv`) that keeps
  JavaScript's key order, number formatting and string escaping, so stored jobs, manifests, API
  bodies and the canonical digests (idempotency keys, recipe, bundle and adapter digests) are the
  ones 0.3.0 produced. Tests pin digests and rendered dashboard/doctor text produced by 0.3.0.
  Validation reports the first failing field path the way zod's first issue did.
- Concurrency: HTTP handlers run concurrently with the scheduler goroutine, so one mutex
  serializes job read-modify-write sequences (cancellation, admission, dispatch and every
  scheduler save, which re-reads cancellation intent first) and SQLite uses one connection. The
  scheduler still runs at most one tick at a time.
- CLI flags use the standard `flag` package, re-parsed around positionals so options may follow
  subcommands as before; Go also accepts single-dash spellings, and unknown-flag messages differ
  in wording (exit code 2 is unchanged).
- The 0.3.0 tag and a `typescript-0.3` maintenance branch are release-management steps for the
  repository owner when this release is tagged; the 0.3.0 sources remain in history.
- Dependency policy: standard library first; a third-party module is accepted only when it does
  something unsafe or impractical to own, is pinned by checksum and isolated in one package. Every
  module is listed in the ledger below with its justification; adding one is a recorded decision.

## Dependency ledger — Go runtime (0.4.0)

| Module               | Why it is not owned code                                                   |
| -------------------- | -------------------------------------------------------------------------- |
| modernc.org/sqlite   | Durable atomic admission, idempotency and event sequences; pure Go, no cgo |

The driver brings its own transitive modules (`modernc.org/libc`, `mathutil`, `memory`,
`github.com/dustin/go-humanize`, `github.com/google/uuid`, `github.com/mattn/go-isatty`,
`github.com/ncruces/go-strftime`, `github.com/remyoudompheng/bigfft`, `golang.org/x/exp`,
`golang.org/x/sys`), all pinned by `go.sum`; none is imported by runtime code.

Standard library only: `net/http` (API and SSE), `crypto/tls` (reserved for network mode and remote workers),
`encoding/json`, `flag`, `os/exec` (owned worker supervision), `archive/zip` (frame archive
checks), terminal dashboard via ANSI escapes. Rejected: `bubbletea`/`tview` (the 0.3.0 dashboard
already needs no library), `cobra` (stdlib `flag` suffices), any HTTP router or validation
framework (strict decoding with `DisallowUnknownFields` and hand-written range checks).

The gateway client package keeps TypeScript's zero-dependency profile: it uses `fetch` and no
third-party module.

# Implementation decisions — 0.5.0

- Protocol 1.2 is negotiated when the worker lists it; 1.0 and 1.1 remain. Each 1.2 field is
  admitted only against a 1.2 worker that advertises it and is otherwise refused with
  `unsupported` naming the field (`parameters.count`, `precision`, `authorized_fallbacks`,
  `recipe_id` for `upscale`, `family` at registration).
- Output limits: `max_output_bytes` is a per-MIME map with defaults `image/png` 32 MiB,
  `application/zip` 256 MiB, `video/mp4` 256 MiB and `audio/wav` 64 MiB; the 0.4.0 number is
  read as the PNG limit. The video and audio defaults are unmeasured guesses until those stages.
- `count` is 1–4 in the request schema and further bounded by the worker's `limits.count`. The
  expected output set is the worker's `task_parameters.<task>.outputs` with `repeated_outputs`
  expanded to `id_2 … id_N`; an extra or missing output fails the job (`output_unavailable`).
  Output `k` of a batch records seed `seed + k − 1`.
- Upscale requests carry only `width`, `height` (the input's), `tile` and `tile_overlap`. The
  registration's `scale` is the factor the worker reports when it validates the bundle; the
  runtime checks `max(width, height) × scale ≤ limits.output_side` before admission and the worker
  checks again. Model and recipe families must match before any worker call.
- Conversion provenance comes only from the directory's `conversion.json`: a registration may
  repeat it (it must match) but never supply it alone. A directory holding one `.safetensors` file
  and no `model_index.json`/`config.json` is a bare checkpoint and is refused with field `format`.
- Fallback policy: credentials default to `allow_fallbacks: false`; a request naming fallbacks with
  such a credential is `403 fallbacks_not_allowed`, field `authorized_fallbacks`. The one
  readiness relaxation is a 1.2 worker whose selected device is unavailable (`device_available:
  false`, dependencies present) when the request authorizes `cpu`: it is admitted and dispatched
  so the worker can apply that fallback. Without the authorization the 503 refusal is unchanged.
  Applied fallbacks are recorded from the loaded bundle when the job is dispatched, so a job that
  reuses an already loaded fallback bundle records it too.
- `auto` stays in the request and the load identity (the worker reports the requested options as
  the loaded options), so an identical request does not reload; resolved values are recorded,
  never copied into a retry.
- Manifest schema 3 adds `family`, `scale` (upscale), `device_identity`, `resolved_options`,
  `resolutions`, `applied_fallbacks` and every output with its seed. Schema 2 manifests are never
  rewritten.

# Implementation decisions — 0.6.0

- Operation model: every job carries `operations[]` and `current_operation`; the scheduler drives
  only the current operation, and its worker identities (`attempt_id`, `session_id`,
  `execution_id`, `worker_input_id`, `load_operation_id`, `dispatch_phase`, `worker_sequence`,
  `applied_fallbacks`, `released`) are mirrored at the top level, where single-operation jobs have
  always recorded them. Top-level `task`, `model`, `input`, `adapters` and `backend` are the first
  operation's. A stored job without `operations` is read as one operation; nothing is rewritten
  until the job is next saved.
- Selection: `model_id`, `parameters` and `adapters` apply to the first operation; `models`
  names every later operation explicitly (no default to `model_id`, since the model is always
  selected explicitly); later operations' parameters come only from recipe defaults, bound number
  inputs and worker defaults. A bound number that conflicts with an explicit parameter is refused.
  Load options and fallbacks are job-wide.
- Names: operation IDs are lowercase words joined by single underscores (at most 32 characters),
  so `<op_id>__<output_id>` is unambiguous. Published outputs keep the worker's output ID unless
  two published outputs share it, in which case both carry the prefix; intermediates always do.
  A `publish` entry or binding names a base output (`image`); publishing it publishes each image
  of a batch, and binding it passes the first. `prompt`, `negative_prompt` and `image` keep their
  schema-1 types in every recipe, so a submission using only those names is validated exactly as
  before.
- Intermediate retention: intermediates follow `output_retention_days` exactly like published
  outputs, are kept with the job (never deleted when the job succeeds) and are served to the job's
  owner; `publish` controls exposure, not retention. Committed operation outputs of a job that
  later failed or was cancelled are kept and expire the same way.
- Durability order per operation: output bytes and their directories are fsynced and the
  operation is recorded as succeeded before the next operation is dispatched; the manifest is
  written once, after the last operation, before the job is recorded as succeeded. Retry is a new
  job running every operation again; no partial resume is offered.
- Compatibility: schema-1 jobs keep manifest schema 3, their events carry no operation fields and
  their output IDs are unchanged; schema 4 manifests and operation-annotated events are written for
  schema-2 recipes. A schema-2 recipe holds at most 8 operations and 16 publish entries.
- No new dependency; the ledger is unchanged.

# Implementation decisions — 0.7.0

- Configuration: `workers` lists the pool, each entry with `worker_id`, `url`, `token_env`,
  optional `command`, `backend_id`, `device` and `memory_budget_mb`, with the 0.6.0 defaults for
  `url` and `token_env`. The 0.6.0 single `worker` object is still accepted and read as a pool of
  one named `default`; giving both is an error. Worker IDs, configured devices and the URLs of
  externally managed workers must be distinct. `device` is the operator's expectation only: a
  worker whose reported device differs is unavailable, and the worker's own report is what jobs
  record. `memory_budget_mb` is displayed beside the worker's own budget and enforces nothing; the
  worker's budget remains the one applied.
- Execution: one scheduler loop per worker, each on its own poll ticker, and at most one running
  job per worker. Dispatch decisions are taken under the job mutex, so two loops never take the
  same job; a loop reads the other workers' readiness from their last probe, which is at most one
  poll interval old and corrects itself on the next probe. Queue capacity stays global.
- Admission: every configured worker (only the pinned one when `worker_id` is given) is probed and
  checked against the request's tasks, families, protocol minor, options and fallbacks. The workers
  that pass are the job's `candidates`, each recorded with the minor it negotiated. The first
  candidate in configuration order resolves the job and validates its bundles, so a submission
  hashes its bundles once, not once per worker; with no candidate the first worker's refusal is
  returned, exactly as a pool of one refuses. An unknown pinned worker is `400 unknown_worker`,
  field `worker_id`. Every worker must therefore serve the same artifact catalog (same handles,
  same bytes); a worker that cannot load a bundle fails the job with its own load error.
  Registration and `models show --verify` validate through the first available worker.
- Scheduling: the FIFO head goes to an idle, ready, eligible worker whose loaded identity (bundle,
  load options, adapter set and scales) equals the job's, else to the first idle eligible worker.
  Eligible means the pinned worker, or a candidate still negotiating at least the minor it did at
  admission and still advertising every operation's task, family loader and backend. The tie-break
  among several matching workers, and among several non-matching ones, is configuration order:
  deterministic, visible in the configuration file (list the preferred device first) and free of
  hidden state; least-recently-used was rejected because it makes placement depend on history.
- Reordering default: `allow_reordering` is false, so FIFO is literal. When the head is pinned to
  a busy worker, or no idle worker can run it, nothing behind it is dispatched even if a worker is
  free; this can idle a worker and is the documented trade-off for predictable order. With
  `allow_reordering` a later job may use a worker the head cannot, and the head still takes the
  first worker it can use.
- Linear jobs: every operation of a schema-2 job runs on the worker its first operation was
  dispatched to. The job stays running between operations and so keeps the worker; the next
  operation reloads there when its load identity differs. Moving an operation to another worker
  would let unrelated jobs interleave, split one job's reconciliation across sessions and gain
  nothing for intermediates, which are uploaded from durable storage either way.
- Device identity: two workers are the same device when their 1.2 `device_identity` has the same
  `host_id`, `device` and `index` (`pid` is the process, not the device). Workers before 1.2 report
  no identity and are not compared. With `strict_devices` (default true) a duplicate at start
  refuses to start the service; with it false, both workers stay configured but unavailable. A
  duplicate that appears while serving (a worker restarted onto another's device) always makes both
  unavailable until one changes, since a running service cannot refuse to start.
- Attribution: a job records `worker_id` (null while queued) and `device_identity` at dispatch, as
  does every operation's `worker_id`; the worker facts in each operation's `backend` (protocol,
  device, device identity, ownership, budget, worker version) are refreshed from the dispatching
  worker, while the capability facts the job was admitted against are kept. The `running`
  `job_state` event carries `worker_id` and, from 1.2, `device_identity`. Manifests 3 and 4 gain
  `worker_id` (and schema 4 the job's `device_identity`); their schema numbers are unchanged.
- Recovery: each worker reconciles only its own running job against its own session, and a lost
  owned worker interrupts only its job. Jobs written before 0.7.0 that were dispatched read as
  `worker_id: "default"`; never-dispatched ones have none and may run on any worker. A running job
  whose worker is no longer configured fails `execution_interrupted` at start; a queued job whose
  pinned worker or every candidate is no longer configured fails `backend_unavailable` instead of
  holding the queue.
- Operator surfaces: `GET /v1/runtime` keeps `worker` (the first worker) and adds `workers`;
  `doctor` exits 3 when any worker is unavailable. A pool of one keeps the 0.6.0 doctor and
  dashboard layout exactly; a larger pool prints one section and header line per worker and a
  worker column.
- No new dependency; the ledger is unchanged.
