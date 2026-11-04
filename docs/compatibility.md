# Compatibility record

Each release records the worker-protocol minors it negotiates, the Engine releases and fixture it
was tested against, and how. "Fixture" means Engine's explicitly identified test backend launched
by `tests/serve_fixture.py`; it proves protocol behavior, not GPU inference.

## 1.1.0 development acceptance — 2026-11-04

Validated Runtime code commit `822604c1a417ee971857663926ad64c223e8f0f3`
against Engine `c19e7cfd5158b86031c9c7b02b0b4db74636fbd3`. Runtime negotiates
media-worker/1.0–1.4; new capabilities require both the negotiated minor and explicit
advertisement. Native remains 1.3 and has the same durable Runtime lifecycle.

[Hosted fixture validation](https://github.com/jxburros/Media-Runtime/actions/runs/41890913399/job/126787750393)
passes Go vet, the complete Go race suite, TypeScript compilation, the fixture
testbench, static binary/gateway/Debian packaging and Python-client checks. New
integration cases cover managed security, namespace isolation, DAG/foreach,
intermediate reuse/corruption, ControlNet, conditioning, new image families,
hires-fix/outpaint, explicit pixel transforms and encoded-video metadata refusal.

[Real-backend acceptance](https://github.com/jxburros/Media-Runtime/actions/runs/41890913399/job/126787749781)
passes on reference Python, owned Python and native CPU workers: registration,
capability refusal, two-image generation, output hashes, idempotency/restart,
identical-request repeatability, image-to-image, LoRA effects, multi-operation
recipes, SSE and cancellation. These use tiny random weights. Both Runtime CI
jobs and [release validation](https://github.com/jxburros/Media-Runtime/actions/runs/41890914581) passed.

The previous Runtime 0.10.0 is also tested against this Engine:
[full race suite passes](https://github.com/jxburros/Media-Engine/actions/runs/41890897530/job/126787695292).
Studio's companion implementation at `80694be` passes its full backend, frontend,
browser smoke and release checks in
[CI run 41888797038](https://github.com/jxburros/AI-Server-Studio/actions/runs/41888797038).
These checks do not claim a live Studio-to-production-weight GPU walkthrough.

The active local environment blocks SQLite module storage downloads. Local standard-
library/available-dependency race tests pass; the compile-only overlay omits the
SQLite registration import and is explicitly not persistence evidence. Hosted CI
uses the real driver and database. GPU acceptance jobs are prepared as requested;
no RTX 4090 or production weights were available. Production release publication
remains a manual post-merge operation. See [delivery status](delivery-status.md).

## Historical release record

| Runtime | Negotiates                 | Engine tested                    | Fixture protocol | Toolchain        | Suite                         | Result |
| ------- | -------------------------- | -------------------------------- | ---------------- | ---------------- | ----------------------------- | ------ |
| 0.3.0   | media-worker/1.0, 1.1      | 0.3.0 (fixture)                  | 1.1              | Node 22.22, TypeScript 5.8.3 | `npm test` (14 tests)         | pass   |
| 0.4.0   | media-worker/1.0, 1.1      | 0.3.0 (fixture, commit 7ad6c39)  | 1.1 (1.0 by negotiation override) | Go 1.24.7 linux/amd64 | `go test ./...`, also `-race` | pass   |
| 0.5.0   | media-worker/1.0, 1.1, 1.2 | 0.4.0 (fixture)                  | 1.2, and 1.1 pinned with `--protocol` | Go 1.24.7 linux/amd64 | `go test ./...`               | pass   |
| 0.5.0   | media-worker/1.0, 1.1, 1.2 | 0.3.0 (fixture, commit 7ad6c39)  | 1.1              | Go 1.24.7 linux/amd64 | `go test ./...` (1.2 tests skip) | pass   |
| 0.6.0   | media-worker/1.0, 1.1, 1.2 | 0.4.0 (fixture, commit 2e18243)  | 1.2, and 1.1 pinned with `--protocol` | Go 1.24.7 linux/amd64 | `go test ./...`, also `-race` | pass   |
| 0.6.0   | media-worker/1.0, 1.1, 1.2 | 0.5.0 (fixture, commit 245c0f2)  | 1.2, and 1.1 pinned with `--protocol` | Go 1.24.7 linux/amd64 | `go test ./...`               | pass   |
| 0.6.0   | media-worker/1.0, 1.1, 1.2 | 0.3.0 (fixture, commit 7ad6c39)  | 1.1              | Go 1.24.7 linux/amd64 | `go test ./...` (1.2 and linear tests skip) | pass   |
| 0.7.0   | media-worker/1.0, 1.1, 1.2 | 0.4.0 (fixture, commit 2e18243)  | 1.2, and 1.1 pinned with `--protocol`; pools of two fixtures on distinct `--device-index` | Go 1.24.7 linux/amd64 | `go test -race ./...` | pass   |
| 0.7.0   | media-worker/1.0, 1.1, 1.2 | 0.5.0 (fixture, commit 33ba9db)  | 1.2, and 1.1 pinned with `--protocol`; pools of two fixtures on distinct `--device-index` | Go 1.24.7 linux/amd64 | `go test -race ./...` | pass   |
| 0.7.0   | media-worker/1.0, 1.1, 1.2 | 0.3.0 (fixture, commit 7ad6c39)  | 1.1              | Go 1.24.7 linux/amd64 | `go test -race ./...` (1.2, linear and multi-worker pool tests skip; the legacy single-worker test runs) | pass   |
| 0.8.0   | media-worker/1.0, 1.1, 1.2 | 0.6.0 (fixture, commit bae2917)  | 1.2 (fixture lists 1.3; the runtime negotiates 1.2), and 1.1 pinned with `--protocol`; pools of two fixtures; network mode over mutual TLS on loopback | Go 1.24.7 linux/amd64 | `go test -race ./...` | pass   |
| 0.8.0   | media-worker/1.0, 1.1, 1.2 | 0.5.0 (fixture, commit 33ba9db)  | 1.2, and 1.1 pinned with `--protocol`; pools of two fixtures; network mode over mutual TLS on loopback | Go 1.24.7 linux/amd64 | `go test -race ./...` | pass   |
| 0.9.0   | media-worker/1.0, 1.1, 1.2, 1.3 | 0.7.0 (fixture, commit 83d2cc0) | 1.3, 1.2 and 1.1 pinned with `--protocol`; `svd` two-frame bundle; pools; network mode | Go 1.24.7 linux/amd64 | `go test -race ./...` | pass   |
| 0.9.0   | media-worker/1.0, 1.1, 1.2, 1.3 | 0.6.0 (fixture, commit 7c8320c) | 1.3, 1.2 and 1.1 pinned with `--protocol`; `svd` two-frame bundle; pools; network mode | Go 1.24.7 linux/amd64 | `go test ./...` | pass   |
| 0.10.0  | media-worker/1.0, 1.1, 1.2, 1.3 | 0.7.0 (fixture, commit 83d2cc0) | 1.3, 1.2 and 1.1 pinned with `--protocol`; `svd` and `stable-audio-open` bundles; pools; network mode | Go 1.24.7 linux/amd64 | `go test -race ./...` | pass   |
| 0.10.0  | media-worker/1.0, 1.1, 1.2, 1.3 | 0.6.0 (fixture, commit 7c8320c) | 1.3, 1.2 and 1.1 pinned with `--protocol`; `svd` bundle; pools; network mode | Go 1.24.7 linux/amd64 | `go test ./...` (audio tests skip) | pass   |
| 0.10.0  | media-worker/1.0, 1.1, 1.2, 1.3 | 0.8.0 and 1.0.0 (fixture, Engine branch `claude/eager-dirac-7oztji`, commits e78e2ba and 8a2c22f; added later as test evidence) | 1.3, and 1.2/1.1 pinned with `--protocol` | Go 1.24.7 linux/amd64 | `go test ./...` (26 integration tests) | pass   |

0.4.0 also serves a state directory written by Runtime 0.3.0 (`testdata/state-0.3.0`) unchanged:
listing, export, idempotent replay of a 0.3.0 key and a new job.

The 0.5.0 row against Engine 0.3.0 is corrected by 0.6.0's release notes: the CLI package failed
there because its test catalog listed a handle that fixture does not create; the service itself was
unaffected.

The 0.7.0 pool tests run several fixture processes on one host, each reporting a distinct device
index; they prove scheduling, attribution and the device rule, not that two real GPUs were used.

The 0.8.0 network-mode tests run Engine's fixture with `--bind 127.0.0.1 --tls-cert --tls-key
--tls-client-ca --no-parent-watch` and the certificates in `testdata/certs`; the runtime and the
worker share one host, so they prove mutual TLS, fingerprint pinning, refusals and job semantics,
not a real multi-host network. Network mode needs Engine 0.5.0; against older fixtures those tests
skip.

The 0.9.0 video tests use the fixture's `svd` bundle (random stand-in weights, `num_frames` 2,
sizes 64×64, 1024×576 and 576×1024), whose frames are flat colours, not inference. They prove
negotiation, resolution, refusals, the frame archive and poster checks and manifest schema 5. No
Engine release produces the optional `video/mp4` output (Engine ships no encoder), so that path
is tested only with bytes injected by a test hook; it has never been exercised against a worker.

The 0.10.0 audio tests use the fixture's `stable-audio-open` bundle (random stand-in weights,
8 kHz stereo, 256-sample latent frames, 1–47 s), whose clips are seeded sine tones, not
inference. They prove negotiation, resolution, duration rounding, refusals, the WAV checks and
manifest schema 5. Against Engine 0.6.0, which negotiates 1.3 without text-to-audio, the audio
tests skip because its fixture has no audio bundle; refusal by a 1.3 worker that does not list the
task is tested against Engine 0.7.0's fixture with its task list narrowed by a test hook.

Hardware validation: none of these rows ran on a CUDA device or real weights.
