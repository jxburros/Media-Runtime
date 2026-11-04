# Operator credentials and observability

Environment-variable credentials remain the bootstrap path. An operator creates a scoped key
through `POST /v1/keys` or `media-runtime keys create examples/key-policy.json`. The response
contains a random token **once**. The database keeps its SHA-256 hash; the token has 256 random
bits, so password-style stretching is unnecessary. `keys list` never returns a token or hash.
`keys revoke KEY_ID` takes effect on the next request without restarting Runtime. Revocation
stops new requests; it does not cancel already accepted durable jobs.

A key policy has a name, default namespace, explicit permissions (`jobs.submit`, `jobs.read`,
`models.read`, `models.register`, `admin`), model allow-list, optional RFC3339 `expires_at`,
CIDRs, allowed namespaces, fallback authorization and concurrency, queue and rate limits.
An empty model list permits all models; an empty CIDR list permits any authenticated peer.
Namespace patterns may end in `*`; they do not grant access outside that prefix. `admin`
grants all API permissions but does not override explicit model, CIDR or expiry restrictions.
`models.register` permits registration, but a model allow-list still restricts the registered ID.

Defaults on a managed key are one running job, four queued jobs, and 1,200 requests per sliding
minute. Concurrency and queue limits apply across the namespaces that key can use. The global
queue limit still applies. Accepted jobs retain the key's concurrency limit as part of their
admission record; policy updates use a replacement key. The scheduler preserves the configured
FIFO/reordering policy while enforcing this limit.

Every socket peer has a pre-authentication budget of 6,000 requests per sliding minute, shared
across keys. Forwarded IP headers are ignored; a reverse proxy therefore shares one peer budget.
Managed keys have their own configurable sliding-minute budget. Rate/queue refusals return
`429` with `Retry-After`; keys are verified before their per-key budget is charged. The peer
budget is charged first, including refused authentication and pairing attempts. Limiter maps
have a fixed identity cap and prune expired entries.

## Pairing

`media-runtime pairing create POLICY.json` creates a random, single-use pairing code valid for
five minutes. `media-runtime pairing redeem CODE` exchanges it for the policy's token and the
served TLS certificate fingerprint; no existing key is needed to redeem. Redemption is atomic,
and separately limited to ten attempts per peer per minute. At most 64 pending codes are kept.
The policy must be chosen by an operator before handing the code to another device. Verify the
fingerprint through the operator's trusted channel; do not treat a self-reported fingerprint as
initial trust. Use HTTPS for remote pairing, as for every non-loopback API call.

## Audit and diagnostics

`audit.jsonl` records key changes, pairing redemption, registrations, cancellation requests,
applied fallbacks and refused authentication. It records identifiers and event categories,
never prompts, bearer tokens, input bytes or output bytes. It rotates at one MiB with three
retained files, all created mode 0600. Refused authentication is capped at 100 audit entries per
minute, independently of operator event logging. Storage failures can prevent audit writes;
operators should monitor the state volume's health.

`media-runtime diagnostics export REPORT.json` writes a new mode-0600 JSON file and refuses to
overwrite one. Its allow-list contains version, counts by job state and worker readiness and
protocol/backend versions. It does not serialize configuration, filesystem paths, worker errors,
job requests, prompts, input records, output records or manifests.

Responses carry `X-Request-Id`; errors carry the same `request_id`, a stable `code`/`type`,
message and optional field. Some errors include `repair` with a method/path and a proposed
request patch. Repairs are suggestions: applications must show them to the caller and explicitly
validate/submit the resulting request. Runtime never executes a repair itself.

## Discovery, metrics and API description

`GET /.well-known/media-runtime` is public and carries `schemaVersion: 1`, `surfaces`, API
location, endpoint transport and fingerprint, and the authentication/pairing requirements.
It contains no model inventory or user data.

`GET /metrics` requires an operator/admin credential. Runtime reports retained jobs by recipe
and state, queue depth, retained output bytes, recorded phase durations and worker busy state.
These Runtime observations describe retained history and may decrease when retention removes
jobs. Worker `media_engine_*` samples are fetched through the existing authenticated connection
and re-exported with a `worker` label. Older workers without `/metrics` contribute Runtime
observations only. No prompts or bearer credentials appear in metric labels. A starter
[Grafana dashboard](../examples/observability/grafana.json) plots queue, workers, jobs and memory;
configure its Prometheus data source and the scraper's protected bearer credential locally.

`GET /v1/openapi.json` serves the generated route document to operators. Its committed copy is
[openapi.json](openapi.json). Regenerate with `go run ./cmd/openapi > docs/openapi.json`.
The route-predicate test rejects undocumented direct path/method pairs and removed route
predicates; a byte comparison rejects a stale committed document.

## Security validation scope

The implementation uses real HTTP tests for key CRUD, persistence and revocation, permission and
model/namespace/CIDR refusals, pairing replay, rate limiting, private diagnostics and request-ID
correlation. The standard-library unit suite covers policy validation, limiter bounds and audit
rotation. This is implementation-owned validation, not an independent security review.
