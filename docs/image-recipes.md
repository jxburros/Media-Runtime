# Explicit high-resolution refinement and outpainting

Register the JSON recipes in `examples/recipe.hires-fix.json` and
`examples/recipe.outpaint.json` with `POST /v1/recipes`. Both require an Engine
worker advertising protocol 1.4 image transforms. Every operation selects an
explicit registered SDXL model: `model_id` selects the first operation, and
`models` maps each subsequent operation ID to a model. Using the same model for
all operations is supported. The graph and pixel parameters remain in provenance.

`hires-fix` generates at 512×512, resizes with the named bilinear filter to
1024×1024, then performs image-to-image refinement with strength 0.35. Submit a
prompt and `models: {"resize": "sdxl", "refine": "sdxl"}` along with your actual
registered `model_id`. A supplied seed controls the first operation; expose a
number binding or edit the recipe defaults to pin the refinement seed as well.
Only the refined image is published. The intermediate images remain subject to
normal retention.

`outpaint` pads an uploaded image onto a 1024×1024 black canvas at offset
(256,256), then inpaints using the uploaded 1024×1024 mask and prompt. A 512×512
source is centered by these defaults. Larger sources that cannot fit are refused.
Supply `models: {"inpaint": "sdxl"}`. The mask is explicit: black preserves pixels,
white requests regeneration. For border extension, make the mask black over the
original image and white over the padded area. Runtime validates mask dimensions
and ownership. It does not infer a mask or silently rescale either input.

To change sizes, offsets, filter, denoising strength or mask behavior, register a
new recipe revision with those explicit values. Admission validates the complete
graph before queueing; each intermediate is verified and durably committed before
its consumer runs. These examples have contract and fixture HTTP tests; visual
quality with production weights remains part of the GPU acceptance job.
