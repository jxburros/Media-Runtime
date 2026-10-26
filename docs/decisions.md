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
