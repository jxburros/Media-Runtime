# Compatibility record

Each release records the worker-protocol minors it negotiates, the Engine releases and fixture it
was tested against, and how. "Fixture" means Engine's explicitly identified test backend launched
by `tests/serve_fixture.py`; it proves protocol behavior, not GPU inference.

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

Hardware validation: none of these rows ran on a CUDA device or real weights.
