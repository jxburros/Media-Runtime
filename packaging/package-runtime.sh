#!/usr/bin/env bash
set -euo pipefail
output=${1:-dist}
mkdir -p "$output"
output=$(cd "$output" && pwd)
version=$(node -p 'require("./release.json").apps["media-runtime"].version')
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o "$stage/media-runtime" ./cmd/media-runtime
test "$("$stage/media-runtime" --version)" = "$version"
if ldd "$stage/media-runtime" 2>&1 | grep -q '=>'; then
  echo 'Runtime binary must be static' >&2; exit 1
fi
cp LICENSE "$stage/LICENSE"
cp README.md "$stage/README.md"
python3 packaging/licenses.py --kind go --output "$stage/THIRD-PARTY-LICENSES.txt"
tar -czf "$output/media-runtime-v$version-linux-x64.tar.gz" -C "$stage" media-runtime LICENSE README.md THIRD-PARTY-LICENSES.txt
npm ci --prefix gateway-client
npm run build --prefix gateway-client
npm pack ./gateway-client --pack-destination "$stage" --ignore-scripts
mv "$stage/media-runtime-gateway-client-$version.tgz" "$output/media-runtime-gateway-client-$version.tgz"

deb_version=${version/-/\~}
deb="$stage/deb"
mkdir -p "$deb/DEBIAN" "$deb/usr/bin" "$deb/usr/share/doc/media-runtime"
cp "$stage/media-runtime" "$deb/usr/bin/media-runtime"
cp "$stage/LICENSE" "$stage/THIRD-PARTY-LICENSES.txt" "$deb/usr/share/doc/media-runtime/"
printf 'Package: media-runtime\nVersion: %s\nArchitecture: amd64\nMaintainer: Media Runtime maintainers\nDescription: Authenticated local media runtime\n' "$deb_version" > "$deb/DEBIAN/control"
dpkg-deb --root-owner-group --build "$deb" "$output/media-runtime_${version}_amd64.deb"

python3 -m build --wheel --outdir "$output" python-client
cp scripts/install.sh "$output/install.sh"
