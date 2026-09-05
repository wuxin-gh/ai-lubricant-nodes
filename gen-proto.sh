#!/usr/bin/env bash
# Regenerate the Go protobuf + Connect bindings for the node control-plane
# protocol from the canonical .proto source.
#
# This project is self-contained: the proto source lives here and the generated
# Go lives here — there is no dependency on the external agent-compose repo.
#
# Source of truth:
#   node_server/proto/agentcompose_v2.proto
# Generated into:
#   nodes/common/proto/agentcompose/v2/agentcompose.pb.go
#   nodes/common/proto/agentcompose/v2/agentcomposev2connect/agentcompose.connect.go
#
# The proto's `option go_package` is ai-lubricant-nodes/common/proto/agentcompose/v2,
# so the generated import paths match this module (go.mod: module ai-lubricant-nodes).
#
# Requires: buf, protoc-gen-go, protoc-gen-connect-go on PATH (or in ~/go/bin).
# Run from anywhere:
#   bash nodes/gen-proto.sh
set -euo pipefail

REPO_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
SRC_PROTO="$REPO_ROOT/node_server/proto/agentcompose_v2.proto"
OUT_DIR="$REPO_ROOT/nodes/common/proto/agentcompose/v2"

# The descriptor's embedded source path — kept as proto/agentcompose/v2/agentcompose.proto
# so regeneration produces a minimal diff against the checked-in files.
STAGE_REL="proto/agentcompose/v2/agentcompose.proto"

# Put ~/go/bin on PATH so the buf plugins (protoc-gen-go / protoc-gen-connect-go)
# are found even when they are not on the caller's PATH.
export PATH="$HOME/go/bin:$PATH"

command -v buf >/dev/null 2>&1 || { echo "buf not found on PATH (expected ~/go/bin/buf)" >&2; exit 1; }
[ -f "$SRC_PROTO" ] || { echo "source proto not found: $SRC_PROTO" >&2; exit 1; }

STAGE="$(mktemp -d)"
trap 'rm -rf "$STAGE"' EXIT

mkdir -p "$STAGE/$(dirname "$STAGE_REL")"
cp "$SRC_PROTO" "$STAGE/$STAGE_REL"

cat > "$STAGE/buf.yaml" <<'YAML'
version: v2
YAML

cat > "$STAGE/buf.gen.yaml" <<'YAML'
version: v2
plugins:
  - local: protoc-gen-go
    out: gen
    opt: paths=source_relative
  - local: protoc-gen-connect-go
    out: gen
    opt: paths=source_relative
YAML

( cd "$STAGE" && buf generate )

GEN_V2="$STAGE/gen/proto/agentcompose/v2"
mkdir -p "$OUT_DIR/agentcomposev2connect"
cp "$GEN_V2/agentcompose.pb.go" "$OUT_DIR/agentcompose.pb.go"
cp "$GEN_V2/agentcomposev2connect/agentcompose.connect.go" \
   "$OUT_DIR/agentcomposev2connect/agentcompose.connect.go"

echo "generated:"
echo "  $OUT_DIR/agentcompose.pb.go"
echo "  $OUT_DIR/agentcomposev2connect/agentcompose.connect.go"
