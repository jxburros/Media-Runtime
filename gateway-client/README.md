# @media-runtime/gateway-client

The typed TypeScript entry point to the Media Runtime HTTP API, used by server-side gateways such
as Studio's. It has no runtime dependencies (it uses `fetch`) and speaks only the public API
documented in [runtime-spec.md](../runtime-spec.md). Credentials belong on the gateway server,
never in a browser.

```sh
npm ci
npm run build
```

```ts
import { MediaRuntimeClient } from "@media-runtime/gateway-client";
const runtime = new MediaRuntimeClient("http://127.0.0.1:7810", process.env.MEDIA_STUDIO_TOKEN!, "studio:personal:user-a");
const { job_id } = await runtime.submit({ recipe_id: "portrait", model_id: "studio-sdxl", inputs: { prompt: "Soft window light" } });
```

A runtime in network mode is reached at its `https://` address; for a certificate issued by a
private CA, start Node with `NODE_EXTRA_CA_CERTS=/path/ca.pem`. Worker summaries carry each
worker's `transport` (`http` or `mtls`), `remote`, `certificate_sha256` and `latency_ms`.

From 0.9.0 submissions may carry the image-to-video parameters (`frames`, `fps`, `motion`,
`noise_augmentation`, `decode_chunk`); a video job's outputs are `frames` (`application/zip`, with
`frames`, `fps` and `duration_seconds` on its record), `poster` (`image/png`) and, only when the
worker produces one, `video` (`video/mp4`), and the job carries `media` facts. Models of the `svd`
family report `video_sizes` and `default_frames`.

The service itself is the Go binary at the repository root. This package is versioned with it and
gains each release's new request, output, recipe and worker types in the same change.
