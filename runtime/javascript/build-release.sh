#!/usr/bin/env bash
# Build platform-specific runtime archives for a GitHub Release.
#
# Output:
#   release/agent-compose-runtime-<os>-<arch>.tar.gz
#   release/runtime-VERSION
#   release/runtime-checksums-sha256.txt
#
# TARGETS may be overridden for a CI matrix, for example:
#   TARGETS="windows:amd64" bash build-release.sh
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RELEASE="$HERE/release"
STAGE="$HERE/.release-stage"
TARGETS="${TARGETS:-linux:amd64 linux:arm64 darwin:amd64 darwin:arm64 windows:amd64 windows:arm64}"
VERSION="$(cd "$HERE" && node -p "require('./package.json').version")"

rm -rf "$RELEASE" "$STAGE"
mkdir -p "$RELEASE" "$STAGE"
trap 'rm -rf "$STAGE"' EXIT

( cd "$HERE" && npm run build )

for target in $TARGETS; do
  os="${target%%:*}"
  arch="${target##*:}"
  npm_os="$os"
  npm_cpu="$arch"
  [ "$os" = "windows" ] && npm_os="win32"
  [ "$arch" = "amd64" ] && npm_cpu="x64"

  root="$STAGE/$os-$arch"
  package_root="$root/runtime"
  mkdir -p "$package_root"
  cp "$HERE/package.json" "$HERE/package-lock.json" "$package_root/"
  cp -R "$HERE/dist" "$package_root/dist"

  echo "installing production dependencies for $os/$arch"
  (
    cd "$package_root"
    npm ci --omit=dev --os="$npm_os" --cpu="$npm_cpu"
  )

  archive="$RELEASE/agent-compose-runtime-$os-$arch.tar.gz"
  echo "packing $archive"
  tar -C "$root" -czf "$archive" runtime

done

printf '%s\n' "$VERSION" > "$RELEASE/runtime-VERSION"
(
  cd "$RELEASE"
  sha256sum agent-compose-runtime-*.tar.gz > runtime-checksums-sha256.txt
)

echo "runtime version: $VERSION"
echo "release artifacts:"
ls -la "$RELEASE"
