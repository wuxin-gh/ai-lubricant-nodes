#!/usr/bin/env bash
#
# 一键打包节点发行版：两个 Go 二进制 + runtime tar.gz，按版本号分目录落到
# nodes/dist/<version>/。产物直接拖进市场管理页「上传新版本」弹框即可。
#
# 这是 nodes/build.sh 的"上层编排"：build.sh 只负责把产物铺到 dist/，本脚本额外负责
#  - 默认从 nodes/runtime/javascript 取 runtime（含 dist/cli.js），可覆盖
#  - 默认版本号 = YYYYMMDD-HHMM（每次打都是新版本；同分钟多次打自动加序号 -2/-3/...）
#    可 VERSION= 显式覆盖（也接受 semver 如 1.2.3，与历史版本兼容）
#  - 把版本号烧进两个 Go 二进制（-X main.buildVersion），节点靠它上报 client_version
#
# 为什么不生成 version.json / checksums：走市场上传时，version.json 由服务端在上传后
# 自动生成（_refresh_node_release_marker 从最新 published 版本投影），checksums 用不上。
# 上传弹框只认 node-execution-* / agent-compose-node-management-* / node-runtime.tar.gz。
#
# 用法（从仓库根）：
#   bash nodes/pack-release.sh                       # 默认：runtime=nodes/runtime/javascript，版本=日期-时分
#   VERSION=1.2.3 bash nodes/pack-release.sh         # 显式指定版本（也接受 semver）
#   RUNTIME_DIST=/path/to/runtime bash nodes/pack-release.sh
#   SKIP_RUNTIME=1 bash nodes/pack-release.sh        # 只打两个 Go 二进制，不含 runtime
#   PLATFORMS="linux/amd64" bash nodes/pack-release.sh   # 只打指定平台（默认全 6 平台）
#
# 产物布局（nodes/dist/<version>/，全部拖进上传弹框）：
#   node-execution-<os>-<arch>[.exe]
#   agent-compose-node-management-<os>-<arch>[.exe]
#   node-ios-<os>-<arch>[.exe]
#   node-runtime.tar.gz                               (除非 SKIP_RUNTIME=1；通用包，一份)
#
# 注意：上传弹框里版本号要手填，必须填脚本打印的版本号（跟烧进二进制的一致），
# 否则节点自报的 client_version 与市场 manifest 的 version 对不上，升级判定会错。
set -euo pipefail
#
# 注意：version.json 里的 download_url 默认是 GitHub Release 直链占位。
#   - 如果走"市场管理页上传"流程，市场会按上传结果重写 version.json，这里的占位会被覆盖，无需手改。
#   - 如果走"本地 dist + 手动提交 version.json"流程，上传 Release 资产后按 Release 实际 URL 改一下即可。
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$HERE/.." && pwd)"
DIST_ROOT="$HERE/dist"

# ── 参数 ────────────────────────────────────────────────────────────────────
VERSION="${VERSION:-}"
RUNTIME_DIST="${RUNTIME_DIST:-$HERE/runtime/javascript}"
SKIP_RUNTIME="${SKIP_RUNTIME:-0}"
# 默认全平台；可传 PLATFORMS="linux/amd64 windows/amd64" 缩减
PLATFORMS="${PLATFORMS:-linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64}"

# ── 版本号 ──────────────────────────────────────────────────────────────────
# 默认用 YYYYMMDD-HHMM（日期 + 时分），同一天多次打包每次都不一样。
# 同分钟内多次打会在末尾加序号 -2/-3/... 避免覆盖。
# validator 接受此格式（_is_node_version），也兼容显式传的 semver。
if [ -z "$VERSION" ]; then
  VERSION="$(date +%Y%m%d-%H%M)"
fi
echo "==> 打包版本: $VERSION"

# 同分钟内多次打：若目录已存在，追加序号 -2/-3/... 不覆盖旧产物。
OUT_BASE="$DIST_ROOT/$VERSION"
OUT="$OUT_BASE"
n=2
while [ -e "$OUT" ]; do
  OUT="$OUT_BASE-$n"
  n=$((n + 1))
done
VERSION_TAG="$VERSION"
if [ "$OUT" != "$OUT_BASE" ]; then
  # 同分钟多次打：把序号也带进版本号，保证 version.json 的 version 唯一。
  VERSION_TAG="$(basename "$OUT")"
fi

# ── runtime 来源校验 ────────────────────────────────────────────────────────
if [ "$SKIP_RUNTIME" != "1" ]; then
  if [ ! -f "$RUNTIME_DIST/dist/cli.js" ]; then
    echo "RUNTIME_DIST=$RUNTIME_DIST 不含 dist/cli.js" >&2
    echo "若暂时不想打 runtime，设 SKIP_RUNTIME=1 重跑" >&2
    exit 1
  fi
  RUNTIME_SRC="$(cd "$RUNTIME_DIST" && pwd)"
  echo "==> runtime 来源: $RUNTIME_SRC"
else
  RUNTIME_SRC=""
  echo "==> 跳过 runtime 打包（SKIP_RUNTIME=1）"
fi

# ── 产物目录（按版本分目录，所有产物平铺，全都要上传）──────────────────────
mkdir -p "$OUT"
echo "==> 产物目录: $OUT"

# ── Go 二进制 ───────────────────────────────────────────────────────────────
build_binary() {
  local role_dir="$1" label="$2" os="$3" arch="$4"
  local ext=""; [ "$os" = "windows" ] && ext=".exe"
  local name="$label-$os-$arch$ext"
  echo "  build $name"
  ( cd "$HERE" && GOOS="$os" GOARCH="$arch" \
    go build -ldflags="-s -w -X main.buildVersion=$VERSION_TAG" -o "$OUT/$name" "./$role_dir" )
}

echo "==> 打 Go 二进制"
for ta in $PLATFORMS; do
  os="${ta%%/*}"; arch="${ta##*/}"
  build_binary execution node-execution "$os" "$arch"
  build_binary management agent-compose-node-management "$os" "$arch"
  build_binary ios node-ios "$os" "$arch"
done

# ── runtime tar.gz（通用包，一份）─────────────────────────────────────────
# dist/cli.js 是**未 bundle** 的 ESM：它 `import` commander 等第三方包，所以归档里
# 必须带 node_modules，否则节点解压后一跑就 ERR_MODULE_NOT_FOUND 立刻退出（表现为
# 会话 exit_code=1 秒退，且看不出跟 runtime 有关）。这里只装生产依赖（--omit=dev），
# 并剔除 release/coverage/test 与源码 src，把体积压在合理范围内。
# JS runtime 平台无关（节点用宿主 Node.js 跑 dist/cli.js），只打一份通用包
# node-runtime.tar.gz，所有平台共用，不再按 (os, arch) 出 6 份重复归档。
RUNTIME_ARTIFACT="node-runtime.tar.gz"
if [ -n "$RUNTIME_SRC" ]; then
  echo "==> 打 runtime tar.gz（通用包 $RUNTIME_ARTIFACT）"
  STAGE="$(mktemp -d)"
  trap 'rm -rf "$STAGE"' EXIT
  mkdir -p "$STAGE/runtime"
  cp -R "$RUNTIME_SRC/." "$STAGE/runtime/"
  # 源目录可能带着 dev 依赖的 node_modules；一律丢掉，下面重装干净的生产依赖。
  rm -rf "$STAGE/runtime/node_modules" "$STAGE/runtime/release" \
         "$STAGE/runtime/coverage" "$STAGE/runtime/test"

  echo "==> 安装 runtime 生产依赖（不含 dev / SDK 自带平台 CLI）"
  if [ ! -f "$STAGE/runtime/package-lock.json" ]; then
    echo "runtime 源缺少 package-lock.json，无法可复现地装依赖" >&2
    exit 1
  fi
  # claude-agent-sdk / codex-sdk 把各平台 CLI 二进制声明为 optionalDependencies。
  # runtime 本身通过 resolveExecutable 使用节点已安装、管理员批准的 claude/codex
  # CLI，因此这些可选平台包既重复又有害：Windows stage 会把数百 MB 的 .exe 塞进
  # Linux/macOS 归档。只保留 SDK JS 本体及 commander 等真正的生产依赖。
  ( cd "$STAGE/runtime" && npm ci --omit=dev --omit=optional --no-audit --no-fund )
  if [ ! -d "$STAGE/runtime/node_modules" ]; then
    echo "npm ci 之后仍无 node_modules，拒绝打出跑不起来的包" >&2
    exit 1
  fi

  # 自检：归档的内容必须真的能启动。以前删了 node_modules 打出 114KB 的"空壳包",
  # 装到节点上才发现跑不起来；这道门禁把那类问题挡在打包阶段。
  echo "==> 自检 runtime 可启动（node dist/cli.js --version）"
  if command -v node >/dev/null 2>&1; then
    if ! ( cd "$STAGE/runtime" && node dist/cli.js --version ); then
      echo "runtime 自检失败：dist/cli.js 起不来，拒绝出包" >&2
      exit 1
    fi
  else
    echo "  跳过（本机没有 node），注意未做启动自检" >&2
  fi

  echo "  pack $RUNTIME_ARTIFACT"
  ( cd "$STAGE" && tar -czf "$OUT/$RUNTIME_ARTIFACT" runtime )

  # 再验一次**归档本身**（而不是 stage 目录）：解压到临时目录跑一遍，确保 tar 内容
  # 完整。归档是节点真正拿到的东西，只验 stage 不够。
  if command -v node >/dev/null 2>&1; then
    echo "==> 自检归档解压后可启动"
    VERIFY="$(mktemp -d)"
    tar -xzf "$OUT/$RUNTIME_ARTIFACT" -C "$VERIFY"
    if ! ( cd "$VERIFY/runtime" && node dist/cli.js --version ); then
      rm -rf "$VERIFY"
      echo "归档自检失败：解压后 dist/cli.js 起不来，拒绝出包" >&2
      exit 1
    fi
    rm -rf "$VERIFY"
  fi
fi

# ── 汇总 ───────────────────────────────────────────────────────────────────
echo
echo "==> 完成，产物在: $OUT"
ls -la "$OUT"
echo
echo "================================================================"
echo "  版本号: $VERSION_TAG"
echo "  上传弹框里版本号填这个，跟烧进二进制的一致（节点靠它判定要不要升级）"
echo "================================================================"
echo "下一步：把 $OUT/ 下的文件拖进市场管理页「上传新版本」弹框。"
echo "  弹框只认 node-execution-* / agent-compose-node-management-* / node-ios-* / node-runtime.tar.gz，版本说明/状态在弹框里填。"
echo "  上传后服务端自动写 node-releases/version.json，无需手动提交。"
