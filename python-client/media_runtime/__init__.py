"""Media Runtime's public API; no Engine credentials or inference dependencies."""

from __future__ import annotations

import hashlib
import ipaddress
import json
import ssl
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path
from typing import Any, Iterator


class RuntimeError(Exception):
    def __init__(self, status: int, payload: dict[str, Any]):
        self.status = status
        self.payload = payload
        self.code = payload.get("error", {}).get("code", "request_failed")
        super().__init__(payload.get("error", {}).get("message", self.code))


class _NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


class Client:
    def __init__(self, url: str, token: str, namespace: str | None = None,
                 ca_file: str | None = None, timeout: float = 30):
        parsed = urllib.parse.urlsplit(url)
        host = parsed.hostname or ""
        try:
            loopback = ipaddress.ip_address(host).is_loopback
        except ValueError:
            loopback = host.lower() == "localhost"
        if parsed.scheme not in ("http", "https") or not host or parsed.username or parsed.password:
            raise ValueError("expected an HTTP(S) service URL without credentials")
        if parsed.scheme == "http" and not loopback:
            raise ValueError("non-loopback APIs require HTTPS")
        if parsed.query or parsed.fragment or parsed.path not in ("", "/"):
            raise ValueError("service URL must not contain a path, query, or fragment")
        self.url, self.token, self.namespace, self.timeout = url.rstrip("/"), token, namespace, timeout
        context = ssl.create_default_context(cafile=ca_file)
        self.opener = urllib.request.build_opener(_NoRedirect(), urllib.request.HTTPSHandler(context=context))

    def request(self, method: str, path: str, body: Any = None, *, data: bytes | None = None,
                headers: dict[str, str] | None = None):
        if not path.startswith("/") or path.startswith("//"):
            raise ValueError("API paths must be absolute local paths")
        supplied = {"Authorization": f"Bearer {self.token}"}
        if self.namespace:
            supplied["X-Media-Namespace"] = self.namespace
        if body is not None:
            data = json.dumps(body, separators=(",", ":")).encode()
            supplied["Content-Type"] = "application/json"
        supplied.update(headers or {})
        request = urllib.request.Request(self.url + path, method=method, data=data, headers=supplied)
        try:
            return self.opener.open(request, timeout=self.timeout)
        except urllib.error.HTTPError as error:
            raw = error.read(1024 * 1024)
            error.close()
            try:
                payload = json.loads(raw)
            except (ValueError, UnicodeError):
                payload = {"error": {"code": "http_error", "message": f"HTTP {error.code}"}}
            raise RuntimeError(error.code, payload) from None

    def json(self, method: str, path: str, body: Any = None, **kwargs) -> Any:
        with self.request(method, path, body, **kwargs) as response:
            return json.load(response)

    def submit(self, request: dict[str, Any], idempotency_key: str | None = None) -> dict[str, Any]:
        headers = {"Idempotency-Key": idempotency_key} if idempotency_key else {}
        return self.json("POST", "/v1/jobs", request, headers=headers)

    @staticmethod
    def _id(value: str) -> str:
        return urllib.parse.quote(value, safe="")

    def status(self, job_id: str) -> dict[str, Any]:
        return self.json("GET", f"/v1/jobs/{self._id(job_id)}")

    def cancel(self, job_id: str) -> dict[str, Any]:
        return self.json("POST", f"/v1/jobs/{self._id(job_id)}/cancel")

    def wait(self, job_id: str, timeout: float = 600, interval: float = 0.25) -> dict[str, Any]:
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            job = self.status(job_id)
            if job["state"] in ("succeeded", "failed", "cancelled"):
                return job
            time.sleep(interval)
        raise TimeoutError(f"job {job_id} did not finish before the deadline")

    def upload_input(self, png: bytes) -> dict[str, Any]:
        return self.json("POST", "/v1/inputs", data=png, headers={"Content-Type": "image/png"})

    def events(self, job_id: str, after: int = 0) -> Iterator[dict[str, Any]]:
        headers = {"Last-Event-ID": str(after)}
        with self.request("GET", f"/v1/jobs/{self._id(job_id)}/events", headers=headers) as response:
            fields: dict[str, str] = {}
            for raw in response:
                line = raw.decode("utf-8").rstrip("\r\n")
                if not line:
                    if "data" in fields:
                        yield {"id": int(fields.get("id", 0)), "event": fields.get("event", "message"),
                               "data": json.loads(fields["data"])}
                    fields = {}
                elif not line.startswith(":") and ":" in line:
                    key, value = line.split(":", 1)
                    fields[key] = value.lstrip(" ")

    def manifest(self, job_id: str) -> dict[str, Any]:
        return self.json("GET", f"/v1/jobs/{self._id(job_id)}/manifest")

    def export(self, job_id: str, directory: str | Path) -> list[Path]:
        job = self.status(job_id)
        destination = Path(directory)
        destination.mkdir(parents=True, exist_ok=True)
        extensions = {"image/png": ".png", "application/zip": ".zip", "video/mp4": ".mp4", "audio/wav": ".wav"}
        written = []
        for output in job["outputs"]:
            output_id = output["output_id"]
            if not output_id or any(c not in "abcdefghijklmnopqrstuvwxyz0123456789_" for c in output_id):
                raise ValueError("unsafe output ID")
            path = destination / (output_id + extensions[output["mime_type"]])
            with self.request("GET", f"/v1/jobs/{self._id(job_id)}/outputs/{output_id}") as response:
                data = response.read(int(output["size"]) + 1)
            if len(data) != output["size"] or hashlib.sha256(data).hexdigest() != output["sha256"]:
                raise ValueError("output size or digest differs from the committed record")
            with path.open("xb") as file:
                file.write(data)
            written.append(path)
        return written


__all__ = ["Client", "RuntimeError"]
