# Migrating a ComfyUI text-to-image workflow

`tools/comfyui_import.py` is an offline, out-of-process translator. It accepts a deliberately
small ComfyUI **API-format** graph: one `CheckpointLoaderSimple`, two `CLIPTextEncode` nodes,
one `EmptyLatentImage`, one full-denoise `KSampler`, one `VAEDecode` and one `SaveImage`.
Unknown/custom nodes, additional branches, disconnected nodes, partial denoise and unmapped
checkpoints are refused. Nothing imports ComfyUI, evaluates node code or contacts a server.

First prepare and register the same SDXL weights in Media Runtime using the documented conversion
workflow. Create a model map, for example `{"sdxl.safetensors":"my-registered-sdxl"}`. An explicit
mapping is required because a checkpoint filename is not a Runtime model identity.

```sh
python3 tools/comfyui_import.py workflow-api.json --model-map model-map.json \
  --recipe-id imported-portrait --output ./translated-portrait
media-runtime recipes register ./translated-portrait/recipe.json
```

Review all three generated documents before submitting `request.json` to `POST /v1/jobs` with your
normal namespace and bearer key. The output directory must not already exist; files are private
and never overwrite an earlier import. A translation does not register or submit anything.

Sampler pairs are explicit: ComfyUI `euler/normal` maps to `euler/euler`,
`euler_ancestral/normal` to `euler-ancestral/euler`, `dpmpp_2m/karras` to `dpmpp-2m/karras`,
`ddim/normal` to `ddim/ddim`, and `uni_pc/normal` to `unipc/unipc`. Non-Euler pairs require a
worker that advertises them in protocol 1.4. Runtime still validates all settings against the
actual worker before admission. Seeds must fit Runtime's unsigned 32-bit range; batches use
Runtime's documented successive-seed rule. Results need not be pixel-identical across engines.
`SaveImage.filename_prefix` becomes Runtime's immutable output IDs and is recorded as an explicit
translation note. No image resize, model replacement, precision fallback or custom-node behavior
is inferred.

This does not implement arbitrary ComfyUI graph import. Image editing, ControlNet, custom nodes,
LoRA loader graphs, video and audio graphs are outside this translator's accepted subset; compose
those operations explicitly using Runtime's registered recipes instead.
