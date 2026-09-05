#!/bin/sh
# 节点镜像入口分派：按 AGENT_COMPOSE_NODE_ROLE 选执行哪个二进制。
# 不设或非管理角色 → 执行节点（这也是管理节点 `docker run` 拉起子容器时的默认形态）。
# tini 已在 Dockerfile ENTRYPOINT 负责 PID1 / 信号 / reap，这里只做 exec。
set -e

case "${AGENT_COMPOSE_NODE_ROLE:-execution}" in
  management|passive_management)
    exec /usr/local/bin/node-management "$@"
    ;;
  *)
    exec /usr/local/bin/node-execution "$@"
    ;;
esac
