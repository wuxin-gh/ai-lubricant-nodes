# Agent Compose 节点统一镜像（管理节点 + 执行节点共用）。
#
# 本镜像由节点安装脚本在宿主机本地 build（不推任何公共仓库）：下载两个 Go 二进制 +
# entrypoint.sh 到本目录，再 `docker build -t <image> .`。build context 里二进制为不带
# 平台后缀的通用名（安装脚本下载 linux-<arch> 构建后重命名而来）。
#
# entrypoint 按 AGENT_COMPOSE_NODE_ROLE 分派执行哪个二进制：
#   management → /usr/local/bin/node-management
#   execution  → /usr/local/bin/node-execution（默认）
# 这样管理容器与其拉起的子执行容器共用同一镜像，宿主机只缓存一份。
#
# docker-cli 让管理节点能在容器内 `docker run` 子执行节点（通过挂载的 docker.sock）。
FROM alpine:3.22

# 节点容器要能跑 agent 会话、clone 仓库、装编辑器 CLI，光有两个二进制不够。每个包都
# 对应节点运行时确实会 exec 的外部命令，缺一个对应链路就断：
#   git            — provisionGit clone 仓库；fetchResource clone skill/plugin（git 形态）；
#                    fileservice 的 __git_changes__ / __git_diff__ 走 git status/diff。
#   nodejs         — resolveRuntimeLauncher 用 `node dist/cli.js` 跑服务端下发的
#                    agent-compose-runtime（runtime 由 NodeRuntimeUpgrade 帧下发到 state 目录，
#                    不进镜像）；缺 node 则 RuntimeInstalled() 失败、create-ack 直接被拒。
#   npm            — manageEditor 用 `npm i -g <pkg>@latest` 装 claude/codex/gemini/opencode/ocr。
#   openssh-client — git over SSH 克隆（HTTPS+token 走 git 自身即可，SSH 源需要 ssh）。
#   docker-cli     — 管理节点 + 执行节点 docker driver 经 docker.sock 拉子执行容器。
#   ca-certificates — TLS：git/npm/服务端 HTTPS 出站校验。
#   tini           — PID1 + 信号 reap（ENTRYPOINT 用）。
RUN apk add --no-cache \
    ca-certificates \
    docker-cli \
    git \
    nodejs \
    npm \
    openssh-client \
    tini

COPY node-execution /usr/local/bin/node-execution
COPY agent-compose-node-management /usr/local/bin/node-management
COPY entrypoint.sh /usr/local/bin/node-entrypoint

RUN chmod +x \
    /usr/local/bin/node-execution \
    /usr/local/bin/node-management \
    /usr/local/bin/node-entrypoint

# System mode is disabled in container nodes by default: the container HOME is
# the image's HOME, not the operator's host HOME.
ENV AGENT_COMPOSE_NODE_ALLOW_SYSTEM_ENV=off

# 会话工作树是从宿主机 bind mount 进来的（见 execution/docker_executor.go）。宿主机
# 上的 clone 属于节点进程的用户，容器内是 root，git 会报 "detected dubious ownership"
# 并拒绝在里面执行任何命令。system 级 safe.directory 覆盖所有挂载路径，且与 HOME
# 无关（会话 HOME 是每会话临时目录，写 --global 会被换掉）。
RUN git config --system --add safe.directory '*'

ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/node-entrypoint"]
