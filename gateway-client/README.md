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

The service itself is the Go binary at the repository root. This package is versioned with it and
gains each release's new request, output, recipe and worker types in the same change.
