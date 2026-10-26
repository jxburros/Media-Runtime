# Media Runtime implementation plan — deferred capabilities

This plan turns [runtime-spec.md](../runtime-spec.md) revision 0.2.0 into ordered, releasable
increments of the service. The first increment rewrites the 0.3.0 TypeScript service in Go at
exact feature parity; every later increment is written in Go. It covers every capability the specification marks
"Specified below": multiple outputs, upscaling, the conversion provenance workflow, automatic
settings and authorized fallbacks, linear typed recipes, the worker pool for multiple GPUs,
distributed workers and network modes, video, audio and the native backend. Out-of-scope items
(ComfyUI graph import, executable recipe code, plugin marketplaces) have no phase here. Studio
adapter changes are noted where the runtime's contract changes but are delivered in Studio's
repository.

Each phase names its Runtime release, the Engine release and worker-protocol minor it needs, the
files it touches, the tests that prove it and the acceptance rule that closes it. Phases are
ordered so that each one is useful on its own and so that Runtime never requires an Engine release
that does not exist yet; the companion [Engine plan](https://github.com/jxburros/Media-Engine/blob/main/docs/implementation-plan.md)
sequences the worker side.

## Baseline (Runtime 0.3.0)

| Area                 | Where                                                                                               | State                                                     |
| -------------------- | --------------------------------------------------------------------------------------------------- | --------------------------------------------------------- |
| Contracts            | `src/contracts.ts` (`PROTOCOLS`, `BACKENDS`, `TASKS`, `parameters`, `submission`, `recipeSchema`, `registration`, `Job`) | 1.0/1.1, two tasks, schema-1 recipes, one `image` output |
| Configuration        | `src/config.ts` (one `worker`, `artifact_catalog`, `credentials`, limits)                           | Loopback only, one worker                                 |
| Core                 | `src/runtime.ts` (`resolve`, `submit`, `cancel`, `tick`, `commitOutputs`, `doctor`, `cleanup`)      | One running job, `dispatch_phase` loading/executing       |
| Worker client        | `src/worker.ts` (`WorkerClient`: `start`, `probe`, `json`, `response`, `stop`)                      | Owned child or external loopback worker                   |
| HTTP API             | `src/server.ts`                                                                                     | Routes listed in the spec through adapters/inputs         |
| Persistence          | `src/store.ts` (`jobs`, `events`, `idempotency`, `models`, `recipes`, `adapters`, `inputs`; job data is a JSON column) | No schema migration needed for new job fields |
| CLI and dashboard    | `src/cli.ts`, `src/tui.ts`, `src/models.ts` (catalog inspection), `src/client.ts` (gateway client)  |                                                           |
| Tests                | `test/integration.test.mjs` (drives Engine's `tests/serve_fixture.py`), `test/tui.test.mjs`         | Needs Media-Engine beside the checkout                    |
| Decisions            | `docs/decisions.md`                                                                                 | 0.1.0–0.3.0 entries                                       |

Conventions every phase keeps: zod schemas are `.strict()`, unknown fields are errors, every
capability is checked against the worker's advertised `capabilities` and the negotiated protocol
minor before admission, nothing is silently changed to fit, success is recorded only after
verified bytes and the manifest are fsynced, prompts stay out of logs, and the CLI uses only the
public API.

## Release map

| Runtime | Needs Engine | Protocol | Phase                                                                     |
| ------- | ------------ | -------- | ------------------------------------------------------------------------- |
| 0.4.0   | 0.3.0        | 1.1      | R0 Go rewrite at 0.3.0 parity                                             |
| 0.5.0   | 0.4.0        | 1.2      | R1 Multiple outputs, upscaling, conversion provenance, auto/fallbacks     |
| 0.6.0   | 0.4.0        | 1.2      | R2 Linear typed recipes                                                   |
| 0.7.0   | 0.4.0        | 1.2      | R3 Worker pool for multiple GPUs                                          |
| 0.8.0   | 0.5.0        | 1.2      | R4 Distributed workers and API network mode                               |
| 0.9.0   | 0.6.0        | 1.3      | R5 Video                                                                  |
| 0.10.0  | 0.7.0        | 1.3      | R6 Audio                                                                  |
| 1.0.0   | 1.0.0        | 1.3      | R7 Native backend acceptance and compatibility record                     |

Engine 0.4.0 does not depend on the runtime, so R0 and Engine phase E1 proceed in parallel.
The TypeScript service is frozen at 0.3.0 the day R0 starts; no feature is implemented twice.

Every release keeps negotiating 1.0 and 1.1 so an older Engine still serves text-to-image, and
every feature that needs a newer minor is refused with `unsupported` naming the field, as
image-to-image and adapters are today.

## Phase R0 — Runtime 0.4.0, Go rewrite at 0.3.0 parity

The service is rewritten in Go before any deferred capability is built, while the parity target is
smallest: about 4,000 lines of TypeScript, one dependency and a fully specified API, config,
state directory, manifest and CLI surface. Phases R3 and R4 are the ones Go simplifies most (a
goroutine per worker, mutual TLS from the standard library), so the rewrite lands before them.

### R0.1 Stack and layout

- Go 1.23 or later; a single static binary `media-runtime` with no cgo. Module layout:
  `cmd/media-runtime` (CLI entry), `internal/contracts` (request/recipe/registration shapes and
  strict validation), `internal/config`, `internal/store` (SQLite), `internal/worker` (Engine
  client and child supervision), `internal/runtime` (resolve, submit, scheduler, commit, doctor,
  cleanup), `internal/api` (HTTP and SSE), `internal/tui`, `internal/catalog` (inspection).
- SQLite through `modernc.org/sqlite` (pure Go) with the same schema, WAL mode and FULL
  synchronous setting as `src/store.ts`, so an existing 0.3.0 state directory opens unchanged.
- `net/http` for the API and SSE; `encoding/json` with `DisallowUnknownFields` and hand-written
  range checks replacing zod's `.strict()` schemas; `crypto/tls` reserved for R4; `bubbletea` for
  the dashboard; stdlib `flag` with subcommands for the CLI, keeping every command, flag, exit
  code and `--json` shape from `src/cli.ts`.
- Dependencies are pinned in `go.mod`/`go.sum`; the release records the Go toolchain version in
  `docs/decisions.md` as the TypeScript release recorded Node.

### R0.2 Parity contract

Everything below is identical to 0.3.0, byte for byte where a format is involved:

- The HTTP API in [runtime-spec.md](../runtime-spec.md), including error codes, `request_id`,
  `429` with `Retry-After`, `410` for expired outputs, SSE sequence IDs and `Last-Event-ID`
  resume, namespace rules and operator separation.
- `runtime.config.json` fields and environment variables (`MEDIA_RUNTIME_TOKEN`,
  `MEDIA_RUNTIME_URL`, `MEDIA_RUNTIME_NAMESPACE`, `MEDIA_RUNTIME_CONFIG`).
- The state directory: SQLite schema, `inputs/`, `outputs/<job>/`, manifest schema 2, file modes
  0700/0600, atomic write and fsync order (bytes, manifest, then database), the exclusive process
  lock.
- Worker supervision: startup record parsing, per-launch token, session and attempt
  reconciliation, cancellation grace and forced termination semantics, stdout drain, no stderr
  retention.
- CLI commands, flags, output text used by `test/tui.test.mjs`, exit codes 0/2/3/4.
- Recipes, registrations, adapters, idempotency, retention and cleanup behavior.

### R0.3 Proving parity

- Keep `test/integration.test.mjs` and `test/tui.test.mjs` as the oracle: add a
  `MEDIA_RUNTIME_BIN` switch so they launch the Go binary instead of `node dist/cli.js`, and run
  them against Engine 0.3.0's fixture. The rewrite is done when both suites pass unmodified apart
  from that switch.
- Port the suites to Go tests (`internal/.../_test.go` driving the real binary and Engine's
  `tests/serve_fixture.py`) in the same release, then delete the Node test harness; `npm test` is
  replaced by `go test ./...` in the README and compatibility record.
- Open a 0.3.0 state directory with the Go binary and run `jobs list`, `outputs export` and a
  new job to prove migration-free continuity.

### R0.4 TypeScript retirement

- `src/client.ts` moves to a small package of its own (`gateway-client/`) and stays TypeScript:
  it is Studio's typed entry point and gains the new output, recipe and worker types in each later
  phase exactly as the plan already states.
- The rest of `src/` is deleted in 0.4.0; the 0.3.0 tag and a `typescript-0.3` maintenance branch
  remain for one feature release and are archived after 0.5.0 ships.
- README, CHANGELOG, `docs/decisions.md` (0.4.0 entry: Go toolchain, SQLite driver, the reasons
  for the rewrite and for keeping the gateway client in TypeScript) and `docs/compatibility.md`.

Acceptance: both 0.3.0 test suites pass against the Go binary with Engine 0.3.0, a 0.3.0 state
directory is served unchanged, and no feature beyond 0.3.0 is present.

### File references in later phases

Phases R1 through R7 were written against the TypeScript modules. After R0, read them through
this mapping; responsibilities move, names do not:

| TypeScript module   | Go package                                   |
| ------------------- | -------------------------------------------- |
| `src/contracts.ts`  | `internal/contracts`                         |
| `src/config.ts`     | `internal/config`                            |
| `src/store.ts`      | `internal/store`                             |
| `src/worker.ts`     | `internal/worker` (plus `internal/pool` in R3) |
| `src/runtime.ts`    | `internal/runtime`                           |
| `src/server.ts`     | `internal/api`                               |
| `src/cli.ts`        | `cmd/media-runtime`                          |
| `src/tui.ts`        | `internal/tui`                               |
| `src/models.ts`     | `internal/catalog`                           |
| `src/client.ts`     | `gateway-client/` (TypeScript, unchanged role) |
| `test/*.test.mjs`   | Go tests driving the binary and Engine's fixture |

## Phase R1 — Runtime 0.5.0, protocol 1.2

### R1.1 Negotiate 1.2 and record worker identity

- `src/contracts.ts`: `PROTOCOLS` adds `media-worker/1.2`; `negotiate()` is unchanged.
- `src/runtime.ts` `resolve()`: copy `device_identity`, `loaders`, `auto_options`, `fallbacks`
  and `task_parameters` from `caps`/`info` into `job.backend`; the manifest (schema 3) records
  device identity. `doctor()` prints it.
- Tests: Engine's fixture advertises 1.2; the integration suite asserts the negotiated minor and
  that a fixture pinned to 1.1 (existing `serve_fixture.py` option) still runs text-to-image.

### R1.2 Multiple outputs and `count`

- `src/contracts.ts`: `parameters.count` (int 1–4, optional); `submission` forbids `count` on
  non-text-to-image recipes at `resolve()` time using the worker's `task_parameters`. Add an
  `outputRecord` schema with optional `frames`, `fps`, `duration_seconds`, `sample_rate`,
  `channels` for later phases.
- `src/config.ts`: `max_output_bytes` becomes `max_output_bytes: { "image/png": 33554432,
  "video/mp4": 268435456, "audio/wav": 67108864 }` with a numeric form accepted as the PNG
  value for existing configs.
- `src/runtime.ts` `commitOutputs()`: iterate `snapshot.outputs`, require the set of IDs the
  worker advertised for the task (`caps.task_parameters[task].outputs`), verify each by size,
  SHA-256 and a per-MIME signature check (PNG today; MP4/WAV checks added in R5/R6 behind a
  `signatures` map in `contracts.ts`), write `outputs/<job>/<output_id>.<ext>`, fsync all, then
  the manifest, then the database. Record per-output seeds (`seed + k − 1`) in the job.
- `src/server.ts`: `/v1/jobs/{id}/outputs/{output_id}` serves any committed ID with its MIME
  type; `publicJob()` lists all outputs.
- `src/cli.ts`: `run --count N`; `outputs export` writes every output with a MIME-derived
  extension and refuses to overwrite. `src/tui.ts` shows the output count.
- `src/client.ts`: typed output records for Studio's adapter.
- Tests: `count: 3` commits three outputs with distinct digests; a missing advertised output
  fails the job with `output_unavailable`; export writes three files; a 1.1 fixture refuses
  `count`.

### R1.3 Upscaling

- `src/contracts.ts`: `TASKS` adds `upscale`; `recipeSchema` `model_family` becomes an enum
  (`sdxl-base`, `esrgan`); `parameters` adds `tile` and `tile_overlap`; `registration` adds
  `family` (default `sdxl-base`) and, for `esrgan`, the worker-reported `scale` is stored on the
  model record. Default recipe `enlarge` (`pipeline: upscale`, `inputs.image` required,
  `defaults: { tile: 0, tile_overlap: 16 }`).
- `src/runtime.ts` `resolve()`: for `upscale`, require `inputs.image`, forbid `prompt`, `seed`,
  `steps`, `guidance`, `count` and `adapters`; resolve width/height to the input and refuse
  overrides; check `input × scale ≤ caps.limits.output_side`; skip seed resolution and record
  `seed: null`. Model/recipe family must match and the worker's `loaders` must list the family.
- `src/models.ts`: catalog inspection recognizes a single-file `esrgan` layout (one safetensors
  file plus `config.json`) and prints a registration document with `family: esrgan`.
- `src/server.ts` `POST /v1/models`: validate through the worker with the family-aware bundle;
  `GET /v1/models/{id}` shows `scale`.
- `src/cli.ts`: `run enlarge --model upscaler --image in.png`; `doctor` lists family and scale.
- Manifest schema 3: `task`, `family`, `scale`, no seed for upscale.
- Tests: register the fixture `esrgan` bundle, run `enlarge`, assert output dimensions, the
  tile progress events, the oversize refusal at validation, and that `--seed` is rejected.

### R1.4 Conversion provenance workflow

- `src/models.ts`: when a handle's directory contains `conversion.json`, parse and validate it
  (`source_sha256`, `reference_bundle_digest`, `converter`, `mapped_keys`), include it in
  `models inspect` output and in the printed registration's `source.conversion`.
- `src/contracts.ts` `registration.source`: optional `conversion` object with those fields.
- `src/runtime.ts`: job `model` snapshot and manifest carry `source.conversion`.
- `src/server.ts` `POST /v1/models`: a handle whose directory is a bare single-file checkpoint
  (one safetensors file, no `model_index.json`, no `config.json`) is refused with `unsupported`,
  field `format`, pointing at Engine's `convert` subcommand in the message.
- README: the end-to-end workflow (download → `media-engine convert` → `models inspect` →
  `models register`).
- Tests: a fixture directory with `conversion.json` registers with provenance; a bare
  checkpoint directory is refused with the documented code.

### R1.5 Automatic settings and authorized fallbacks

- `src/contracts.ts`: `loadOptions.precision` accepts `"auto"`; `submission.authorized_fallbacks`
  (array of `cpu` | `lower_precision`, optional).
- `src/config.ts`: credential field `allow_fallbacks` (default false).
- `src/runtime.ts` `resolve()`: allow `auto` only when `caps.auto_options` lists the option and
  the protocol is 1.2; forward `authorized_fallbacks` only when the credential allows them and
  `caps.fallbacks` lists each; persist the validate response's `resolved_options` and
  `resolutions` on `job.backend`. In `tick()`, when the load operation succeeds, copy
  `applied_fallbacks` into the job, emit a `stage` event `{stage: "loading", fallback}` and keep
  it in the manifest. The load-identity comparison uses resolved values so a repeated request
  does not reload.
- `src/cli.ts`: `run --fallback cpu --fallback lower_precision`; `jobs watch` prints applied
  fallbacks; `src/tui.ts` marks a job that ran on a fallback.
- Tests: unauthorized fallback fails exactly as in 0.3.0; authorized fallback is recorded in the
  job, events and manifest; a credential without `allow_fallbacks` gets `403` with field
  `authorized_fallbacks`.

### R1.6 Release

- Version 0.5.0 in `internal/contracts` and the release tag; `CHANGELOG.md`; README sections;
  `docs/decisions.md` 0.5.0 entry (per-MIME limits, count limit, fallback policy); examples:
  `examples/recipe.enlarge.json`, `examples/model.upscaler.json`, config with the output limit
  map.
- Add `docs/compatibility.md`: Runtime version, negotiated minors, Engine versions tested,
  fixture version.
- `go test ./...` with Engine 0.4.0 beside the checkout; also run against Engine 0.3.0's fixture to
  prove the 1.1 path is unchanged.

Acceptance: every R1 capability is refused cleanly against a 1.1 worker and works against the
1.2 fixture; existing jobs, manifests (schema 2) and configs remain readable.

## Phase R2 — Runtime 0.6.0, linear typed recipes

- `src/contracts.ts`: `recipeSchemaV2` with `inputs` (typed declarations: `string`, `image`,
  `number` with optional range), `operations[]` (`op_id`, `task`, `model_family`, `inputs` bound
  from `input:<name>` or `op:<op_id>/<output_id>`, `defaults`, optional `adapters`), `publish`
  (list of `op_id/output_id`, default the last operation's outputs). A union `recipeSchema`
  accepts v1 and normalizes it to a one-operation v2 in memory (`normalizeRecipe()`), so every
  downstream path handles one shape. Bindings are validated at registration: no forward
  references, every referenced output exists for its task, input types match the consuming task.
- `submission`: `models: Record<op_id, model_id>` optional; `model_id` applies to the first
  operation; every operation must resolve to a registered model of its family.
- `Job`: add `operations[]` (`op_id`, `task`, `state`, `model`, `adapters`, `parameters`,
  `attempt_id`, `session_id`, `execution_id`, `worker_input_id`, `load_operation_id`,
  `effective`, `outputs`, `error`) and `current_operation` (index). Existing single-operation
  jobs are read as `operations.length === 1` by a `migrateJob()` on load in `src/store.ts`.
- `src/runtime.ts` `resolve()`: resolve every operation up front (model, family, worker task
  support, parameters, adapters, load options) so admission is all-or-nothing; the validate
  endpoint returns the per-operation plan. `tick()`: the per-job state machine becomes
  per-operation: dispatch operation `i`, commit its outputs durably under
  `outputs/<job>/<op_id>/`, then, if a later operation binds one of them, upload the committed
  file to the worker (`POST /v1/inputs`) and record `worker_input_id`; reload when the next
  operation's load identity differs; honor `cancel_requested` before each dispatch; a failed
  operation fails the job with `error.operation`. Release worker records per operation.
- Events: `stage` and `progress` gain `op_id`, `operation_index`, `operation_count`; no
  cross-operation percentage. `job_state` is unchanged.
- `src/server.ts`: `GET /v1/recipes/{recipe_id}` returns the normalized recipe with typed
  declarations; `publicJob()` includes `operations` and only `publish`ed outputs under
  `outputs`, with intermediates listed under `intermediate_outputs`; the outputs route serves
  both (the job owns them). Manifest schema 4 lists every operation and output.
- `src/cli.ts`: `run portrait-4k --model studio-sdxl --model-for enlarge=upscaler`;
  `recipes inspect` prints operations and bindings; `jobs watch` prints `op_id` with the stage.
  `src/tui.ts`: stage column shows `op_id/stage`.
- Retention: intermediates follow `output_retention_days` with the job; `cleanup()` treats them
  like outputs.
- Examples: `examples/recipe.portrait-4k.json` (generate → upscale).
- Tests: two-operation recipe end-to-end with the fixture (text-to-image then upscale):
  intermediate committed before the second dispatch, the second operation receives an uploaded
  input, worker reload observed, publish filtering, cancel between operations, failure in the
  second operation names it, restart during operation two reconciles only that attempt, v1
  recipes still run, `GET /v1/recipes/{id}` shape. `docs/decisions.md` 0.6.0 entry records the
  intermediate retention rule.

Acceptance: a linear recipe runs each operation as its own worker execution with durable
intermediates, honest per-operation progress and no partial-resume claims; schema-1 recipes and
0.5.0 jobs are unaffected.

## Phase R3 — Runtime 0.7.0, worker pool for multiple GPUs

- `src/config.ts`: `workers: [{ worker_id, url, token_env, command?, backend_id?, device?,
  memory_budget_mb? }]` (min 1); the legacy single `worker` object is accepted and converted to a
  one-entry pool with `worker_id: "default"`. Reject duplicate IDs; `device` is informational
  until the probe confirms it.
- `src/worker.ts`: `WorkerClient` gains `worker_id` and is instantiated per entry; in Go each
  worker runs its own goroutine loop instead of sharing one polling tick; a new
  `src/pool.ts` holds the clients, probes each, keeps per-worker `info`/`caps`/`protocol`, and
  refuses to start (or marks both unavailable, configurable `strict_devices`, default true) when
  two probes report the same `device_identity`.
- `src/runtime.ts`: `tick()` loops over workers: each has at most one running job; restart and
  child-exit reconciliation run per worker session; collection releases per worker. Scheduling:
  take the FIFO head; if it pins `worker_id`, wait for that worker; otherwise prefer an idle
  ready worker whose loaded identity equals the job's, else any idle ready worker that advertises
  the job's task, family and minor; never skip the head to run a later job on a free worker that
  the head cannot use unless `allow_reordering` is set (default false, so FIFO is literal).
  `resolve()` validates against the union of pool capabilities and records the candidate set;
  a pinned worker must be in it. Jobs record `worker_id` and `device_identity` at dispatch.
- `src/contracts.ts`: `submission.worker_id` optional; `Job.worker_id`.
- `src/server.ts`: `GET /v1/workers` (ID, device identity, readiness, loaded identity, running
  job ID for the caller's namespace only); `GET /v1/runtime` summarizes per-worker readiness;
  `GET /v1/jobs?worker_id=` filter.
- `src/cli.ts`: `workers list`, `run --worker ID`; `doctor` prints a section per worker;
  `src/tui.ts` adds a worker column and a per-worker header line.
- Manifest records `worker_id` and device identity (schema stays 4; additive field).
- Tests: launch two fixture workers on different ports with distinct device identities; two
  queued jobs run concurrently; a pinned job waits for its worker; affinity keeps a matching
  loaded identity on the same worker; killing one owned worker interrupts only its job; a
  duplicate device identity is refused; a legacy single-`worker` config still works.
- `docs/decisions.md` 0.7.0 entry: scheduling tie-break and the reordering default.

Acceptance: N configured workers give N concurrent executions with FIFO honored, per-worker
reconciliation and complete attribution in jobs, manifests, dashboard and doctor.

## Phase R4 — Runtime 0.8.0, distributed workers and API network mode (needs Engine 0.5.0)

- `src/config.ts`: a worker entry may use an `https://` URL with `tls: { ca_file, cert_file,
  key_file, pinned_sha256? }` and `remote_timeout_ms`; `command` is rejected together with a
  remote URL. Runtime API: `bind` (default `127.0.0.1`), `tls: { cert_file, key_file }` required
  when `bind` is not loopback, `allowed_origins` (default empty, meaning browser origins stay
  refused).
- `src/worker.ts`: in Go this is one `http.Transport` per remote entry with a `tls.Config`
  carrying the client certificate, the CA and a `VerifyPeerCertificate` that compares the fingerprint to `pinned_sha256` when set, and to the
  fingerprint `/v1/info` reported on first contact otherwise (recorded in the store and
  compared on every reconnect). SSE streaming works over the same agent. `stop()` never
  terminates a remote worker; the cancellation-deadline branch in `tick()` records
  `execution_interrupted` only after the worker reports a terminal state or the session changes.
- `src/server.ts`: `https.createServer` when `tls` is configured; refuse to start on a
  non-loopback bind without it; CORS allow-list from `allowed_origins`; everything else unchanged.
- `src/cli.ts`: `MEDIA_RUNTIME_URL` may be `https://`, with `MEDIA_RUNTIME_CA` for a private CA;
  `doctor` reports transport, fingerprint and remote latency per worker.
- Tests: fixture certificates under `test/certs/` (generated once, documented); the integration
  suite launches Engine's fixture in network mode (Engine 0.5.0's `--bind --tls-*` flags) and
  runs a job over mutual TLS; a fingerprint mismatch is refused; a remote entry with `command`
  fails config validation; the runtime API over TLS with a client that presents the token; a
  non-loopback plain-HTTP bind is refused at startup.
- README: a "Remote workers" section with the trust model (per-worker catalog, digests prove
  identity, no ownership) and the certificate expectations; `docs/decisions.md` 0.8.0 entry.

Acceptance: a job runs on a remote worker with identical job semantics, nothing ever terminates
a remote process, and no plain-HTTP non-loopback listener can be configured.

## Phase R5 — Runtime 0.9.0, video (needs Engine 0.6.0, protocol 1.3)

- `src/contracts.ts`: `PROTOCOLS` adds 1.3; `TASKS` adds `image-to-video`; family `svd`;
  parameters `frames`, `fps`, `motion`, `noise_augmentation`, `decode_chunk`; default recipe
  `animate` (`inputs.image` required, defaults from the worker's `task_defaults`). `signatures`
  gains an MP4 check (`ftyp` box at offset 4).
- `src/runtime.ts` `resolve()`: width/height equal the input and must be in the worker's
  advertised `video_sizes`; forbid `count`, `adapters`, `tile`; `commitOutputs()` verifies
  `video` and `poster` and records `frames`, `fps`, `duration_seconds`.
- `src/server.ts`: outputs route serves `video/mp4` with `Content-Length`; manifest (schema 5)
  includes the video facts. `src/cli.ts`: `run animate --model svd-xt --image frame.png
  --frames 25 --fps 7`; export writes `.mp4` and `.png`. `src/tui.ts`: frames/duration column.
- `src/client.ts`: video output types for Studio's adapter; Studio imports the video and poster
  (tracked in Studio's repository).
- Tests: the fixture's two-frame MP4 runs end-to-end, container signature verified, size limit
  enforced from the per-MIME map, a 1.2 fixture refuses the task; `docs/decisions.md` 0.9.0
  records the reference bundle and the video output limit.

## Phase R6 — Runtime 0.10.0, audio (needs Engine 0.7.0)

- `src/contracts.ts`: `TASKS` adds `text-to-audio`; family `stable-audio-open`; parameters
  `duration_seconds`; default recipe `soundscape`; `signatures` gains a RIFF/WAVE check.
- `src/runtime.ts`: forbid image parameters; `commitOutputs()` verifies `audio` and records
  `sample_rate`, `channels`, `duration_seconds`; manifest schema 5 additive fields.
- `src/cli.ts`: `run soundscape --model stable-audio --prompt "rain on a tin roof"
  --duration 10`; export writes `.wav`. `src/tui.ts`: duration column.
- Tests and decisions as in R5.

## Phase R7 — Runtime 1.0.0, native backend and compatibility record (needs Engine 1.0.0)

- `src/contracts.ts`: `BACKENDS` adds `native-sdxl`; `backend_options` accepts its namespace;
  `job.backend.ownership` records `engine`.
- Run the full integration suite against the native worker in CI when `MEDIA_ENGINE_NATIVE` is
  set; record the result in `docs/compatibility.md` with the Engine conformance suite version.
- README and decisions: when to choose each backend; nothing else changes because the protocol
  is unchanged.

Acceptance: every integration test passes against all three backends; the compatibility record
lists each tested combination.

## Cross-cutting work

- **Engine fixture dependency.** Each phase lands after the matching Engine release so that
  `tests/serve_fixture.py` advertises the needed surface. The integration suite pins the Engine
  revision it expects in `docs/compatibility.md` and `npm test` prints the fixture's negotiated
  minor so a mismatch is obvious.
- **Schema and migration.** Job data is a JSON column; new fields are additive with defaults
  applied by `migrateJob()` in `src/store.ts`. Manifest schemas are versioned (3 in R1, 4 in R2,
  5 in R5) and older manifests are never rewritten. Config accepts the previous release's shape
  for one release before removal is even considered.
- **Gateway client.** `src/client.ts` (after R0, `gateway-client/`) is Studio's typed entry point; every new output type,
  recipe shape and worker field is added there in the same change, and the Studio adapter work
  is listed in that phase's CHANGELOG entry as a companion task.
- **Documentation.** README sections per feature, `CHANGELOG.md`, `docs/decisions.md` per
  release, `docs/compatibility.md` per release, and examples for every new recipe, model and
  config field.
- **Release checklist.** Version bump and tag, `go build` and `go test ./...` against the
  required Engine and the previous Engine, CHANGELOG, README, decisions, compatibility,
  examples, `go.sum` unchanged unless a dependency changed, gateway client package published
  when its types changed.

## Risks and open decisions

| Item                               | Risk or decision                                                                       | Plan                                                                   |
| ---------------------------------- | -------------------------------------------------------------------------------------- | ---------------------------------------------------------------------- |
| FIFO versus utilization in a pool  | Literal FIFO can idle a worker when the head is pinned or unsuitable                   | `allow_reordering` off by default; document the trade-off              |
| Intermediate output storage        | Linear recipes multiply bytes on disk                                                   | Same retention as outputs; `publish` controls exposure, not retention |
| Rewrite while Engine moves         | R0 and Engine E1 overlap; a parity target that drifts doubles the work                  | Freeze TypeScript at 0.3.0; R0 targets Engine 0.3.0's fixture only     |
| Video and audio limits             | Default per-MIME limits are guesses until measured                                      | Record in decisions; configurable                                      |
| Studio adapter lag                 | Runtime features ship before Studio shows them                                           | Gateway client typed first; Studio tasks listed per phase              |
| Hardware validation                | No GPU or weights in this workspace                                                     | Each release states hardware-validated versus implemented              |
