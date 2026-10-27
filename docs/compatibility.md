# Compatibility record

Each release records the worker-protocol minors it negotiates, the Engine releases and fixture it
was tested against, and how. "Fixture" means Engine's explicitly identified test backend launched
by `tests/serve_fixture.py`; it proves protocol behavior, not GPU inference.

| Runtime | Negotiates                 | Engine tested                    | Fixture protocol | Toolchain        | Suite                         | Result |
| ------- | -------------------------- | -------------------------------- | ---------------- | ---------------- | ----------------------------- | ------ |
| 0.3.0   | media-worker/1.0, 1.1      | 0.3.0 (fixture)                  | 1.1              | Node 22.22, TypeScript 5.8.3 | `npm test` (14 tests)         | pass   |
| 0.4.0   | media-worker/1.0, 1.1      | 0.3.0 (fixture, commit 7ad6c39)  | 1.1 (1.0 by negotiation override) | Go 1.24.7 linux/amd64 | `go test ./...`, also `-race` | pass   |

0.4.0 also serves a state directory written by Runtime 0.3.0 (`testdata/state-0.3.0`) unchanged:
listing, export, idempotent replay of a 0.3.0 key and a new job.

Hardware validation: none of these rows ran on a CUDA device or real weights.
