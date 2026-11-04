# Declarative DAG recipes and intermediate reuse

Recipe schema 3 accepts the same typed declarations, bindings, defaults and explicit
model selections as schema 2. Operations may be declared out of dependency order.
Registration rejects cycles, missing producers, incompatible input/output types and
unused declarations. Admission resolves every model and output size before queuing.
Execution follows deterministic topological order on one worker lane; declaration
order breaks ties between available operations. `model_id`, `parameters` and
submission adapters select the first operation in that order. `models` selects the
remaining operations by their original recipe IDs.

An output may feed several consumers. `op:generate/image_2` selects the second image
of a batch; admission refuses a reference beyond the submitted batch count.
`publish: ["generate/image"]` publishes the entire batch. An explicit numbered
publish selects that image only.

A consumer can declare `"foreach": {"from": "op:generate/image"}` instead of an
`inputs.image` binding. Admission expands it once per image in the text-to-image
producer's batch. The operation must consume an image. Its other declared inputs,
model, adapters and defaults apply to every item. Expanded IDs are
`<original_id>_item_1` through `_item_4`, and ID collisions are refused. A publish
entry using the original ID publishes every expanded item. At most eight declared
operations and 32 expanded operations are admitted. A foreach group cannot itself
be consumed as an aggregate or nested in another foreach; these forms are refused.
This schema contains no scripts or expressions.

For example, this graph generates three images and upscales each:

```json
{
  "schema_version": 3,
  "recipe_id": "batch-upscale", "revision": 1,
  "inputs": {"prompt": {"type": "string", "required": true}},
  "operations": [
    {"op_id": "generate", "task": "text-to-image", "model_family": "sdxl-base",
     "inputs": {"prompt": {"from": "input:prompt"}},
     "defaults": {"count": 3, "width": 512, "height": 512}},
    {"op_id": "enlarge", "task": "upscale", "model_family": "esrgan",
     "inputs": {}, "foreach": {"from": "op:generate/image"}}
  ],
  "publish": ["enlarge/image"]
}
```

Submit with `model_id` selecting SDXL and `models.enlarge` selecting the upscaler.
Manifest schema 6 preserves the registered source operations, expanded graph,
original `source_op_id` for every operation, output bindings and all model/input/
worker provenance. Existing schema 1 and 2 recipes keep their existing behavior.

## Opt-in intermediate reuse

Set `reuse_intermediates: true` on a schema 2 or 3 submission to reuse an identical
intermediate from an earlier successful opted-in job. Final operations always
execute. The worker must first confirm the new attempt has not been accepted, so
an ambiguous network response never substitutes a cache result for an execution.

The content identity includes the resolved task and parameters (including seed),
input byte digests and prompts, complete model bundle, adapter digests and scales,
backend snapshot and resolved precision/device/options/fallbacks, Runtime version,
worker ID and worker session. Reuse is restricted to the same namespace and
credential. Worker restart invalidates reuse even if its advertised version did
not change; this also prevents reuse after dependency changes hidden by a version
string. Randomly chosen seeds naturally produce misses.

A source must be a successful retained job with unexpired files. Runtime copies
its bytes to the new job and verifies length, SHA-256, MIME signature and media
facts before fsync and commit. Missing or expired sources are misses; corrupted
bytes or a source disappearing during the copy fail safely with
`output_unavailable`. The cache adds no separate files and follows ordinary
retention. Lookup scans retained successful jobs; large installations should keep
retention bounded. A hit still loads/validates the requested model before reuse.

Schema 6 records `cache.key`, `source_job_id`, `source_op_id`, the original
execution ID and explicitly attributed source measurements. A cached operation
has no new worker execution ID or inference measurements. Normal output events,
durable commit, cancellation-before-dispatch and ownership checks still apply.

Pure tests cover graph topology/refusals, expansion and semantic/security cache
identity. HTTP fixture tests cover expanded execution, provenance, opt-in reuse,
seed misses and corruption rejection. GPU acceptance remains a separate job.
