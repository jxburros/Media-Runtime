# Integrator compatibility

The public API is `/v1`. Runtime versions and Engine versions are independent: inspect
`GET /v1/runtime` for Runtime's version, negotiated worker minors, backend identity and limits.
Use `/.well-known/media-runtime` to discover surfaces. A protocol-major mismatch fails before
execution. Optional tasks, parameters and optimization values require advertised capabilities;
never infer them from a version number alone.

Clients may rely on these invariants across Runtime 1.x:

- Accepted jobs have durable IDs independent of a worker session. Closing a client does not
  cancel work. A lost Engine attempt fails explicitly; Runtime never silently regenerates it.
- Idempotency is scoped to the caller namespace. Repeating a key with a changed request fails.
- Success is visible only after verified output bytes and the manifest are durably committed.
  Read outputs by advertised IDs and MIME types rather than assuming a single `image.png`.
- The persisted manifest records explicit and resolved settings, backend/ownership, model
  identity, adapters and applied fallbacks. Older manifests are not rewritten on upgrade.
- SSE sequence IDs support `Last-Event-ID`; a resume refusal requires a fresh job snapshot.
- Unknown request fields are errors. New response fields may be added; clients should ignore
  unfamiliar response fields. New request fields are never required within `/v1`.
- Durable inputs and outputs belong to their caller namespace. Managed keys may narrow model,
  namespace and operation access without changing the data's owner.
- Worker support differs by backend. Native 1.0 provides SDXL over loopback; reference Python
  also provides the advertised audio/video families. Capability refusals are normal API results.

Error responses carry `error.code`, `error.message`, optional `field`/`repair`, and `request_id`.
Use codes for control flow, not message text. Respect `429` and `Retry-After`. Terminal job errors
are part of the job record and do not become successful outputs.

The Python and TypeScript gateway clients call the same HTTP API. Both are server-side clients;
applications keep Runtime credentials in their gateway rather than exposing them to browsers.
