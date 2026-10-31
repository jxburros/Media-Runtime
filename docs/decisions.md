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

# Implementation decisions — 0.8.0

- Configuration: a worker entry (pool or legacy single `worker`) is remote exactly when its URL is
  `https://`; any host name or IP is accepted there, while `http://` stays loopback only. A remote
  entry requires `tls` with `ca_file`, `cert_file` and `key_file` and may give `pinned_sha256` (64
  hex digits, stored lowercase) and `remote_timeout_ms` (default `worker_timeout_ms`). A remote
  entry with `command` is refused at validation, as are `tls` or `remote_timeout_ms` on an
  `http://` entry, so ownership can never be configured for a process on another host. Relative
  TLS paths resolve against the working directory. The spec's "reconnection settings" need no new
  field: a lost connection is retried every poll interval and reconciled from the worker snapshot
  and event sequence, exactly as for a local worker.
- TLS material format: PEM certificates (leaf first), unencrypted PEM private keys (PKCS#8, PKCS#1
  or SEC 1), PEM CA bundles. Files are read once at start with Engine's mode rules: a key readable
  by group or others, or a certificate or CA file writable by them, refuses the start with a
  message naming the field and the `chmod` to run. Rotation of any file is a restart; nothing is
  reloaded, generated or fetched, and revocation lists are not consulted.
- Worker transport: one `http.Transport` per remote entry with a `tls.Config` of TLS 1.3 minimum
  (Engine's network mode serves nothing lower), `ca_file` as the only root (system roots are not
  consulted, so a public CA cannot vouch for a worker), the client certificate, ordinary host name
  verification against the URL, and a `VerifyPeerCertificate` comparing the SHA-256 of the leaf's
  DER to the pin or the recorded fingerprint. No `ClientSessionCache` is set, so there is no
  session resumption and the check runs on every handshake. HTTP/2 is not attempted (Engine serves
  HTTP/1.1). The runtime polls worker snapshots rather than streaming the worker's SSE, so "SSE
  over the same transport" means that every worker request, including any stream, goes through
  this one transport; there is no second client.
- Fingerprint pinning: with `pinned_sha256`, every handshake must present that certificate. Without
  it, the first successful contact (chain and host verified) records the fingerprint per worker ID
  in the state directory (`worker_certificates`), together with the URL it was seen at, and every
  later handshake, including after a restart, must match. The record is keyed by worker ID, not URL:
  moving a worker to a new address keeps its identity. The probe additionally requires `/v1/info`
  to report `scheme: https` and a `certificate_sha256` equal to the certificate presented; a worker
  without these fields (before Engine 0.5.0) is refused as `protocol_mismatch`. A configured pin
  replaces the recorded fingerprint once it has been seen, which is the rotation path: pin the new
  fingerprint, and optionally remove the pin afterwards. A refused certificate is
  `certificate_mismatch` (HTTP 503 class) and is treated like an unreachable worker: a running job
  keeps its dispatch identity for reconciliation rather than failing, because nothing was said to
  the worker.
- Ownership and cancellation: `Start` never launches and `Stop` never signals a remote worker.
  After `cancellation_grace_ms` a remote job is not forced; the runtime keeps sending cancel and
  polling. A `cancelled` snapshot settles it as `cancelled`; a `succeeded` or `failed` snapshot
  reported after the deadline settles it as `failed`, `execution_interrupted`, `forced_termination:
  false`, and commits nothing, mirroring the owned case where work past the deadline is discarded.
  Before the deadline a worker that finishes anyway commits as today. A new session interrupts the
  job through the existing session check. Loopback external workers keep their 0.7.0 behavior.
- API network mode: `bind` is an IP address (default `127.0.0.1`; host names are refused, as in
  Engine). A bind that is not loopback requires `tls` (`cert_file`, `key_file`); the check runs at
  configuration and again in the listener, so no code path opens a plain-HTTP non-loopback socket.
  The API serves TLS 1.2 or later (browsers and gateways vary) and asks for no client certificate:
  the bearer credential authorizes every request, and credentials, namespaces and operator
  separation are unchanged. The startup record adds `scheme`, `bind` and `certificate_sha256`.
- Origins: `allowed_origins` is an exact allow-list of `scheme://host[:port]` origins, compared
  lowercase; wildcards, paths and `null` are refused. An allowed origin receives
  `Access-Control-Allow-Origin` (with `Vary: Origin`), and a preflight `OPTIONS` is answered 204
  with the methods and headers the API reads; the credential is still required. The default empty
  list keeps every browser origin refused with 403 as before.
- CLI and clients: `MEDIA_RUNTIME_URL` may be `https://`; `MEDIA_RUNTIME_CA` is a PEM CA bundle
  that replaces the system roots. The CLI refuses `http://` to a non-loopback host so the
  credential never travels in clear text. The gateway client relies on Node's trust store
  (`NODE_EXTRA_CA_CERTS` for a private CA), since `fetch` has no per-request trust option and a
  dispatcher library would be a dependency.
- Diagnostics: each worker report adds `transport` (`http` or `mtls`), `remote`, `latency_ms` (the
  last `/v1/info` round trip) and, for remote workers, `certificate_sha256` (expected),
  `certificate_source` (`pinned` or `first_contact`), `presented_sha256` and the worker's
  `ownership_available`. `doctor` prints a transport line per worker; reports without these fields
  print as in 0.7.0.
- Test material: `testdata/certs` holds a test CA (key discarded), worker, rotated-worker, runtime
  and client certificates, EC P-256, ten years from 2026-10-29, generated once with the commands in
  its README; tests copy them to private modes and never generate certificates.
- `crypto/tls`, reserved in the 0.4.0 ledger, is now used for both paths. No new dependency; the
  ledger and `go.sum` are unchanged.

# Implementation decisions — 0.9.0

- Protocol: `media-worker/1.3` is negotiated when the worker lists it; 1.0–1.2 remain. The task
  needs minor 3 (`TaskMinor`) and the family minor 3 (`FamilyMinor`), so against a 1.2 worker the
  recipe is `unsupported`, field `recipe_id`, and registration `unsupported`, field `family`. The
  `tasks` list, not the minor, decides what a worker serves: a 1.3 worker that does not list
  `image-to-video` (Engine's owned backend) refuses it the same way.
- Reference bundle: `stabilityai/stable-video-diffusion-img2vid-xt` (25 frames, 1024×576), with
  `stable-video-diffusion-img2vid` (14 frames) as the smaller alternative, in Diffusers format
  with safetensors weights, as Engine 0.6.0 records. Stability AI licenses them separately from
  this project (the Stability AI Community License for current revisions); the operator checks
  the model card of the installed revision and supplies the weights. Nothing here downloads them.
  `examples/model.svd.json` names the reference repository; no real weights were used.
- Bundle facts: the sizes an svd bundle serves and its default frame count are the worker's
  answer when it validates the bundle at registration (`video_sizes`, `default_frames`, read from
  `video.json` and the unet configuration), recorded on the model like an upscaler's `scale`.
  Admission uses the recorded facts on every candidate worker; `limits.video_sizes` is consulted
  only for a model without them, because it describes the loaded bundle, not the requested one.
- Resolution order for image-to-video: worker `task_defaults` and task `defaults` (fps 7, motion
  127, noise_augmentation 0.02, decode_chunk 8), the model's `default_frames`, recipe defaults,
  the image's size, then the request; the seed is resolved as for diffusion tasks. The worker has
  no steps default for video, so `animate` carries `steps: 25` (the reference pipeline's
  default) and a recipe without one requires `parameters.steps`. `capabilities.defaults` is never
  merged into a video operation (it would add guidance, sampler and scheduler). Every value is
  explicit in the stored request, so a retry and the manifest describe exactly what ran.
- Refusals: parameters that do not exist for the task (`guidance`, `sampler`, `scheduler`,
  `strength`, `count`, `tile`, `tile_overlap`), a prompt and adapters are `invalid_request` with
  the field; a video parameter the worker does not list in `task_parameters.image-to-video
  .parameters` is `unsupported`; a value outside the worker's advertised range (`video_steps`,
  `frames`, `fps`, `motion`, `noise_augmentation`, `decode_chunk`) is `invalid_request` naming
  the parameter, mirroring the worker's own code; a size the model does not serve is
  `invalid_request`, field `video_size`, the worker's field name. Nothing is clamped or resized.
- Output set: the worker's `task_parameters.image-to-video.outputs` with their advertised types,
  exactly as for images. `video` (`video/mp4`) is therefore required when, and only when, the
  worker advertises it, and the runtime never encodes or requires a video itself. Engine 0.6.0 and
  0.7.0 ship no encoder (Engine 0.6.0 decided against its optional MP4 extra), so no fixture
  produces one; the optional path is tested with bytes injected through a test-only response hook
  on the worker client (`SetResponseHook`), and the MP4 check is unit-tested.
- Archive verification (owned code on `archive/zip`): `PK\x03\x04` at offset 0; the central
  directory must parse; member names are unique; `frames.json` (at most 4 KiB) is an object of
  exactly `fps`, `width`, `height` and `frames`, positive integers, equal to the output record;
  the members are exactly `frames.json` and `000001.png` … one per frame, each starting with a PNG
  signature and IHDR of the declared size (only the first 24 bytes of each member are read); the
  record's `duration_seconds` equals `frames / fps` exactly (Engine's value is the same IEEE
  division). The record's `width`, `height`, `frames` and `fps` must also equal the request, so a
  worker can never drop or re-time frames unnoticed. Storage method is not required to be
  `stored`: compression is the worker's business. MP4: an `ftyp` box at offset 4 whose 32-bit (or
  64-bit large) size is at least 8 and fits the file; the runtime does not parse further.
- Streaming commit: every output is now copied from the worker into `<file>.tmp` through a
  SHA-256 writer with a limit of the MIME's `max_output_bytes` plus one byte, verified from the
  file (size, digest, signature), fsynced, closed and renamed; a failed check removes the
  temporary file and fails the job `output_unavailable`. The fsync order (bytes, directories,
  manifest, database) is unchanged. `outputs export` streams likewise.
- Output limits: the 0.5.0 defaults stand — `application/zip` and `video/mp4` 256 MiB (the
  specification's archive default), `image/png` 32 MiB, `audio/wav` 64 MiB — and remain
  unmeasured guesses: by estimate (about 1–2 MiB per lossless 1024×576 PNG) the reference 25
  frames are tens of MiB and the protocol's 64 frames stay under the limit, but no real archive
  was measured here. Operators raise the map entry; the runtime never shrinks an output to fit.
- Manifest schema 5: the schema-4 operations layout (every operation with its model, inputs,
  settings, worker identity and outputs; published and intermediate outputs) plus `media` on each
  operation, written for every job that runs a video (from 0.10.0 also an audio) operation,
  single-operation or linear, so one consumer handles every media job. Image-only jobs keep schema
  3 (one operation) and 4 (linear) byte for byte; no older manifest is rewritten. Jobs and
  operations carry the same `media` object (`type`, `frames`, `fps`, `duration_seconds`, `width`,
  `height`, `encoded_video`); a linear job's top-level `media` is its last media operation's.
- Linear recipes: output types are declared per base output ID (`image`, `poster` PNG; `frames`
  ZIP; `video` MP4); only a PNG output may be bound to an image input, checked at registration.
  `TaskParameterInputs` lets number inputs bind to `seed`, `steps` and the video parameters.
- Dashboard: progress carrying a `unit` renders as `<stage> c/t <unit>` (also upscale tiles). The
  media column is shown only when a listed job has video facts, as the worker column is shown only
  for a pool, so the image dashboards keep the 0.3.0 layout pinned by the tests.
- No new dependency; the ledger and `go.sum` are unchanged (`archive/zip` was already listed for
  the frame archive checks in 0.4.0).

# Implementation decisions — 0.10.0

- Protocol: unchanged (`media-worker/1.3`); Engine 0.7.0 added text-to-audio within the minor,
  so availability follows the worker's `tasks` and `loaders`, not the minor: Engine 0.6.0 and a
  reference backend without the `inference-audio` extra negotiate 1.3 and still refuse the recipe
  (`unsupported`, `recipe_id`) and registration (`unsupported`, `family`). The runtime does not
  read `missing_task_dependencies`; the worker's own refusal, and `doctor`'s task list, say what
  is served.
- Reference bundle: `stabilityai/stable-audio-open-1.0` (44.1 kHz stereo, up to 47.55 s; the
  default range 1–47 s) in Diffusers format with safetensors weights, as Engine 0.7.0 records.
  Stability AI licenses it under the Stability AI Community License, separately from this project;
  its T5 encoder derives from `google-t5/t5-base` (Apache-2.0). The operator checks the installed
  revision's model card (the repository is gated) and supplies the weights; nothing here downloads
  them. `examples/model.audio.json` names the reference repository; no real weights were used.
- Bundle facts: the worker's validation at registration reports `sample_rate`, `channels`,
  `hop_length` and `duration_range`; they are recorded on the model, like the svd facts, and
  admission uses them on every candidate. `limits.duration_seconds` describes the loaded bundle and
  is consulted only for a model without a recorded range.
- Resolution order: worker `task_defaults` and task `defaults` (guidance 7), recipe defaults, then
  the request; the seed is resolved. The worker requires `steps` and `duration_seconds` and
  defaults neither, so `soundscape` carries `steps: 100` (the library pipeline's default) and
  `duration_seconds: 10` (the plan's example); `capabilities.defaults` is never merged into an
  audio operation.
- Steps: the request schema's steps bound is now the widest task's, 200 (the protocol's audio
  range); the image tasks keep 150, checked at resolution and at recipe registration with the
  field they always had (`parameters.steps`, or the recipe's `inputs` refinement), so no existing
  request changes outcome or error field. The worker's `audio_steps` and `guidance` limits apply.
- Duration: a request outside the model's `duration_range` is `invalid_request`, field
  `parameters.duration_seconds`; the clip is never trimmed or padded by the runtime. The worker
  rounds up to whole latent frames, so the committed clip may be longer than requested by less
  than `hop_length / sample_rate` seconds; the commit check refuses a clip shorter than the request
  or longer than that bound (a 1e-9 s tolerance absorbs binary rounding of Engine's own division).
  The job records the requested duration in its request and the actual one in `media` and the
  output record.
- WAV verification (owned code on `encoding/binary`): exactly the protocol's canonical layout —
  `RIFF` with size = file size − 8, `WAVE`, a 16-byte `fmt ` chunk of PCM (format 1), 16 bits,
  channels ≥ 1, block alignment = channels × 2 and byte rate = rate × alignment, then `data` at
  offset 36 holding a non-zero whole number of frames and ending at the end of the file. Other
  chunk layouts (`LIST`, extensible format, padding) are refused because Engine never writes them;
  a worker that does would need a recorded decision here. The record's `sample_rate`, `channels`,
  `samples` and `duration_seconds` (= samples / sample_rate exactly) must be the header's, and the
  rate and channels the model's.
- Output limit: `audio/wav` keeps the 0.5.0 default of 64 MiB, an unmeasured guess: by arithmetic
  a 47 s 44.1 kHz stereo 16-bit clip is about 8 MiB (the protocol's 600 s ceiling would be about
  101 MiB and needs a larger limit), and the fixture's clips are a few hundred KiB.
- Manifest schema 5 also covers audio jobs (operations layout with `media`), with no shape change
  from 0.9.0; audio `media` is `{type: audio, sample_rate, channels, samples, duration_seconds}`.
- Parameters across tasks: `TaskParameterNames` lists what each task takes. A video or audio
  parameter on a task that does not take it is refused by name before resolution; 0.9.0 let such
  a parameter reach the strict resolved-parameter schema, which refused it without a field.
- Catalog: stable-audio-open roles are listed in Engine's order (transformer, vae, text_encoder,
  tokenizer, projection_model, scheduler) with transformer, vae, text_encoder and projection_model
  as the weight roles, so a registration the runtime builds has the digest Engine's own fixture
  computes for the same directory; `tokenizer/tokenizer.json` is required, as the worker requires
  it, and the scheduler class is `CosineDPMSolverMultistepScheduler`.
- No new dependency; the ledger and `go.sum` are unchanged.
