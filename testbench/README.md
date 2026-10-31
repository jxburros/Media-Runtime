# Testbench

One command that answers "do Media Runtime and Media Engine work on my machine, with my local
models?" It starts Engine and Runtime in a fresh run directory, registers your models, runs one
small job per feature through Runtime's public API, checks each output against its recorded size
and SHA-256, and writes `report.html` showing every result and the generated images, audio and
frame archives.

It checks that things work. It does not judge image quality or speed; the settings are
deliberately small so a full run is quick.

It is a single Python file using only the standard library (Python 3.9 or later). It needs no
install and adds nothing to the Runtime binary.

## Try it without models

```sh
python3 testbench/testbench.py --fixture
```

`--fixture` runs Engine's test fixture (from a `Media-Engine` checkout beside this repository,
or `--engine-repo PATH`) in place of real inference. Every check should pass in a few seconds.
The outputs are placeholders. This proves the wiring, not the models.

## Run it on your models

1. Install Engine with its inference extras (see the Engine README). Runtime is built from this
   checkout with `go build` on each run (cached, so it takes seconds). Without Go, it uses
   `media-runtime` from your `PATH`, or you can set `runtime_binary`.
2. Copy `testbench/testbench.example.json` to `testbench.json`. Set `engine_command`, `device`
   and the absolute path of each model directory you have. Leave out the ones you don't have;
   their checks are skipped.
3. Run:

```sh
python3 testbench/testbench.py --config testbench.json
```

The terminal prints one line per check. The run directory
(`./testbench-runs/<timestamp>/`) holds `report.html`, `report.json`, `engine.log`,
`runtime.log`, the generated config files and every output with its manifest. The exit code is
0 when nothing failed.

The first job on each model includes loading the model, so it takes longest.

## Config

| Key              | Meaning |
| ---------------- | ------- |
| `engine_command` | How to start Engine, without the `--catalog`, `--directory`, `--device` and `--port` flags (the testbench adds those). Add `"--backend", "owned-sdxl"` to test the owned pipeline. For the native engine, use `["/abs/Media-Engine/native/target/release/media-engine-native"]`. |
| `device`         | `cuda:0` (default), `cpu`, … |
| `models`         | Model directories by role: `sdxl` (SDXL base, Diffusers layout), `lora` (one PEFT-layout LoRA), `upscaler` (ESRGAN safetensors + `config.json`), `video` (Stable Video Diffusion), `audio` (Stable Audio Open). Each must be the complete, already-downloaded directory the Engine README describes. |
| `settings`       | Optional overrides: `size` (512), `steps` (12), `cancel_steps` (60), `video_steps` (6), `video_frames` (6), `audio_steps` (20), `audio_seconds` (3), `timeout_seconds` per job (1800). |
| `runtime_binary` | Optional path to `media-runtime`. |
| `runtime_config` | Optional keys merged into the generated Runtime config, e.g. `{"max_output_bytes": {...}}`. |

The backend under test is whichever one `engine_command` starts. The testbench reads it from
Engine's startup record and passes it to Runtime, so you don't set it twice.

## Checks

| Id               | What it proves | Needs |
| ---------------- | -------------- | ----- |
| `engine`         | Engine starts, prints its startup record and reports itself ready (no missing dependencies) | — |
| `runtime`        | Runtime starts, connects to Engine and negotiates a protocol | — |
| `register-*`     | Each configured model or adapter registers (hashed and validated through Engine) | the model |
| `text-to-image`  | A seeded SDXL image | `sdxl` |
| `batch`          | `count: 2` gives two images with consecutive seeds | `sdxl`, protocol 1.2 |
| `lora`           | The same image with the LoRA applied | `sdxl`, `lora` |
| `image-to-image` | A variation of the text-to-image output | `sdxl` |
| `upscale`        | The text-to-image output enlarged by the upscaler's factor | `upscaler`, protocol 1.2 |
| `chain`          | A two-step recipe: generate, then upscale | `sdxl`, `upscaler` |
| `image-to-video` | A short frame archive and poster from an image | `video`, protocol 1.3 |
| `text-to-audio`  | A short WAV clip | `audio`, protocol 1.3 |
| `cancel`         | A running job cancels, and the next job still succeeds | `sdxl` |

Use `--only text-to-image,upscale` or `--skip cancel` to pick checks.

A check is **skipped**, not failed, when its model isn't configured or the running Engine
doesn't advertise the task or family. That way the same run works against older releases, today's
releases and later ones. When a planned release lands, rerun the testbench: a skip that turns into
a pass means the feature arrived. The planned native backend shows up the same way. Today Engine
1.0.0's `media-engine-native` passes `engine`, and Runtime 0.10.0 refuses its `native-sdxl`
backend, which the report says plainly. Runtime 1.0.0 (phase R7) is planned to accept it.

## Poking around by hand

`--keep-running` leaves Engine and Runtime up after the checks and prints the URL and token to
export, so you can use `media-runtime doctor`, `media-runtime tui` or `media-runtime run …`
against the same models. Press Ctrl-C to stop both.
