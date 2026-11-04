# Python client

Install this directory with `python -m pip install ./python-client`. It uses only Python's
standard library and calls Runtime's public API, never Engine directly.

```python
import os
from media_runtime import Client

runtime = Client("http://127.0.0.1:7810", os.environ["MEDIA_RUNTIME_TOKEN"])
job = runtime.submit({"recipe_id": "portrait", "model_id": "sdxl",
                      "inputs": {"prompt": "soft window light"}}, idempotency_key="portrait-1")
result = runtime.wait(job["job_id"])
if result["state"] == "succeeded":
    runtime.export(job["job_id"], "./exports")
```

`events(job_id, after=sequence)` resumes SSE, `upload_input(png)` creates a durable input,
`manifest(job_id)` reads provenance, and `json(method, path, body)` exposes other operator
routes. `RuntimeError` carries `status`, `code` and the server's error payload. Remote URLs
require HTTPS; pass `ca_file` for a private CA. Redirects are refused so credentials cannot
escape to another service. Export verifies size and digest and refuses to overwrite files.
