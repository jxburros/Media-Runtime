# Media Runtime action items from the 2026-10-31 cross-repo review

**Implementation update (2026-11-04):** Checked items have code, documentation and targeted tests in the 1.1.0 development PR. Final cross-repository validation is tracked in [delivery status](../delivery-status.md); checked implementation items do not assert GPU acceptance or a published release. The dated requirements below are preserved.

**Date:** 2026-10-31 · **Source:** [cross-repo review](2026-10-31-cross-repo-review.md) (section references below are to that document) · **Agent:** Claude Code (Claude)

This is the to-do list for **Media Runtime**: what to build, the decisions only the maintainer can
make, and the inconsistencies the review found. It is a dated working list. Tick items in this file
or in the tracking issue. When an item turns into real work, the change itself updates the README,
`runtime-spec.md`, `docs/decisions.md` and `docs/compatibility.md` as usual.

Priority means: **P0** blocks any real use. **P1** is needed before an application can rely on the
runtime as a ComfyUI replacement. **P2** is important. **P3** is later. Items marked
**(needs Engine)** depend on a Media Engine item with the same name in
[Media-Engine's action items](https://github.com/jxburros/Media-Engine/blob/main/docs/reviews/2026-10-31-action-items.md).
Items marked **(decision)** wait on a decision in section D.

## A. P0: prove it works

- [x] **A1. Continuous integration.** Add `.github/workflows/ci.yml` running:
  - `go vet ./...` and `go test -race ./...`, with Media-Engine checked out beside the repository at
    a pinned commit, so the integration suites drive its `tests/serve_fixture.py`
  - `npm ci && npm run build` in `gateway-client/`
  - `python3 testbench/testbench.py --fixture`

  Done when every PR runs these and the compatibility record cites CI runs instead of manual ones.
  (§5.8)
- [ ] **A2. A real-hardware run.** Run `testbench/testbench.py` on the reference RTX 4090 with real
  weights: SDXL base, one LoRA, Real-ESRGAN, SVD img2vid-xt and Stable Audio Open. Add the result
  as a row in `docs/compatibility.md`; today it says "none of these rows ran on a CUDA device or
  real weights". Done when that row exists, including any failures it finds. (§1, §9.3)
- [ ] **A3. First tagged release.** Wire the shared `JX-Holdings-LLC/release-infrastructure`
  workflows (manual dispatch, dry run first, all-or-nothing). Publish:
  - a static `linux-x64` binary under a contracted asset name, for example
    `media-runtime-v<ver>-linux-x64.tar.gz`
  - `checksums.txt`
  - the gateway-client package

  The repository has no tags today. (§5.7)
- [x] **A4. Phase R7: accept `native-sdxl`.** This is the open phase in `docs/implementation-plan.md`.
  Accept `backend_id: native-sdxl`, record `ownership: engine`, and run the integration suite
  against `media-engine-native`. (Engine 1.0.0 already ships the binary.)
- [x] **A5. Validation wording.** Until A2 lands, mark features in the README as "delivered
  (fixture-tested)", as Engine's spec table already does. (§9.3)

## B. P1: usable by applications (operator and security layer)

These are modelled on JX Runtime. Port the *design* to Go and review it as new security code; JX's
tests are not evidence for the port. (§5.1)

- [x] **B1. API keys you can manage.**
  - Hashed keys, shown once.
  - Created, listed and revoked through operator API routes and a `media-runtime keys` CLI.
  - Changes reload without a restart.
  - Each key carries: permissions (`jobs.submit`, `jobs.read`, `models.read`, `models.register`,
    `admin`), a model allow-list, expiry, CIDR restrictions, allowed namespaces (replacing
    `allowed_namespaces` in config), `allow_fallbacks`, and a concurrency limit.
  - Keep the environment-variable credentials as a bootstrap path.
- [x] **B2. Rate limits.** A per-IP limit checked *before* key verification, per-key sliding
  windows, and a per-key share of the queue. Today `max_queued_jobs` is global. Answer with `429` and
  `Retry-After`.
- [x] **B3. Audit log.** A rotating log of key changes, registrations, cancellations, applied
  fallbacks and refused authentication. Cap the events an attacker can trigger.
- [x] **B4. Watched model folders.** `media-runtime folders add PATH` registers a directory in place:
  never copied, never deleted, and an unplugged drive is told apart from a deletion. Recognise an
  existing **ComfyUI `models/` tree**, a Hugging Face cache and plain folders. This is the
  cheapest migration path from ComfyUI (§5.2). **(needs Engine: file-level catalog handles)**
- [x] **B5. One catalog.** For owned workers, Runtime generates the Engine catalog it passes on the
  command line, so handles cannot drift between `artifact_catalog` and Engine's `catalog.json`.
- [x] **B6. Prometheus metrics.** Serve `GET /metrics`: jobs by recipe and state, phase durations,
  queue depth, worker busy, output bytes, and per-worker peak memory as the worker reports it. Ship a
  starter Grafana dashboard. Re-export Engine metrics. **(needs Engine: `/metrics`)** (§5.4)
- [x] **B7. Guided repair in errors.** Add an optional `error.repair` with pre-filled follow-up
  requests that an application can show as a button. Examples: "register the converted bundle",
  "lower `count` to N", "authorize the `cpu` fallback". (§5.4)
- [x] **B8. Managed-mode handshake.** `serve --managed --port 0` prints NDJSON
  `ready`/`stopping`/`stopped` on stdout and stops on stdin close or SIGTERM. This lets an
  application embed the runtime as a child process. (§5.4)
- [x] **B9. Service registration.** `media-runtime service install|uninstall|status` for a systemd user
  unit, with named instances. (§5.4)
- [x] **B10. OpenAPI and a drift test.** Commit `docs/openapi.json` generated from the routes. Add a
  test that fails when a served route is undocumented or a documented route is not served. (§5.6)
- [x] **B11. Discovery.** Serve `GET /.well-known/media-runtime` with the same `schemaVersion` and
  `surfaces` shape as JX Runtime's descriptor. (§9.2)
- [x] **B12. Diagnostics export.** One JSON file for bug reports, never containing prompts or outputs.
  (§5.4)

## C. P1: ComfyUI-capable (runtime side of generation features)

Each item is the runtime half of an Engine task. It needs a recipe, refusal rules, manifest fields,
CLI flags and gateway-client types, and an integration test against the fixture. (§4.1)

- [x] **C1. Sampler and scheduler choice.** Pass the worker's advertised set through. Record it in the
  job and manifest, and never substitute. **(needs Engine)**
- [x] **C2. Utility image operations** (`resize`, `crop`, `pad`, `composite`) as explicit recipe
  operations. Then ship a `hires-fix` recipe (generate → upscale → resize → image-to-image) and an
  outpaint recipe. **(needs Engine)** **(decision D2)**
- [x] **C3. Inpainting.** A `mask` input that is a durable, namespace-owned PNG matching the image size,
  plus an `inpaint` recipe. **(needs Engine)**
- [x] **C4. Live previews.** Relay the worker's optional `preview` events as SSE events that reference
  a fetchable preview handle (no inline base64). Do not keep previews as job outputs. **(needs
  Engine)**
- [x] **C5. New image families.** One Flux-class DiT family and SD3.5: registration validation,
  `models inspect` layouts and recipes. **(needs Engine)**
- [x] **C6. ControlNet.** Register ControlNet models like adapters. A load-identity list
  `controlnets: [{id, scale}]` and a `control_image` input are recorded in the manifest. **(needs
  Engine)**
- [x] **C7. MP4 output.** The commit path for `video/mp4` exists but has never seen a real worker. Test it
  end to end once Engine ships the encoder extra. **(needs Engine)** **(decision D3)**

## D. Decisions for the maintainer

- [x] **D1. ComfyUI workflow import.** Keep it fully out of scope (the current `runtime-spec.md`), or
  allow an offline, out-of-process translator for a subset of ComfyUI API-format workflows plus a
  `docs/comfyui-migration.md`? (§4.4)
- [x] **D2. Utility image operations.** The rule today is "nothing is resized". The proposal reads it as
  "nothing is resized *silently*" and makes resizing an operation the caller names. Confirm, and
  choose where the pixels are touched:
  - in Engine, as a task: one producer of outputs and provenance
  - in Runtime, as owned Go code: no worker round trip
- [x] **D3. MP4 encoder.** Does Engine ship an optional pinned encoder extra? This is the open decision
  in `engine-spec.md`.
- [x] **D4. Model downloads.** `runtime-spec.md`'s dependency policy says "no downloads, no registries",
  while JX-style opt-in, pinned Hugging Face installs would be an explicit operator command, never
  part of a job. Allow it (amend the policy), or keep acquisition fully manual and do only B4? (§5.2)
- [x] **D5. Webhooks.** Allow completion callbacks: a signed POST on terminal state to
  operator-allow-listed URLs. That would be the runtime's first outbound network call. (§4.1)
- [x] **D6. Governance.** Adopt Spec-Driven Docs (`AGENTS.md`, `CLAUDE.md`, `development-docs/` with a
  Never list) as JX Runtime does, or add only an `AGENTS.md` with the hard constraints? (§5.8)
- [x] **D7. Version and changelog policy.** JX and Onyx bump the version on every change to main. Should
  Media Runtime do the same, and does this document (docs only) need a version bump?
- [x] **D8. Shared GPU agreement with JX Runtime.** Design an advisory per-device GPU lease now, or keep
  operator-set budgets? (§9.1)
- [x] **D9. Positioning.** Add the positioning statement from §4.3 to `runtime-spec.md`?
- [x] **D10. Platforms.** Linux x64 only, or also macOS and Windows binaries? Go makes them cheap, but
  Engine is Linux/CUDA.

## E. P2–P3

- [x] **E1 (P2). Managed Engine install.**
  - Pin `media-engine-native` archives by SHA-256 per platform, as JX pins Onyx.
  - Or manage a private `uv` virtual environment from a pinned Engine wheel and `uv.lock`, or a
    pinned CUDA image.

  **(needs Engine: releases)** (§5.3)
- [x] **E2 (P2). Orphan reaper.** A PID-file guard that reaps owned workers left behind by a crashed
  runtime, after confirming the PID is still the expected binary. (§5.3)
- [x] **E3 (P2). Idle unload.** `idle_unload_minutes` per worker, a per-job `keep_loaded` hint, and LRU
  across the pool that respects affinity. (§5.5)
- [x] **E4 (P2). Hugging Face installs.** Pinned revisions, per-file SHA-256, `allow_patterns`,
  resumable downloads, a free-space check and SSRF-safe fetching (JX `safeFetch` rules).
  **(decision D4)**
- [x] **E5 (P2). Completion webhooks.** **(decision D5)**
- [x] **E6 (P2). DAG recipes.** Schema 3: several consumers per output and `foreach` over a batch, still
  declarative, with no code. (§4.1)
- [x] **E7 (P2). Python client** beside `gateway-client`. (§5.6)
- [x] **E8 (P2). Installer and update check.** A `curl | sh` installer that verifies `checksums.txt` and
  never uses sudo, a `.deb`, and a check-only `update` command. (§5.7)
- [x] **E9 (P2). Split `internal/runtime/runtime.go`.** It is 2,507 lines. Split it into
  resolve/admission, scheduling, commit and doctor files before the B and C items grow it. (§5.9)
- [x] **E10 (P2). Integrator compatibility doc.** What a client may rely on across Runtime minors, like
  JX's `docs/version-compatibility.md`. (§5.6)
- [x] **E11 (P3). Unify the error envelope with JX.** Media's `field` plus JX's `type` and `repair`,
  keyed by `request_id`, and echo `X-Request-Id`. (§9.2)
- [x] **E12 (P3). Execution caching.** Reuse intermediate outputs across jobs, keyed by digest.
  **(needs Engine for text-encoding caches)**
- [ ] **E13 (P3). GPU lease with JX Runtime.** **(decision D8)**
- [x] **E14 (P3). Pairing codes** for applications on other devices: a single-use code exchanged for a
  scoped key plus the TLS fingerprint. (§5.1)

## F. Inconsistencies found

- [x] **F1.** `README.md:291-292` says "The worker contract is media-worker/1.2" and "Runtime 0.5.0
  negotiates…", but Runtime 0.10.0 speaks `media-worker/1.3`.
- [x] **F2.** `runtime-spec.md:20` titles the delivery table "Status in Runtime 0.3.0". Rows delivered
  since then (multiple outputs, upscaling, recipes, pools, network mode, video, audio, the Go
  rewrite) still read "Specified below".
- [x] **F3.** `runtime-spec.md:46-47` (product sheet) lists only text-to-image and image-to-image as
  delivered, and upscaling, video, audio and linear recipes as planned.
- [x] **F4.** `runtime-spec.md:326` still says "The next implementation step is the Go rewrite at 0.3.0
  parity…".
- [x] **F5.** `runtime-spec.md` front matter: `status: draft_proposal`, `spec_revision: 0.2.0`, which no
  longer matches 0.10.0.
- [x] **F6.** `docs/implementation-plan.md:68` says "Go 1.23 or later", while `go.mod` and the README
  require Go 1.24.
- [x] **F7.** `README.md:4-5` calls Media Engine "its separately installed SDXL inference worker". It now
  also serves ESRGAN, SVD and Stable Audio.
