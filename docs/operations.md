# Operator workflows

## Owned workers and managed applications

For an owned `worker.command`, Runtime writes an owner-only catalog from its configured
`artifact_catalog` and substitutes the worker's `--catalog` argument. The catalog is refreshed
atomically when watched folders change. External workers retain their operator-owned catalogs.
A PID guard records the child PID, executable and Linux process start time; startup reaps only
an exact identity match, never a reused PID. The inherited-stdin ownership watch remains active.

`media-runtime serve --managed --port 0 --config FILE` emits NDJSON `ready`, `stopping` and
`stopped` records on stdout. `ready` contains the chosen port and TLS identity. Closing stdin or
sending SIGTERM stops admission, closes the API and stops owned workers. External workers are
never terminated. Normal service mode does not watch stdin.

`media-runtime service install --instance NAME --config FILE` creates a systemd user unit and
enables it. Use `systemctl --user edit media-runtime-NAME.service` to configure protected
`EnvironmentFile` credentials, then `systemctl --user start media-runtime-NAME.service`.
`service status` reads its status; `service uninstall` disables/stops that named unit and removes
only the generated unit. No sudo or system-wide unit is used.

Set `idle_unload_minutes` on a worker to release its model after it is unused for that interval.
Eligible queued jobs preserve affinity; the latest job's explicit `keep_loaded: true` hint keeps
it resident. Idle timers are per worker, so the least recently used eligible worker unloads first.
Memory budgets remain operator-set across Media Runtime and JX Runtime. There is no unilateral
cross-service GPU lease: both services must adopt the same agreement before one can be enforced.

## Watched model folders

`folders add PATH` registers a folder in place through the operator API; `folders list` reports
its scan status, stable opaque handles and entries; `folders remove FOLDER_ID` stops watching
without deleting any source file. Scans run every 30 seconds and recognize Diffusers directories,
ComfyUI safetensors trees, LoRA files and Hugging Face cache snapshots. Handles are derived from
paths, so adding a neighbor does not change a registered file's identity.

File handles name exactly one `.safetensors` file. LoRA registration hashes only that file.
An ESRGAN file additionally needs `<weights>.safetensors.json`; the wire bundle maps that selected
companion to `config.json`. SDXL single-file checkpoints still require the offline converter.
Hugging Face snapshot links are allowed only for an immutable 40-character revision beneath a
`models--…/snapshots/` tree and only to regular files inside the same repository's `blobs/`.
All other bundle symlinks, including directory links, remain refused.

A vanished root on another device is `offline`; a removed entry on the still-mounted device is
`deleted`; an unreadable scan never marks entries deleted. Source folders are never copied or
deleted. Removed handles remain referenced in existing job provenance and cannot be reused for
other bytes without a fresh registration. Already-loaded worker identity remains digest-checked.

## Pinned downloads and Engine installation

Downloads are explicit operator CLI actions, never part of job execution. Supply a reviewed
JSON manifest to `downloads install MANIFEST --directory DIR`:

```json
{
  "schema_version": 1,
  "revision": "0123456789012345678901234567890123456789",
  "allowed_hosts": ["huggingface.co", "cdn-lfs.huggingface.co"],
  "allow_patterns": ["model.safetensors"],
  "files": [{"path": "model.safetensors", "url": "https://huggingface.co/ORG/MODEL/resolve/0123456789012345678901234567890123456789/model.safetensors", "sha256": "REPLACE_WITH_VERIFIED_64_CHARACTER_SHA256", "size": 1234}]
}
```

The example deliberately cannot install until its real hash, revision, hosts and byte size are
provided. Paths must be unique safe relative paths. Transfers check free space, resume `.part`
files with HTTP ranges, verify complete size/SHA-256 and atomically publish. Existing mismatched
files are refused. URL user-info, non-HTTPS/custom-port destinations, private/reserved IPs,
symlink destinations, and redirects outside the host allowlist are refused. DNS is resolved and
pinned at connection time; a redirect is revalidated. Every allowed CDN hostname must be explicit.

`engine install MANIFEST --directory DIR` uses the same downloader. Its manifest requires
`version` and exactly one immutable archive; the install directory is `DIR/VERSION` and must not
already contain a binary. Only the contracted regular `media-engine-native` ELF file is extracted;
archive symlinks and traversal paths are refused. Use the installed absolute path in
`worker.command`. `update` reads public GitHub release metadata; `update MANIFEST` uses an explicitly supplied
manifest instead. Both compare semantic versions without downloading or replacing anything.
The standalone `scripts/install.sh VERSION [BIN_DIRECTORY]` downloads the exact version's
archive and `checksums.txt`, verifies SHA-256 and `--version`, then installs without sudo or
overwriting an existing binary. Release packaging also produces a Linux x64 `.deb`.

Published archive hashes must come from the verified release inventory; no placeholder pin is
accepted as an installable artifact.

## Signed completion webhooks

Webhooks are disabled unless the operator adds a `webhooks` configuration list:

```json
{"webhooks": [{"url": "https://events.example.com/media", "secret_env": "MEDIA_WEBHOOK_SECRET", "allowed_hosts": ["events.example.com"]}]}
```

The secret must contain at least 32 random characters. Terminal notifications contain only
`event_id`, `job_id`, `state`, and `completed_at`, with a stable event ID across retries.
`X-Media-Timestamp` is Unix seconds; `X-Media-Signature` is `v1=` plus the HMAC-SHA256 of
`timestamp + "." + raw request body`. Verify the signature in constant time, reject old timestamps,
and deduplicate the event ID. No prompts, outputs, namespace names or credentials are sent.

Delivery has five persisted attempts with exponential backoff, ten-second request timeouts,
public-address validation and no redirects. A crash can repeat an already-received event; this is
at-least-once delivery, not exactly once. Exhausted attempts remain recorded in the private
`webhooks.json` state file and are not retried indefinitely.
