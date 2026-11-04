# SDXL conditioning components

Protocol 1.4 reference SDXL workers can advertise optional VAE replacement,
textual-inversion embeddings, IP-Adapter and CLIP skip. Runtime refuses these on
workers that do not advertise them. Owned/native workers do not acquire these
features through reference-worker fallbacks.

Register approved local directories with `POST /v1/conditioning` (operator only):

```json
{"conditioning_id":"my-style","kind":"textual-inversion","artifact_handle":"approved-style"}
```

`GET /v1/conditioning` returns IDs, kinds, digests and source provenance. Registration
hashes an exact non-executable file set and asks a capable worker to validate it:

| Kind | Required files | Submission use options |
| --- | --- | --- |
| `vae` | `config.json`, `diffusion_pytorch_model.safetensors` | none |
| `textual-inversion` | `embedding.safetensors` | `token`, `encoder` |
| `ip-adapter` | `ip_adapter.safetensors`, `image_encoder/config.json`, `image_encoder/model.safetensors`, `preprocessor_config.json` | `scale` between 0 and 2 |

No other files or symlinks inside these directories are accepted. The worker
validates the architecture/tensor formats, including the textual-inversion file's
single `embedding` tensor. No pickle, Python source or downloaded model code runs.
Optional `source` metadata uses the same provenance fields as adapters.

A submission or recipe operation may include `conditioning`:

```json
{"conditioning":[
  {"conditioning_id":"my-vae"},
  {"conditioning_id":"my-style","token":"<style>","encoder":"text_encoder"},
  {"conditioning_id":"my-ip-adapter","scale":0.6}
],"inputs":{"prompt":"a <style> landscape","reference_image":"input_..."},
"parameters":{"clip_skip":2}}
```

At most four components may be loaded: one VAE, one IP-Adapter and one embedding
for each of `text_encoder` and `text_encoder_2`. Tokens contain 1–80 characters and
no whitespace. A first-operation submission setting overrides its recipe's list;
subsequent operations use their own declared list. An empty list explicitly clears
conditioning. Changes to component digests, scale, token or encoder trigger a load
identity change; components never silently carry over to later jobs.

IP-Adapter is text-to-image only, requires a namespace-owned uploaded PNG in
`inputs.reference_image`, and cannot combine with ControlNet. The pinned IP-Adapter
preprocessor explicitly controls its encoder's resizing/cropping; Runtime neither
resizes nor substitutes the uploaded image. The reference image is retained while
the job uses it, verified before worker upload, and included in manifest provenance.
The worker handle is collected after the operation commits.

Conditioning requires `offload: none`, default quantization/compilation options and
a reference `sdxl-base` bundle. `parameters.clip_skip` may be 1 or 2 on advertised
reference SDXL text-to-image, image-to-image and inpaint tasks. Unsupported backend,
family, options and combinations fail admission before queuing.

Manifests record component IDs, kinds, content digests and every use option, together
with reference input digests and effective CLIP settings. Intermediate cache keys
include the complete conditioning identity and reference-image bytes. Public jobs
omit full component descriptors and input namespaces.

The Runtime suite includes closed-file catalog and load-identity unit tests plus
HTTP fixture coverage for registration, load/clear, IP input ownership, refusal
cases and provenance. Real image quality and hardware acceptance use the separate
GPU jobs; protocol fixtures are not inference evidence.
