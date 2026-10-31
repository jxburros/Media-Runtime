#!/usr/bin/env python3
"""Media testbench: does Media Runtime + Media Engine work end to end on my local models?

    python3 testbench/testbench.py --fixture                  # plumbing only, no weights needed
    python3 testbench/testbench.py --config testbench.json    # your local models

One file, standard library only. It starts Engine (the worker) and Runtime (the service) in a
fresh run directory, registers the models you listed, runs one small job per feature through
Runtime's public API, checks every output against its recorded size and SHA-256, saves it, and
writes report.html next to it. A feature is skipped, not failed, when you did not configure its
model or when the running build does not advertise it yet, so the same checks cover today's
releases and the planned ones. See testbench/README.md.
"""

import argparse
import hashlib
import html
import json
import os
import secrets
import shutil
import signal
import struct
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.request
import zlib
from pathlib import Path

HERE = Path(__file__).resolve().parent
RUNTIME_REPO = HERE.parent

# The model roles a config may list: role -> (Runtime family, how it is registered).
ROLES = {
    "sdxl": ("sdxl-base", "model"),
    "lora": (None, "adapter"),
    "upscaler": ("esrgan", "model"),
    "video": ("svd", "model"),
    "audio": ("stable-audio-open", "model"),
}

# Small settings: enough to prove a feature works, not to judge quality.
QUICK = {"size": 512, "steps": 12, "cancel_steps": 60, "video_steps": 6, "video_frames": 6,
         "audio_steps": 20, "audio_seconds": 3, "timeout_seconds": 1800}
FIXTURE_QUICK = {"size": 64, "steps": 2, "cancel_steps": 60, "video_steps": 2, "video_frames": 2,
                 "audio_steps": 2, "audio_seconds": 2, "timeout_seconds": 60}
# The Engine fixture's own catalog handles for each role.
FIXTURE_HANDLES = {"sdxl": "fixture", "lora": "fixture-lora", "upscaler": "upscaler", "video": "video", "audio": "audio"}


class Skip(Exception):
    pass


class Fail(Exception):
    pass


# ---------------------------------------------------------------------------------------------
# Small helpers


def png(width, height):
    """A diagonal-gradient RGB PNG, so image inputs have some structure to work with."""
    rows = []
    for y in range(height):
        row = bytearray([0])
        for x in range(width):
            row += bytes(((x * 255) // max(width - 1, 1), (y * 255) // max(height - 1, 1), 128))
        rows.append(bytes(row))

    def chunk(kind, data):
        return struct.pack(">I", len(data)) + kind + data + struct.pack(">I", zlib.crc32(kind + data) & 0xFFFFFFFF)

    header = struct.pack(">IIBBBBB", width, height, 8, 2, 0, 0, 0)
    return b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", header) + chunk(b"IDAT", zlib.compress(b"".join(rows))) + chunk(b"IEND", b"")


def tail(path, lines=15):
    try:
        return "\n".join(Path(path).read_text(errors="replace").splitlines()[-lines:])
    except OSError:
        return ""


def protocol_minor(protocol):
    try:
        return int(str(protocol).rsplit(".", 1)[1])
    except (IndexError, ValueError):
        return 0


EXTENSIONS = {"image/png": ".png", "application/zip": ".zip", "audio/wav": ".wav", "video/mp4": ".mp4"}


class Api:
    """A bearer-authenticated JSON client for Runtime or Engine."""

    def __init__(self, url, token):
        self.url, self.token = url.rstrip("/"), token

    def call(self, method, path, body=None, raw=False, content_type="application/json", timeout=120):
        data = None
        if body is not None:
            data = body if isinstance(body, bytes) else json.dumps(body).encode()
        request = urllib.request.Request(self.url + path, data=data, method=method)
        request.add_header("Authorization", "Bearer " + self.token)
        if data is not None:
            request.add_header("Content-Type", content_type)
        try:
            with urllib.request.urlopen(request, timeout=timeout) as response:
                payload = response.read()
        except urllib.error.HTTPError as exc:
            text = exc.read().decode(errors="replace")
            try:
                error = json.loads(text).get("error", {})
                message = f"{error.get('code')}: {error.get('message')}" + (f" (field {error['field']})" if error.get("field") else "")
            except (ValueError, AttributeError):
                message = text[:300]
            raise Fail(f"{method} {path} -> HTTP {exc.code} {message}") from None
        except (urllib.error.URLError, OSError) as exc:
            raise Fail(f"{method} {path} -> {exc}") from None
        return payload if raw else json.loads(payload or b"null")


class Service:
    """A child process whose first stdout line is a JSON startup record."""

    def __init__(self, name, command, env, log_path, cwd=None):
        self.name, self.log_path = name, log_path
        self.log = open(log_path, "w")
        self.process = subprocess.Popen(command, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=self.log,
                                        env=env, cwd=cwd, text=True)
        self.record = None
        box = []
        reader = threading.Thread(target=lambda: box.append(self.process.stdout.readline()), daemon=True)
        reader.start()
        reader.join(120)
        line = box[0] if box else ""
        try:
            self.record = json.loads(line)
        except ValueError:
            self.record = None
        if not isinstance(self.record, dict) or "port" not in self.record:
            self.stop()
            raise Fail(f"{name} did not start.\n{line.strip()}\n{tail(log_path)}".strip())
        # Keep draining stdout so the child never blocks on a full pipe.
        threading.Thread(target=self.drain, daemon=True).start()

    def drain(self):
        try:
            for line in self.process.stdout:
                self.log.write(line)
                self.log.flush()
        except ValueError:  # the log was closed on stop
            pass

    def alive(self):
        return self.process.poll() is None

    def stop(self):
        if self.process.poll() is None:
            self.process.send_signal(signal.SIGTERM)
            try:
                self.process.wait(15)
            except subprocess.TimeoutExpired:
                self.process.kill()
                self.process.wait()
        self.log.close()


# ---------------------------------------------------------------------------------------------
# The testbench


class Bench:
    def __init__(self, config, run_dir, fixture_repo=None):
        self.config, self.run_dir, self.fixture_repo = config, run_dir, fixture_repo
        self.settings = {**(FIXTURE_QUICK if fixture_repo else QUICK), **config.get("settings", {})}
        self.results = []
        self.engine = self.runtime = None
        self.api = self.engine_api = None
        self.worker = {}          # Runtime's view of the worker (/v1/runtime -> worker)
        self.registered = {}      # role -> registration response
        self.handles = {}         # role -> catalog handle
        self.first_image = None   # (path, width, height) from text-to-image, reused as an input
        self.engine_token = secrets.token_hex(32)
        self.runtime_token = secrets.token_hex(32)
        (run_dir / "outputs").mkdir(parents=True, exist_ok=True)

    # -- reporting ------------------------------------------------------------------------------

    def check(self, check_id, title, function, *args):
        start = time.time()
        result = {"id": check_id, "title": title, "status": "pass", "detail": "", "files": []}
        try:
            detail = function(result, *args)
            result["detail"] = detail or ""
        except Skip as exc:
            result.update(status="skip", detail=str(exc))
        except Fail as exc:
            result.update(status="fail", detail=str(exc))
        except Exception as exc:  # a testbench bug should still produce a report
            result.update(status="fail", detail=f"testbench error: {exc!r}")
        result["seconds"] = round(time.time() - start, 1)
        self.results.append(result)
        mark = {"pass": "PASS", "fail": "FAIL", "skip": "skip"}[result["status"]]
        first = result["detail"].splitlines()[0] if result["detail"] else ""
        print(f"  {mark:4}  {title:<34} {result['seconds']:>6}s  {first}", flush=True)
        if result["status"] == "fail" and "\n" in result["detail"]:
            for line in result["detail"].splitlines()[1:]:
                print(f"              {line}")
        return result["status"] == "pass"

    # -- startup --------------------------------------------------------------------------------

    def start_engine(self, result):
        directory = self.run_dir / "engine"
        directory.mkdir()
        env = {**os.environ, "MEDIA_ENGINE_TOKEN": self.engine_token}
        if self.fixture_repo:
            env["PYTHONPATH"] = str(self.fixture_repo / "src")
            command = [sys.executable, str(self.fixture_repo / "tests" / "serve_fixture.py"), str(directory)]
        else:
            catalog = {}
            for role, path in self.config.get("models", {}).items():
                if role not in ROLES:
                    raise Fail(f"unknown model role {role!r}; use {', '.join(ROLES)}")
                if not Path(path).is_absolute() or not Path(path).is_dir():
                    raise Fail(f"models.{role}: {path} is not an absolute path to a directory")
                catalog[role] = path
            (directory / "catalog.json").write_text(json.dumps(catalog, indent=2))
            command = list(self.config["engine_command"]) + [
                "--catalog", str(directory / "catalog.json"), "--directory", str(directory / "work"),
                "--device", self.config.get("device", "cuda:0"), "--port", "0"]
        self.engine = Service("Engine", command, env, self.run_dir / "engine.log")
        record = self.engine.record
        if self.fixture_repo:
            catalog = json.loads((directory / "catalog.json").read_text())
            self.handles = {role: handle for role, handle in FIXTURE_HANDLES.items() if handle in catalog}
            self.catalog = {handle: catalog[handle] for handle in self.handles.values()}
        else:
            self.handles = {role: role for role in catalog}
            self.catalog = catalog
        host = record.get("bind") or "127.0.0.1"
        self.engine_url = f"http://{host}:{record['port']}"
        self.engine_api = Api(self.engine_url, self.engine_token)
        caps = self.engine_api.call("GET", "/v1/capabilities")
        self.engine_caps = caps
        detail = (f"{record.get('backend_id')} {record.get('worker_version')}, protocols up to "
                  f"{record.get('highest_protocol', record.get('protocol'))}, device {caps.get('device')}"
                  f"{' (test fixture: no real inference)' if record.get('test_fixture') else ''}\n"
                  f"tasks: {', '.join(caps.get('tasks', [])) or 'none'}")
        missing = caps.get("missing_dependencies") or []
        if missing:
            detail += f"\nmissing dependencies: {', '.join(missing)}"
        if caps.get("ready") is False:
            raise Fail("Engine is not ready to generate (install its inference extra, or check the device): " + detail)
        return detail

    def runtime_binary(self):
        """runtime_binary from the config, else a build of this checkout, else media-runtime on PATH."""
        binary = self.config.get("runtime_binary")
        if binary:
            return binary
        if not shutil.which("go"):
            found = shutil.which("media-runtime")
            if not found:
                raise Fail("no Go toolchain to build Runtime and no media-runtime on PATH; set runtime_binary")
            return found
        target = self.run_dir / "media-runtime"
        print("  ....  building media-runtime from this checkout", flush=True)
        built = subprocess.run(["go", "build", "-o", str(target), "./cmd/media-runtime"], cwd=RUNTIME_REPO,
                               env={**os.environ, "CGO_ENABLED": "0"}, capture_output=True, text=True)
        if built.returncode != 0:
            raise Fail("go build failed (set runtime_binary in the config instead):\n" + built.stderr[-1500:])
        return str(target)

    def start_runtime(self, result):
        directory = self.run_dir / "runtime"
        config = {
            "directory": str(directory),
            "port": 0,
            "artifact_catalog": self.catalog,
            # No allow_fallbacks: a GPU problem must fail here, not quietly run on the CPU.
            "credentials": [{"token_env": "MEDIA_RUNTIME_TOKEN", "namespace": "owner", "operator": True}],
            "worker": {"url": self.engine_url, "token_env": "MEDIA_ENGINE_TOKEN",
                       "backend_id": self.engine.record.get("backend_id")},
        }
        config.update(self.config.get("runtime_config", {}))
        path = self.run_dir / "runtime.config.json"
        path.write_text(json.dumps(config, indent=2))
        env = {**os.environ, "MEDIA_ENGINE_TOKEN": self.engine_token, "MEDIA_RUNTIME_TOKEN": self.runtime_token}
        try:
            self.runtime = Service("Runtime", [self.runtime_binary(), "serve", "--config", str(path)], env,
                                   self.run_dir / "runtime.log")
        except Fail as exc:
            if "backend_id" in str(exc):
                raise Fail(f"{exc}\nThis Runtime build does not accept backend "
                           f"{config['worker']['backend_id']!r} yet (native-sdxl is planned for Runtime 1.0.0).") from None
            raise
        self.runtime_url = f"http://127.0.0.1:{self.runtime.record['port']}"
        self.api = Api(self.runtime_url, self.runtime_token)
        deadline = time.time() + 60
        while True:
            info = self.api.call("GET", "/v1/runtime")
            self.worker = info.get("worker") or {}
            if self.worker.get("state") == "ready" or time.time() > deadline:
                break
            time.sleep(0.5)
        if self.worker.get("state") != "ready":
            raise Fail(f"Runtime's worker is {self.worker.get('state')!r}: "
                       f"{json.dumps(self.worker.get('error') or self.worker)[:600]}\n{tail(self.run_dir / 'runtime.log')}")
        return (f"Runtime {info.get('runtime_version')}, negotiated {self.worker.get('protocol')} with "
                f"{self.worker.get('backend_id')} ({self.worker.get('ownership')})")

    def register(self, result, role):
        if role not in self.handles:
            raise Skip(f"no {role} model in the config")
        family, kind = ROLES[role]
        if kind == "adapter":
            body = {"adapter_id": role, "artifact_handle": self.handles[role]}
            self.registered[role] = self.api.call("POST", "/v1/adapters", body, timeout=1800)
            return f"digest {self.registered[role].get('digest', '')[:19]}"
        # Workers before media-worker/1.2 list no loaders and load sdxl-base only.
        loaders = {f for loader in self.worker.get("loaders") or [] for f in loader.get("families", [])} or {"sdxl-base"}
        if family not in loaders:
            raise Skip(f"the worker has no loader for the {family} family")
        body = {"model_id": role, "artifact_handle": self.handles[role], "family": family}
        self.registered[role] = self.api.call("POST", "/v1/models", body, timeout=1800)
        estimate = (self.registered[role].get("memory_estimate") or {}).get("value_mb")
        return (f"{family}, {self.registered[role].get('bundle_digest', '')[:19]}"
                + (f", estimate {estimate} MiB" if estimate is not None else ""))

    # -- jobs -----------------------------------------------------------------------------------

    def need(self, *roles, task=None, minor=None):
        for role in roles:
            if role not in self.handles:
                raise Skip(f"no {role} model in the config")
            if role not in self.registered:
                raise Skip(f"the {role} model did not register")
        if task and task not in (self.worker.get("tasks") or []):
            raise Skip(f"the worker does not advertise {task}")
        if minor and protocol_minor(self.worker.get("protocol")) < minor:
            raise Skip(f"needs media-worker/1.{minor}; negotiated {self.worker.get('protocol')}")

    def submit(self, body):
        return self.api.call("POST", "/v1/jobs", body)["job_id"]

    def wait(self, job_id, until=None):
        deadline = time.time() + self.settings["timeout_seconds"]
        while time.time() < deadline:
            job = self.api.call("GET", f"/v1/jobs/{job_id}")
            if until and until(job):
                return job
            if job["state"] in ("succeeded", "failed", "cancelled"):
                return job
            if not self.engine.alive():
                raise Fail("Engine exited during the job\n" + tail(self.run_dir / "engine.log"))
            time.sleep(0.2)
        raise Fail(f"job {job_id} did not finish within {self.settings['timeout_seconds']} s")

    def run_job(self, result, name, body):
        job = self.wait(self.submit(body))
        if job["state"] != "succeeded":
            raise Fail(f"job {job['state']}: {json.dumps(job.get('error'))}")
        files = []
        for output in job.get("outputs", []):
            data = self.api.call("GET", f"/v1/jobs/{job['job_id']}/outputs/{output['output_id']}", raw=True, timeout=600)
            if len(data) != output["size"] or hashlib.sha256(data).hexdigest() != output["sha256"]:
                raise Fail(f"output {output['output_id']} does not match its recorded size/SHA-256")
            path = self.run_dir / "outputs" / f"{name}-{output['output_id']}{EXTENSIONS.get(output['mime_type'], '.bin')}"
            path.write_bytes(data)
            files.append({"path": str(path.relative_to(self.run_dir)), "mime": output["mime_type"], "record": output})
        manifest = self.api.call("GET", f"/v1/jobs/{job['job_id']}/manifest")
        (self.run_dir / "outputs" / f"{name}-manifest.json").write_text(json.dumps(manifest, indent=2))
        result["files"] = files
        return job, files

    def upload(self, data):
        return self.api.call("POST", "/v1/inputs", data, content_type="image/png")["input_id"]

    def image_input(self):
        """The first text-to-image output when there is one, else a generated gradient."""
        if self.first_image:
            path, width, height = self.first_image
            return self.upload((self.run_dir / path).read_bytes()), width, height
        size = self.settings["size"]
        return self.upload(png(size, size)), size, size

    # -- the feature checks ---------------------------------------------------------------------

    def text_to_image(self, result):
        self.need("sdxl", task="text-to-image")
        size, steps = self.settings["size"], self.settings["steps"]
        job, files = self.run_job(result, "text-to-image", {
            "recipe_id": "portrait", "model_id": "sdxl", "inputs": {"prompt": "a red apple on a wooden table, studio light"},
            "parameters": {"width": size, "height": size, "steps": steps, "seed": 42}})
        self.first_image = (files[0]["path"], size, size)
        return f"{size}x{size}, {steps} steps, seed 42, {job['operations'][0]['backend'].get('device', '')}"

    def batch(self, result):
        self.need("sdxl", task="text-to-image", minor=2)
        size = self.settings["size"]
        _, files = self.run_job(result, "batch", {
            "recipe_id": "portrait", "model_id": "sdxl", "inputs": {"prompt": "a lighthouse at dusk"},
            "parameters": {"width": size, "height": size, "steps": self.settings["steps"], "seed": 7, "count": 2}})
        if len(files) != 2:
            raise Fail(f"expected 2 outputs, got {len(files)}")
        seeds = [f["record"].get("seed") for f in files]
        return f"2 images, seeds {seeds}"

    def lora(self, result):
        self.need("sdxl", "lora", task="text-to-image")
        if not (self.worker.get("adapters") or {}).get("formats"):
            raise Skip("the worker does not advertise adapters")
        size = self.settings["size"]
        self.run_job(result, "lora", {
            "recipe_id": "portrait", "model_id": "sdxl", "inputs": {"prompt": "a red apple on a wooden table, studio light"},
            "parameters": {"width": size, "height": size, "steps": self.settings["steps"], "seed": 42},
            "adapters": [{"adapter_id": "lora", "scale": 0.8}]})
        return "adapter at scale 0.8 (compare with the text-to-image output)"

    def image_to_image(self, result):
        self.need("sdxl", task="image-to-image")
        input_id, width, height = self.image_input()
        job, _ = self.run_job(result, "image-to-image", {
            "recipe_id": "variation", "model_id": "sdxl", "inputs": {"prompt": "a green apple on a wooden table", "image": input_id},
            "parameters": {"steps": self.settings["steps"], "strength": 0.6, "seed": 42}})
        return f"{width}x{height} input, strength 0.6"

    def upscale(self, result):
        self.need("upscaler", task="upscale")
        input_id, width, height = self.image_input()
        _, files = self.run_job(result, "upscale", {"recipe_id": "enlarge", "model_id": "upscaler", "inputs": {"image": input_id}})
        record = files[0]["record"]
        return f"{width}x{height} -> {record.get('width')}x{record.get('height')}"

    def chain(self, result):
        self.need("sdxl", "upscaler", task="upscale")
        recipe = {
            "schema_version": 2, "recipe_id": "testbench-chain", "revision": 1,
            "inputs": {"prompt": {"type": "string", "required": True}},
            "operations": [
                {"op_id": "generate", "task": "text-to-image", "model_family": "sdxl-base",
                 "inputs": {"prompt": {"from": "input:prompt"}},
                 "defaults": {"width": self.settings["size"], "height": self.settings["size"], "steps": self.settings["steps"]}},
                {"op_id": "enlarge", "task": "upscale", "model_family": "esrgan",
                 "inputs": {"image": {"from": "op:generate/image"}}},
            ],
            "publish": ["enlarge/image"],
        }
        try:
            self.api.call("POST", "/v1/recipes", recipe)
        except Fail as exc:
            raise Fail(f"registering a two-operation recipe failed: {exc}") from None
        job, files = self.run_job(result, "chain", {
            "recipe_id": "testbench-chain", "model_id": "sdxl", "models": {"enlarge": "upscaler"},
            "inputs": {"prompt": "a mountain cabin in snow"}})
        return f"{len(job.get('operations', []))} operations, published {[f['record']['output_id'] for f in files]}"

    def image_to_video(self, result):
        self.need("video", task="image-to-video", minor=3)
        sizes = self.registered["video"].get("video_sizes") or [[1024, 576]]
        width, height = sizes[0]
        frames = min(self.settings["video_frames"], self.registered["video"].get("default_frames") or 64)
        input_id = self.upload(png(width, height))
        _, files = self.run_job(result, "image-to-video", {
            "recipe_id": "animate", "model_id": "video", "inputs": {"image": input_id},
            "parameters": {"steps": self.settings["video_steps"], "frames": frames, "fps": 7, "seed": 42,
                           "decode_chunk": 2}})
        archive = next(f for f in files if f["record"]["output_id"] == "frames")["record"]
        return f"{width}x{height}, {archive.get('frames')} frames at {archive.get('fps')} fps (unzip frames.zip to view)"

    def text_to_audio(self, result):
        self.need("audio", task="text-to-audio", minor=3)
        low, high = self.registered["audio"].get("duration_range") or [1, 47]
        duration = max(low, min(high, self.settings["audio_seconds"]))
        _, files = self.run_job(result, "text-to-audio", {
            "recipe_id": "soundscape", "model_id": "audio", "inputs": {"prompt": "rain on a tin roof"},
            "parameters": {"steps": self.settings["audio_steps"], "duration_seconds": duration, "seed": 42}})
        record = files[0]["record"]
        return f"{record.get('duration_seconds')} s, {record.get('sample_rate')} Hz, {record.get('channels')} channels"

    def cancellation(self, result):
        self.need("sdxl", task="text-to-image")
        size = self.settings["size"]
        job_id = self.submit({"recipe_id": "portrait", "model_id": "sdxl", "inputs": {"prompt": "a long job to cancel"},
                              "parameters": {"width": size, "height": size, "steps": self.settings["cancel_steps"], "seed": 1}})
        job = self.wait(job_id, until=lambda j: j["state"] == "running" and j.get("progress"))
        if job["state"] != "running":
            raise Fail(f"the job was {job['state']} before it could be cancelled (raise settings.cancel_steps)")
        self.api.call("POST", f"/v1/jobs/{job_id}/cancel", {})
        job = self.wait(job_id)
        if job["state"] != "cancelled":
            raise Fail(f"expected cancelled, got {job['state']}")
        # The worker must accept new work after a cancellation.
        after = self.wait(self.submit({"recipe_id": "portrait", "model_id": "sdxl", "inputs": {"prompt": "after cancel"},
                                       "parameters": {"width": size, "height": size, "steps": 1, "seed": 2}}))
        if after["state"] != "succeeded":
            raise Fail(f"the next job after cancelling {after['state']}: {json.dumps(after.get('error'))}")
        progress = job.get("progress") or {}
        return f"cancelled during {job.get('stage') or 'sampling'}; the next job succeeded" + (
            f" (stopped at step {progress.get('completed_steps')})" if progress.get("completed_steps") else "")

    # -- driving everything ---------------------------------------------------------------------

    CHECKS = [
        ("text-to-image", "Text-to-image", "text_to_image"),
        ("batch", "Batch of two (count)", "batch"),
        ("lora", "LoRA adapter", "lora"),
        ("image-to-image", "Image-to-image", "image_to_image"),
        ("upscale", "Upscale", "upscale"),
        ("chain", "Two-step recipe (generate+upscale)", "chain"),
        ("image-to-video", "Image-to-video", "image_to_video"),
        ("text-to-audio", "Text-to-audio", "text_to_audio"),
        ("cancel", "Cancellation", "cancellation"),
    ]

    def run(self, only=None, skip=()):
        print(f"Run directory: {self.run_dir}")
        try:
            if not self.check("engine", "Engine starts", self.start_engine):
                return
            if not self.check("runtime", "Runtime starts and connects", self.start_runtime):
                return
            for role in ROLES:
                self.check(f"register-{role}", f"Register {role}", self.register, role)
            for check_id, title, method in self.CHECKS:
                if (only and check_id not in only) or check_id in skip:
                    self.results.append({"id": check_id, "title": title, "status": "skip", "detail": "not selected",
                                         "files": [], "seconds": 0})
                    continue
                self.check(check_id, title, getattr(self, method))
        finally:
            self.write_report()

    def stop(self):
        for service in (self.runtime, self.engine):
            if service:
                service.stop()

    # -- report ---------------------------------------------------------------------------------

    def write_report(self):
        summary = {s: sum(1 for r in self.results if r["status"] == s) for s in ("pass", "fail", "skip")}
        record = (self.engine.record if self.engine else {}) or {}
        report = {"created": time.strftime("%Y-%m-%d %H:%M:%S"), "summary": summary,
                  "engine": {k: record.get(k) for k in ("backend_id", "worker_version", "highest_protocol", "test_fixture")},
                  "runtime_worker": {k: self.worker.get(k) for k in ("protocol", "backend_id", "ownership", "tasks")},
                  "settings": self.settings, "results": self.results}
        (self.run_dir / "report.json").write_text(json.dumps(report, indent=2))

        def media(f):
            src = html.escape(f["path"])
            if f["mime"] == "image/png":
                return f'<a href="{src}"><img src="{src}" alt="{src}"></a>'
            if f["mime"] == "audio/wav":
                return f'<audio controls src="{src}"></audio>'
            if f["mime"] == "video/mp4":
                return f'<video controls src="{src}"></video>'
            return f'<a href="{src}">{html.escape(Path(f["path"]).name)}</a>'

        rows = "".join(
            f'<tr class="{r["status"]}"><td>{r["status"].upper()}</td><td>{html.escape(r["title"])}</td>'
            f'<td>{r["seconds"]} s</td><td><pre>{html.escape(r["detail"])}</pre>{"".join(media(f) for f in r["files"])}</td></tr>'
            for r in self.results)
        engine = html.escape(f"{record.get('backend_id')} {record.get('worker_version')}"
                             + (" — TEST FIXTURE, outputs are placeholders" if record.get("test_fixture") else ""))
        page = f"""<!DOCTYPE html>
<html lang="en"><head><meta charset="utf-8"><title>Media testbench report</title>
<style>
body {{ font: 15px/1.4 system-ui, sans-serif; margin: 2rem; color: #222; }}
table {{ border-collapse: collapse; width: 100%; }} td {{ border-top: 1px solid #ddd; padding: .5rem; vertical-align: top; }}
tr.pass td:first-child {{ color: #11733b; font-weight: 600; }} tr.fail td:first-child {{ color: #b3261e; font-weight: 600; }}
td:nth-child(-n+3) {{ white-space: nowrap; }} tr.skip {{ color: #777; }} pre {{ margin: 0 0 .4rem; white-space: pre-wrap; font: 13px ui-monospace, monospace; }}
img {{ max-width: 256px; max-height: 256px; margin: 0 .4rem .4rem 0; border: 1px solid #ccc; image-rendering: auto; }}
</style></head><body>
<h1>Media testbench</h1>
<p>{html.escape(report["created"])} · Engine {engine} · Runtime worker protocol {html.escape(str(self.worker.get("protocol")))}<br>
<strong>{summary["pass"]} passed, {summary["fail"]} failed, {summary["skip"]} skipped.</strong>
Logs: <a href="engine.log">engine.log</a>, <a href="runtime.log">runtime.log</a>; data: <a href="report.json">report.json</a>.</p>
<table>{rows}</table></body></html>
"""
        (self.run_dir / "report.html").write_text(page)


def main():
    parser = argparse.ArgumentParser(description="Smoke-test Media Runtime and Media Engine on local models.")
    parser.add_argument("--config", help="testbench JSON config (see testbench.example.json)")
    parser.add_argument("--fixture", action="store_true",
                        help="use Engine's test fixture instead of real models (checks the plumbing only)")
    parser.add_argument("--engine-repo", default=os.environ.get("MEDIA_ENGINE_REPO", str(RUNTIME_REPO.parent / "Media-Engine")),
                        help="Media-Engine checkout for --fixture (default: beside this repository)")
    parser.add_argument("--only", help="comma-separated check ids to run, e.g. text-to-image,upscale")
    parser.add_argument("--skip", default="", help="comma-separated check ids to skip")
    parser.add_argument("--runs", default=str(Path.cwd() / "testbench-runs"), help="where run directories go")
    parser.add_argument("--keep-running", action="store_true",
                        help="leave Engine and Runtime running afterwards for manual testing (Ctrl-C to stop)")
    args = parser.parse_args()
    if not args.config and not args.fixture:
        parser.error("give --config FILE (your models) or --fixture (no models)")
    config = json.loads(Path(args.config).read_text()) if args.config else {}
    fixture_repo = None
    if args.fixture:
        fixture_repo = Path(args.engine_repo).resolve()
        if not (fixture_repo / "tests" / "serve_fixture.py").exists():
            parser.error(f"no Media-Engine checkout at {fixture_repo}; pass --engine-repo")
    elif "engine_command" not in config:
        parser.error("the config needs engine_command, e.g. [\"/abs/Media-Engine/.venv/bin/media-engine\"]")

    run_dir = Path(args.runs).resolve() / time.strftime("%Y%m%d-%H%M%S")
    bench = Bench(config, run_dir, fixture_repo)
    only = {c.strip() for c in args.only.split(",")} if args.only else None
    skip = {c.strip() for c in args.skip.split(",") if c.strip()}
    try:
        bench.run(only, skip)
        failed = sum(1 for r in bench.results if r["status"] == "fail")
        passed = sum(1 for r in bench.results if r["status"] == "pass")
        print(f"\n{passed} passed, {failed} failed. Report: {run_dir / 'report.html'}")
        if args.keep_running and bench.api:
            print(f"\nStill running. In another terminal:\n  export MEDIA_RUNTIME_URL={bench.runtime_url}\n"
                  f"  export MEDIA_RUNTIME_TOKEN={bench.runtime_token}\n  media-runtime doctor   # or: media-runtime tui\n"
                  "Press Ctrl-C to stop.")
            try:
                while bench.engine.alive() and bench.runtime.alive():
                    time.sleep(1)
            except KeyboardInterrupt:
                pass
        return 1 if failed else 0
    except KeyboardInterrupt:
        print("\ninterrupted")
        return 130
    finally:
        bench.stop()


if __name__ == "__main__":
    sys.exit(main())
