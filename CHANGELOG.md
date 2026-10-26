# Changelog

## Unreleased

Specification revision 0.2.0 builds out every capability revision 0.1.0 deferred: multiple
outputs per job, upscaling, the conversion provenance workflow, automatic settings with authorized
fallbacks, linear typed recipes (schema 2), a worker pool for multiple GPUs, distributed workers
with API network mode, video, audio and the native backend. `docs/implementation-plan.md` orders
them into releases 0.5.0 through 1.0.0 against the matching Engine releases, after a Go rewrite
of the service at 0.3.0 parity as 0.4.0. No code changed.

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
