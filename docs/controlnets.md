# SDXL ControlNet

Protocol 1.4 workers may advertise `controlnets`. The reference SDXL backend supports one SDXL ControlNet for text-to-image with `offload: "none"`. Workers that omit this capability reject the request before admission. Real-weight GPU acceptance is a separate runnable acceptance job.

Place an offline Diffusers ControlNet export in an operator-approved catalog directory containing exactly `config.json` and `diffusion_pytorch_model.safetensors`. Symlinks, additional files, executable model code and pickle weights are refused. Register it with operator authorization:

```http
POST /v1/controlnets
Content-Type: application/json

{"controlnet_id":"edges","artifact_handle":"sdxl_edges","source":{"repository":"approved/repository","revision":"immutable-revision","license":"model-license"}}
```

Runtime hashes both files and the descriptor. Engine independently validates the files, descriptor and architecture. `GET /v1/controlnets` lists registered IDs and digests. Registration never downloads weights.

Upload the prepared control image through `POST /v1/inputs` and submit:

```json
{"recipe_id":"portrait","model_id":"sdxl","inputs":{"prompt":"a stone bridge","control_image":"input_UPLOADED_ID"},"parameters":{"width":1024,"height":1024},"controlnets":[{"controlnet_id":"edges","scale":0.8}]}
```

Scale defaults to 1 and must be within 0–2. A control image is required exactly when a ControlNet is selected. Its dimensions must equal the requested output dimensions; Runtime does not resize it. The image belongs to the caller's namespace and remains retained while referenced by a live job. The committed manifest records the control image hash, ControlNet digest, source and scale. Changing any selected weights or scale invalidates the worker's loaded identity.

Recipes may declare `controlnets` on an operation. In schema 2/3, a `control_image` binding can consume a durable uploaded input or an earlier PNG output, for example `{"from":"op:edges/image"}` after an explicit `image-transform` Canny operation. Runtime verifies committed producer bytes before uploading to Engine. This preprocessing remains visible in the recipe and manifest. A submitted `controlnets` list overrides only the first operation's recipe selection.
