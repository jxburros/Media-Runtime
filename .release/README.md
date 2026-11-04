# Release and hardware evidence

`release.json` is authoritative. Synchronize `internal/contracts.VERSION`, gateway package
metadata and lockfile, and add a real `CHANGELOG.md` section for each release. Every main
change requires a version advance. All shared workflow references and toolkit checkouts pin
`JX-Holdings-LLC/release-infrastructure` at `d7f42f9910c075ef34915fa2b95cf28dd2be534a`.

After CI passes and the PR is merged, manually dispatch **Release** on `main` with dry-run
left enabled. The workflow captures one source SHA and builds the static Linux x64 runtime
archive, Debian package, gateway-client npm tarball, dependency-free Python client wheel and
checksum-verifying installer. It stages the complete required inventory, generates
`checksums.txt` and verifies the same bytes that a later manual publish will expose. Failure
in any required package prevents the finish job. The gateway tarball is installable directly with
npm; this workflow does not publish a separate npm registry version before the GitHub release.

Inspect the dry-run artifacts, then dispatch with dry-run disabled to publish. Configure real
reviewer and branch restrictions on the `release` environment before relying on them. The
standard profile checks SHA-256 and GitHub upload metadata; it is not an artifact signing
scheme. Tags and PRs do not trigger publication. Implementing these jobs did not create a tag
or publish a release.

## GPU testbench job

The manual **GPU validation** job runs only on `main` on a trusted runner labeled
`self-hosted, linux, x64, media-gpu`. Configure the `gpu-validation` environment with actual
reviewers/main-only restrictions, `MEDIA_GPU_PYTHON` naming an installed Engine Python, and
`MEDIA_GPU_CONFIG` naming a runner-local testbench config based on
`testbench/testbench.example.json`. Supply approved SDXL base, LoRA, ESRGAN, SVD and Stable
Audio Open directories. The job verifies an RTX 4090 and runs the existing public-API
testbench, uploading only the result JSON. Missing models are explicit skips, not evidence
that a feature passed. Repeat with owned and native `engine_command` configurations for their
advertised tasks. The Engine companion workflow runs conformance and optional benchmarks.

No GPU evidence was generated while preparing these definitions. Record observed results,
including failures/skips, with the workflow URL in `docs/compatibility.md` after a real run.

The packaging toolchain is Go 1.24.13, Node 24, Python 3.12.14 and the build-only
`build==1.2.2.post1` PEP 517 frontend for the Python client. The Go binary has no Python or Node
runtime dependency. Its archive includes collected Go module license notices; its Debian
package installs only the binary and notices, without modifying services or requiring an
installer to invoke sudo. Preview SemVer versions use Debian's `~` ordering inside control
metadata so a later stable version upgrades correctly.
