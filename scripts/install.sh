#!/bin/sh
# Usage: curl -fsSL .../scripts/install.sh | sh -s -- 1.1.0 "$HOME/.local/bin"
set -eu
version=${1:-}
install_dir=${2:-${HOME:?HOME must be set}/.local/bin}
case "$version" in ''|*[!0-9A-Za-z.+-]*) echo 'Pass an explicit release version, for example: sh install.sh 1.1.0' >&2; exit 2;; esac
case "$(uname -s):$(uname -m)" in Linux:x86_64) ;; *) echo 'This installer supports Linux x64 only.' >&2; exit 2;; esac
for command in curl sha256sum tar awk mktemp; do command -v "$command" >/dev/null 2>&1 || { echo "Missing command: $command" >&2; exit 2; }; done
archive="media-runtime-v${version}-linux-x64.tar.gz"
base="https://github.com/jxburros/Media-Runtime/releases/download/v${version}"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT HUP INT TERM
curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' --tlsv1.2 "$base/$archive" -o "$work/$archive"
curl --fail --silent --show-error --location --proto '=https' --proto-redir '=https' --tlsv1.2 "$base/checksums.txt" -o "$work/checksums.txt"
awk -v archive="$archive" '$2 == archive || $2 == "*" archive { if (length($1) != 64 || $1 ~ /[^0-9a-fA-F]/) exit 2; print $1 "  " archive; count++ } END { if (count != 1) exit 2 }' "$work/checksums.txt" > "$work/selected-checksum.txt"
(cd "$work" && sha256sum --check selected-checksum.txt)
# Extract the single contracted payload to stdout, never archive paths onto the filesystem.
tar -xOzf "$work/$archive" media-runtime > "$work/media-runtime"
[ -s "$work/media-runtime" ] || { echo 'Archive has no runtime executable' >&2; exit 1; }
chmod 700 "$work/media-runtime"
"$work/media-runtime" --version | awk -v version="$version" '$0 == version { ok=1 } END { exit !ok }'
mkdir -p "$install_dir"
[ ! -e "$install_dir/media-runtime" ] && [ ! -L "$install_dir/media-runtime" ] || { echo 'Destination exists. Remove or move it explicitly before installing.' >&2; exit 1; }
# The verified file is copied into a private staging name on the destination filesystem.
stage=$(mktemp "$install_dir/.media-runtime.XXXXXXXX")
cp "$work/media-runtime" "$stage"
chmod 755 "$stage"
# noclobber publication avoids replacing a binary another installer just wrote.
if ln "$stage" "$install_dir/media-runtime"; then rm -f "$stage"; else rm -f "$stage"; exit 1; fi
printf 'Installed Media Runtime %s at %s/media-runtime\n' "$version" "$install_dir"
