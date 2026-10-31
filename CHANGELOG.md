# Changelog

## 0.10.0 — 2026-10-31

Audio, phase R6 of the implementation plan (worker protocol unchanged: `media-worker/1.3`;
text-to-audio needs Engine 0.7.0). **Text-to-audio**: the `text-to-audio` pipeline on the
`stable-audio-open` family and the default `soundscape` recipe (`prompt` required,
`negative_prompt` optional, `defaults: {steps: 100, duration_seconds: 10}`). Registration (`family:
stable-audio-open`) validates the Diffusers `StableAudioPipeline` bundle (roles transformer, vae,
text_encoder, tokenizer with `tokenizer.json`, projection_model, scheduler) through a worker that
loads the family on 1.3 and records the `sample_rate`, `channels`, `hop_length` and
`duration_range` the worker read from the bundle; `models inspect`, `models list`/`show` and
`doctor` show them. `guidance` defaults to the worker's task default (7) and the seed is resolved;
every value is recorded before dispatch. A duration outside the model's range is refused (field
`parameters.duration_seconds`), never trimmed; `steps` may go to 200 for audio (the worker's
`audio_steps` applies) while the image tasks keep 150 with the same error field as before; an
image, `width`, `height`, `sampler`, `scheduler`, `strength`, `count`, tiling, video parameters and
adapters are refused by name. A 1.2 worker, or a 1.3 worker that does not list the task (Engine
0.6.0, or a reference backend without its audio extra), refuses it. **Output**: `audio`
(`audio/wav`) is streamed, then checked by size, SHA-256 and an owned canonical WAV check (44-byte
RIFF/WAVE header, 16-bit PCM `fmt `, consistent alignment and byte rate, one `data` chunk of whole
frames to the end of the file) against the worker's record (`sample_rate`, `channels`, `samples`,
`duration_seconds = samples / sample_rate`), against the model's rate and channels, and against
the request: never shorter, and longer by less than one latent frame. Jobs and operations record
audio `media` facts and audio jobs write manifest schema 5 (additive, as specified for 0.9.0).
**CLI and dashboard**: `run --duration SECONDS`; `outputs export` writes `.wav`; the dashboard's
media column shows the duration (requested before commit, actual after) and the detail line the
duration and steps instead of an image size. **Fix**: a video or audio parameter on a task that
does not take it (for example `frames` on `portrait`) is refused with its field named; 0.9.0
refused it as `invalid_request` without a field. The gateway client gains `duration_seconds`, the
`stable-audio-open` family, `samples`, `AudioFacts` and the audio model facts. Examples:
`examples/recipe.soundscape.json`, `examples/model.audio.json`. No new dependency; `go.sum` is
unchanged. **Validated**: protocol behavior against Engine 0.7.0's fixture, whose
`stable-audio-open` bundle has random stand-in weights and produces a seeded sine tone, and the
unchanged image and video paths against Engine 0.6.0's fixture (where the audio tests skip)
([compatibility record](docs/compatibility.md)). **Not validated**: real Stable Audio Open
inference, sound quality, GPU memory and the 64 MiB default WAV limit on real clips (no GPU or
weights here); a real 44.1 kHz stereo bundle was never registered, so its facts are as Engine
documents them. Companion Studio task: import the clip as a Studio-owned file with provenance
(sample rate, channels, duration).

## 0.9.0 — 2026-10-31

Video, phase R5 of the implementation plan (worker protocol `media-worker/1.3`, Engine 0.6.0;
1.0–1.2 are still negotiated). **Image-to-video**: the `image-to-video` pipeline on the `svd`
family and the default `animate` recipe (`inputs.image` required, `defaults: {steps: 25}`).
Registration (`family: svd`) validates the Diffusers `StableVideoDiffusionPipeline` bundle through
a worker that loads the family on 1.3 and records the `video_sizes` and `default_frames` the worker
read from the bundle; `models inspect` recognizes the layout, and `models list`/`show` and `doctor`
print the facts. Width and height are the input's and must be one of the model's sizes (field
`video_size` otherwise; nothing is resized); `frames` defaults to the bundle's, `fps`, `motion`,
`noise_augmentation` and `decode_chunk` to the worker's `task_defaults` and `steps` to the recipe,
all recorded with the seed before dispatch; values outside the worker's `limits` are refused, and a
prompt, `guidance`, `sampler`, `scheduler`, `strength`, `count`, tiling and adapters are refused by
name. A 1.2 worker refuses the recipe (`unsupported`, `recipe_id`) and the family (`unsupported`,
`family`). **Outputs**: `frames` (`application/zip`) and `poster` (`image/png`), plus `video`
(`video/mp4`) only when the worker advertises it; the runtime never encodes video. Each output is
streamed to a temporary file while hashed (no longer buffered in memory), then checked by size,
SHA-256 and an owned signature check on the standard library: for the archive the ZIP local header,
the central directory, a strict `frames.json` equal to the worker's record and to the request, one
PNG per frame of the declared size and `duration_seconds = frames / fps`; for MP4 an `ftyp` box at
offset 4 that fits the file. Per-MIME `max_output_bytes` applies (archive and MP4 256 MiB by
default). Jobs and operations record `media` (frames, fps, duration, size, `encoded_video`); every
job with a video operation writes **manifest schema 5** (the schema-4 operations layout plus each
operation's `media`); other jobs keep schemas 3 and 4. Linear recipes may include video operations;
only PNG outputs (`image`, `poster`) can be bound to an image input. **CLI and dashboard**: `run
--frames --fps --motion --noise-augmentation --decode-chunk`; `outputs export` writes `.zip`, `.png`
and `.mp4` when present and now streams each file (a partial file is removed on failure); the
dashboard shows stage progress in the worker's unit (`decoding 8/25 frames`, which also makes
upscale progress read `upscaling 1/4 tiles`), a media column with frames and duration that appears
only when a listed job has video facts, and the selected job's frames and rate. The gateway client
gains the video parameters, `Family`, `MediaFacts` and model facts. Examples:
`examples/recipe.animate.json`, `examples/model.svd.json`. No new dependency; `go.sum` is
unchanged. **Validated**: protocol behavior against Engine 0.7.0's and 0.6.0's fixtures, whose
`svd` bundle has random stand-in weights and produces flat-colour frames
([compatibility record](docs/compatibility.md)). **Not validated**: real Stable Video Diffusion
inference, GPU memory and the default output limits on real archives (no GPU or weights here), and a
worker-produced `video/mp4`: Engine 0.6.0 and 0.7.0 ship no encoder and never advertise one, so the
optional output path is tested only with bytes injected by a test hook. Companion Studio task:
import the frame archive (and the video when present) as Studio-owned files with provenance and use
the poster as the preview.

## 0.8.0 — 2026-10-29

Distributed workers and API network mode, phase R4 of the implementation plan (worker protocol
unchanged: `media-worker/1.2`; network mode needs Engine 0.5.0). **Remote workers**: a worker entry
may name an `https://` URL with `tls` (`ca_file`, `cert_file`, `key_file`, optional
`pinned_sha256`) and `remote_timeout_ms`; a remote entry with a `command` is refused at validation,
as are `tls` and `remote_timeout_ms` on a loopback `http://` entry. Each remote worker has its own
transport (TLS 1.3, the configured CA as the only root, the runtime's client certificate) that
checks the server certificate's SHA-256 on every handshake against `pinned_sha256` or, without a
pin, against the fingerprint recorded on first contact (kept per worker ID in the state directory
and required on every reconnect); `/v1/info` must also report the certificate presented. A refused
certificate is `certificate_mismatch` and, like a lost connection, leaves a running job for
reconciliation; pinning a new fingerprint is the rotation path. A remote worker is never started,
stopped or signalled: past the cancellation deadline its job waits for the worker, is `cancelled`
when the worker confirms, and otherwise settles as `execution_interrupted` with `forced_termination:
false` and nothing committed (or through the session check when the session changes). Job semantics
are otherwise unchanged. **API network mode**: `bind` (an IP address, default `127.0.0.1`), `tls`
(`cert_file`, `key_file`) and `allowed_origins` (exact CORS allow-list, default empty, which keeps
browser origins refused); a non-loopback bind without `tls` is refused at startup, and the startup
record adds `scheme`, `bind` and `certificate_sha256`. TLS keys must be mode 0600 and certificates
not group/other writable, as in Engine. **CLI and diagnostics**: `MEDIA_RUNTIME_URL` may be
`https://` with `MEDIA_RUNTIME_CA` for a private CA, and plain `http://` to a non-loopback host is
refused; `doctor` prints each worker's transport, certificate fingerprint (pinned or recorded) and
`/v1/info` latency, and how to re-pin after a mismatch; `GET /v1/runtime` and `GET /v1/workers` add
`transport`, `remote`, `certificate_sha256` and `latency_ms`. The gateway client gains the worker
transport fields (a private CA is trusted through `NODE_EXTRA_CA_CERTS`). Example:
`examples/runtime.remote.config.json`. Test certificates (generated once, ten-year validity) are in
`testdata/certs`. Standard library `crypto/tls` only; no new dependency and `go.sum` is unchanged.
Tested against Engine 0.6.0's and 0.5.0's fixtures, including network mode over mutual TLS on
loopback ([compatibility record](docs/compatibility.md)); no multi-host network or GPU was used.
Companion Studio task: accept an `https://` runtime URL (and a private CA) in the Media Runtime
connection settings.

## 0.7.0 — 2026-10-29

Worker pool for multiple GPUs, phase R3 of the implementation plan (worker protocol unchanged:
Engine 0.4.0, `media-worker/1.2`). **Configuration**: `workers` lists one entry per Engine process
and device (`worker_id`, `url`, `token_env`, optional `command`, `backend_id`, `device`,
`memory_budget_mb`); the single `worker` object is still accepted as a pool of one named
`default`; duplicate worker IDs, devices and external URLs are refused; `strict_devices` (default
true) refuses to start when two workers report one device identity (host, device and index), and
otherwise marks both unavailable; `allow_reordering` (default false) keeps FIFO literal.
**Scheduling**: each worker runs its own scheduler loop with at most one running job; the FIFO head
goes to an idle ready worker whose loaded identity matches, else to the first idle worker that
advertises its tasks, families and minor (ties in configuration order), and a head that cannot run
holds the queue unless reordering is allowed. A submission may pin `worker_id` (`run --worker ID`)
and then waits for that worker; an unknown worker is `400 unknown_worker`. Admission checks the
request against every worker, records the passing ones as `candidates` and resolves the job on the
first of them; `POST /v1/jobs/validate` returns `candidate_workers`. A linear job runs every
operation on the worker it started on. **Attribution**: jobs record `worker_id` and
`device_identity` at dispatch (also on each operation and in the `running` `job_state` event), and
manifests carry `worker_id` (schemas 3 and 4 unchanged). **Recovery**: reconciliation, child-exit
handling and forced termination are per worker, so a lost worker interrupts only its own job.
Jobs written by 0.6.0 and earlier read as `worker_id: "default"` when they were dispatched; a job
whose worker is no longer configured fails instead of blocking the queue. **API and CLI**: `GET
/v1/workers` (`workers list`) lists each worker's device identity, readiness, loaded identity,
whether it is busy and, for the caller's namespace only, the running job ID; `GET /v1/runtime` adds
`workers` beside `worker`; `GET /v1/jobs?worker_id=` (`jobs list --worker ID`) filters by worker;
`doctor` prints a section per worker and exits 3 when any is unavailable; the dashboard shows one
header line per worker and a worker column. A pool of one keeps the 0.6.0 output exactly. The
gateway client gains worker summaries, `worker_id` on submissions, jobs and operations, and
`candidates`. Example: `examples/runtime.pool.config.json`. Tested against Engine 0.4.0's and
0.5.0's fixtures (pools of two fixture processes with distinct device indexes) and, for the 1.1
path, Engine 0.3.0's ([compatibility record](docs/compatibility.md)). Companion Studio task: import
`worker_id` and the device identity into output provenance.

## 0.6.0 — 2026-10-27

Linear typed recipes, phase R2 of the implementation plan (worker protocol unchanged: Engine 0.4.0,
`media-worker/1.2`). **Recipe schema 2** declares typed inputs (`string`, `image`, `number` with a
range) and up to eight operations, each with a task, model family, input bindings
(`input:<name>`, `op:<op_id>/<output_id>`), defaults and adapters, plus a `publish` list; bindings
are checked at registration (no forward references, outputs that exist, types the consuming task
accepts, required task inputs always bound). Schema-1 recipes are unchanged and are normalized to
one operation in memory; `GET /v1/recipes/{id}` and `recipes inspect` return any recipe's typed
declarations, operations and bindings. **Submission**: `models: {op_id: model_id}` (`run
--model-for OP=MODEL`) selects later operations' models and named inputs are given as `inputs`
(`run --input NAME=VALUE`); every operation is resolved before admission and errors name it
(`operations.<op_id>.…`). `POST /v1/jobs/validate` returns the per-operation plan. **Execution**:
each operation is its own worker execution; its outputs are committed durably (intermediates under
`outputs/<job>/<op_id>/`) before the next is dispatched, a bound output is uploaded to the worker
from that copy, the worker reloads only when the load identity changes, cancellation is honored
between operations, a failed operation fails the job with `error.operation`, and a restart
reconciles only the current operation's attempt. Jobs gain `operations[]`, `current_operation`
and, for linear jobs, `current_op_id`, `intermediate_outputs` and stage/progress events carrying
`op_id`, `operation_index` and `operation_count`; the dashboard shows `op_id/stage`. Manifest
schema 4 for linear jobs lists every operation and output; schema-1 jobs keep schema 3. Jobs written
by 0.5.0 and earlier are read as one operation (running ones are reconciled through the same
attempt); their API shape, events and manifests are unchanged. Cleanup also expires intermediates
and the committed operation outputs of jobs that failed later. The gateway client gains recipe,
operation and linear-job types. The CLI test no longer lists the `upscaler` catalog handle against
an Engine fixture that does not create it, which made `models inspect` exit 2 against Engine
0.3.0 (so the 0.5.0 row recording a pass against that Engine was wrong for the CLI package).
Tested against Engine 0.4.0's and 0.5.0's fixtures and, for the 1.1 path, Engine 0.3.0's
([compatibility record](docs/compatibility.md)). Companion Studio task: map native-workflow inputs
from `GET /v1/recipes/{id}` and import a linear job's published outputs.

## 0.5.0 — 2026-10-27

Worker protocol `media-worker/1.2` (Engine 0.4.0), phase R1 of the implementation plan. Runtime
records the worker's device identity, loader and task facts on every job and in manifest schema 3.
**Multiple outputs:** `parameters.count` / `run --count` (text-to-image, worker-limited) commits
`image` … `image_N` with per-output seeds; success requires every advertised output; per-MIME
`max_output_bytes`; the outputs route and `outputs export` handle any output ID and MIME type.
**Upscaling:** the `upscale` pipeline, the `esrgan` family (`family` in registrations, scale from
the worker) and the default `enlarge` recipe with `tile`/`tile_overlap`; inputs are never resized
and oversize results are refused before admission. **Conversion provenance:** catalog inspection
and registration read `conversion.json` into `source.conversion`, manifests carry it, and bare
single-file checkpoints are refused with field `format`. **Automatic settings and fallbacks:**
`precision: "auto"` with recorded `resolved_options`/`resolutions`; `authorized_fallbacks`
(`run --fallback`) for credentials with `allow_fallbacks`, recorded as `applied_fallbacks`, a
`stage` event, in the dashboard and in the manifest. The gateway client gains typed submissions,
jobs, output records and model summaries. Every 1.2 capability is refused against 1.0/1.1 workers;
existing jobs, schema 2 manifests and 0.4.0 configurations remain readable. Tested against Engine
0.4.0's fixture and, for the 1.1 path, Engine 0.3.0's ([compatibility record](docs/compatibility.md)).
Companion Studio task: import every output of a multi-output job and the upscale task through the
typed gateway client.

## 0.4.0 — 2026-10-27

The service is rewritten in Go at exact 0.3.0 feature parity, as phase R0 of the implementation
plan. One static `media-runtime` binary (no cgo; `go build ./cmd/media-runtime`) replaces
`node dist/cli.js`; every HTTP route, error code, configuration field, environment variable,
CLI command, flag, exit code and `--json` shape is unchanged, and the SQLite schema, pragmas,
manifest schema 2, file modes and fsync order are those of 0.3.0, so an existing state directory
is served without migration (tested against a directory the 0.3.0 service wrote). Records keep
JavaScript's key order and number formatting, so idempotency digests and recipe digests written by
0.3.0 still match. The only third-party module is `modernc.org/sqlite` (pure Go). The TypeScript
gateway client moves to its own package, `gateway-client/` (`@media-runtime/gateway-client`);
the rest of the TypeScript service and its Node test harness are removed, and `go test ./...`
replaces `npm test`. Behavior fix found while porting: an image-to-image input whose file has
disappeared before dispatch now fails the job with `input_unavailable` instead of retrying forever.
No feature beyond 0.3.0 is added. Tested against Engine 0.3.0's fixture
([compatibility record](docs/compatibility.md)).

Specification revision 0.2.0 builds out every capability revision 0.1.0 deferred: multiple
outputs per job, upscaling, the conversion provenance workflow, automatic settings with authorized
fallbacks, linear typed recipes (schema 2), a worker pool for multiple GPUs, distributed workers
with API network mode, video, audio and the native backend. `docs/implementation-plan.md` orders
them into releases 0.5.0 through 1.0.0 against the matching Engine releases, after a Go rewrite
of the service at 0.3.0 parity as 0.4.0.

## 0.3.0 — 2026-10-26

LoRA adapters as a separately advertised capability. Operators register an approved adapter
directory (`POST /v1/adapters`, `adapters register`); submissions and recipes name up to four
registered adapters with scales (`run --adapter ID[:SCALE]`). The runtime validates them through
the worker, treats the adapter set as part of the worker load identity, records them in the job
and manifest, and refuses them for workers that do not advertise adapters or speak only 1.0.

## 0.2.0 — 2026-10-26

Operator-experience and additional-operation stages. Negotiates `media-worker/1.0` or `1.1` per
worker session and accepts the explicit `owned-sdxl` backend beside `diffusers-sdxl`. Added
image-to-image as a separately advertised recipe pipeline (`variation` default recipe) fed by
durable, namespace-owned PNG inputs (`POST /v1/inputs`, `inputs upload`, `run --image`); width and
height must equal the image and strength sets the effective step count. Added the terminal
dashboard (`tui`), a human-readable `doctor` with exit codes, catalog inspection for the explicit
model installation workflow (`models inspect`, `GET /v1/catalog`), model detail with worker
re-verification and memory estimates (`models show --verify`), newest-first job listing, and
manifest schema 2 with task, inputs and effective settings.

## 0.1.0 — 2026-10-26

Implemented the standalone CLI/API, authenticated namespaces, explicit model/recipe registration,
SQLite FIFO jobs and event history, idempotency, cancellation, worker supervision and restart
reconciliation. Outputs are verified and committed with provenance before success; retention is
explicit and exports preserve originals. Added a gateway client, operator examples, decisions and
cross-process contract tests. Studio integration is delivered in its companion repository.
