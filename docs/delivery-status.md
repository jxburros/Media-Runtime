# Development delivery status — 1.1.0

The implementation plan's R0–R7 code paths are implemented, including native worker
acceptance and companion Studio integration. The later review's application and
operator features are included in the same development PR.

- Operator layer: hashed managed keys and policies, pre-authentication IP limits,
  per-key admission budgets, audit, pairing, metrics/dashboard, guided errors,
  discovery, diagnostics and generated OpenAPI.
- Lifecycle: watched folders, owned-worker catalog generation, managed stdio
  handshake, systemd user service, pinned installation/downloads, orphan guard,
  oldest-idle-first unload, signed allowlisted webhooks, installers and update checks.
- Media: advertised sampling, explicit pixel operations, inpainting, previews,
  FLUX/SD3.5, ControlNet, conditioning components and verified MP4 output. Shipped
  [hires-fix/outpaint recipes](image-recipes.md) compose explicit operations.
- Workflows: schema-3 DAG/fanout/foreach, namespace- and credential-scoped opt-in
  intermediate reuse, provenance, Python/TypeScript clients and a strict offline
  ComfyUI subset translator. The service core is split by responsibility.

D8 resolves E13 through explicit operator-set per-service GPU budgets. A shared
lease requires an agreed protocol and adoption in both services, so Runtime does
not claim to enforce a cross-service lease or evict another service's process.

The user requested prepared hardware jobs rather than access to an RTX 4090 host.
A2 therefore remains unexecuted hardware acceptance with runnable workflows and
approved-weight inputs. A3's release machinery, manifests and artifacts are
implemented; production publication remains a manual post-merge operation.
No release checksums or successful hardware measurements are invented.

Local full Go execution is blocked by this active environment's network policy
on the SQLite dependency download. Targeted tests using available dependencies,
compile-only checks, and hosted full race/integration jobs are distinguished in
[compatibility](compatibility.md). The compile-only overlay removes only the
SQLite registration import and never substitutes for persistence tests.

PRs: [Runtime #12](https://github.com/jxburros/Media-Runtime/pull/12),
[Engine #12](https://github.com/jxburros/Media-Engine/pull/12), and
[Studio #1979](https://github.com/jxburros/AI-Server-Studio/pull/1979).
