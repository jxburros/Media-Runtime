# Changelog

## 0.1.0 — 2026-10-26

Implemented the standalone CLI/API, authenticated namespaces, explicit model/recipe registration,
SQLite FIFO jobs and event history, idempotency, cancellation, worker supervision and restart
reconciliation. Outputs are verified and committed with provenance before success; retention is
explicit and exports preserve originals. Added a gateway client, operator examples, decisions and
cross-process contract tests. Studio integration is delivered in its companion repository.
