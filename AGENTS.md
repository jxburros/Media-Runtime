# Media Runtime contributor constraints

Read `runtime-spec.md`, `docs/decisions.md`, and the relevant implementation-plan or review
item before changing public behavior. Keep the HTTP API, gateway types, CLI, examples and
compatibility record aligned with implemented behavior.

- Strict inputs and capability negotiation precede admission. Never silently change models,
  backend, precision, parameters, dimensions, scheduler or recipe to make a request fit.
- Caller namespaces and operator permissions apply to every durable object and output.
  Tokens, prompts and output bytes do not belong in logs, diagnostics or metrics.
- Commit verified output bytes and the manifest durably before recording job success.
  Cancellation and recovery must never silently regenerate an accepted attempt.
- Downloads and webhooks are explicit opt-in operations with pinned/allowlisted destinations;
  serving inference never downloads model weights. Executable plugins are out of scope.
- Keep Engine protocol negotiation compatible, and test new capabilities against an older minor.
  Native workers advertise only the families and transports they actually implement.
- Run relevant Go tests (including the race detector), the gateway build and Engine integration
  checks. Distinguish fixture, random-weight CPU and real GPU evidence. Do not invent results.
- Update the dependency ledger for new dependencies; preserve release identities, immutable
  versions and manual dry-run-first publication. Do not publish merely to test packaging.
