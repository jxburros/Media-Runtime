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
