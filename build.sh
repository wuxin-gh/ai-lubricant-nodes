#!/usr/bin/env bash
# Build all agent-compose node binaries (execution + management) for every
# supported OS/arch into ``dist/``, and optionally package one platform-independent
# JavaScript runtime as ``node-runtime.tar.gz``.
#
# Artifact names are the contract shared by three consumers: the install
# scripts, ``node_server/binaries.py:_NODE_BIN_NAME_RE``, and the marketplace
# node-version uploader (which derives role/platform/arch from the filename).
# The script asserts every produced name against that contract, so a future
# rename fails the build instead of silently breaking upload recognition.
#
#   node-execution-<os>-<arch>[.exe]
#   agent-compose-node-management-<os>-<arch>[.exe]
#   node-runtime.tar.gz                              (only with RUNTIME_DIST)
#
# Run from the repo root:
#   bash nodes/build.sh
#
# To also package the runtime, point RUNTIME_DIST at a built runtime directory
# containing ``dist/cli.js`` (the node verifies ``runtime/dist/cli.js`` after
# extraction):
#   RUNTIME_DIST=nodes/runtime/javascript bash nodes/build.sh
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DIST="$HERE/dist"

# Version stamped into both binaries' main.buildVersion via -ldflags -X. Prefer
# an explicit VERSION env (a release tag); otherwise derive <local-date>-
# <local-time>（YYYYMMDD-HHMM）——本地时间口径（2026-09-11 起；UTC 口径的
# 20260911-0930 是最后一个 UTC 版本号，本地时间号恒大于它，排序不回退）。
# 与 pack-release.sh 同口径，dev build 与 release 版本号同格式、可直接互比。
# node 上报此值为 client_version 能力标签。
VERSION="${VERSION:-}"
if [ -z "$VERSION" ]; then
  VERSION="$(date +%Y%m%d-%H%M)"
fi
echo "version: $VERSION"

# 版本目录布局：产物直接进 ``dist/<version>/``（与 node_server/binaries.py 的
# 版本目录解析对齐——``_resolve_candidate_root`` 取含二进制的最新版本子目录，
# ``latest_node_version`` 把目录名当版本号）。release 打包脚本（pack-release.sh）
# 走的就是这个布局；dev build 也用同一布局，服务端 ``/binaries/*`` 路由与
# ``latest_node_version`` 零改动直接吃到新构建。每次构建清掉 dist 全量重建，
# dev 不留历史版本（release 才累积）。
rm -rf "$DIST"
BUILD_DIR="$DIST/$VERSION"
mkdir -p "$BUILD_DIR"

# Marketplace-recognized artifact names. Keep in sync with
# monkeycode_compat/marketplace/validator.py:identify_node_asset.
assert_artifact_name() {
  local name="$1"
  if [[ "$name" =~ ^(node-execution|agent-compose-node-management|node-ios)-(linux|darwin|windows)-(amd64|arm64)(\.exe)?$ ]]; then
    local os="${BASH_REMATCH[2]}" exe="${BASH_REMATCH[4]:-}"
    if [ "$os" = "windows" ] && [ -z "$exe" ]; then
      echo "artifact name rejected (windows binary must end with .exe): $name" >&2
      exit 1
    fi
    if [ "$os" != "windows" ] && [ -n "$exe" ]; then
      echo "artifact name rejected (non-windows binary must not end with .exe): $name" >&2
      exit 1
    fi
    return 0
  fi
  if [[ "$name" =~ ^agent-compose-runtime-(linux|darwin|windows)-(amd64|arm64)\.tar\.gz$ ]]; then
    return 0
  fi
  echo "artifact name rejected (does not match marketplace naming contract): $name" >&2
  exit 1
}

# role:label  ->  import-path
ROLES="execution:node-execution management:agent-compose-node-management"
# os/arch pairs
TARGETS="linux:amd64 linux:arm64 darwin:amd64 darwin:arm64 windows:amd64 windows:arm64"

for role_pair in $ROLES; do
  role="${role_pair%%:*}"
  label="${role_pair##*:}"
  for ta in $TARGETS; do
    os="${ta%%:*}"
    arch="${ta##*:}"
    ext=""
    [ "$os" = "windows" ] && ext=".exe"
    name="$label-$os-$arch$ext"
    assert_artifact_name "$name"
    out="$BUILD_DIR/$name"
    echo "building $out"
    ( cd "$HERE" && GOOS="$os" GOARCH="$arch" go build -ldflags="-s -w -X main.buildVersion=$VERSION" -o "$out" "./$role" )
  done
done

# 版本目录布局下版本号=目录名（node_server/binaries.py 直接读目录名），不再
# 单独写 VERSION 文件——release 打包脚本同口径。


# Optional: package one platform-independent JavaScript runtime archive. The archive
# content is identical on every OS/arch; the node runs it with the host's Node.js.
# Emit one ``node-runtime.tar.gz`` instead of six platform-labelled copies.
RUNTIME_DIST="${RUNTIME_DIST:-}"
if [ -n "$RUNTIME_DIST" ]; then
  if [ ! -f "$RUNTIME_DIST/dist/cli.js" ]; then
    echo "RUNTIME_DIST=$RUNTIME_DIST does not contain dist/cli.js" >&2
    exit 1
  fi
  RUNTIME_SRC="$(cd "$RUNTIME_DIST" && pwd)"
  RUNTIME_VERSION="${RUNTIME_VERSION:-$VERSION}"
  STAGE="$(mktemp -d)"
  trap 'rm -rf "$STAGE"' EXIT
  mkdir -p "$STAGE/runtime"
  cp -R "$RUNTIME_SRC/." "$STAGE/runtime/"
  if [ ! -f "$STAGE/runtime/package-lock.json" ]; then
    echo "runtime source missing package-lock.json" >&2
    exit 1
  fi
  ( cd "$STAGE/runtime" && npm ci --omit=dev --omit=optional --no-audit --no-fund )
  if [ ! -d "$STAGE/runtime/node_modules" ]; then
    echo "npm ci produced no node_modules; refusing to package a broken runtime" >&2
    exit 1
  fi
  if command -v node >/dev/null 2>&1; then
    if ! ( cd "$STAGE/runtime" && node dist/cli.js --version ); then
      echo "runtime self-check failed: dist/cli.js did not start" >&2
      exit 1
    fi
  fi
  echo "packaging $BUILD_DIR/node-runtime.tar.gz"
  ( cd "$STAGE" && tar -czf "$BUILD_DIR/node-runtime.tar.gz" runtime )
  printf '%s\n' "$RUNTIME_VERSION" > "$BUILD_DIR/runtime-VERSION"
else
  echo "skipping runtime packaging (set RUNTIME_DIST=<runtime dir with dist/cli.js> to include it)"
fi

echo "checksums"
( cd "$BUILD_DIR" && sha256sum -- * > checksums-sha256.txt )

echo "done; artifacts in $BUILD_DIR:"
ls -la "$BUILD_DIR"
