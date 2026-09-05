# ai-lubricant-nodes

节点客户端（Go）：execution / management 两个二进制，经 h2c NodeConnect 连接
控制面（ai-lubricant-node-server）。Proto 绑定与客户端源自 chaitin/agent-compose
（AGPL-3.0，见 LICENSE/NOTICE）。

```bash
go build ./execution/... ./management/...
bash build.sh          # 多平台产物到 dist/
```

runtime/ 是下发到节点的 agent-compose JS 运行时（vendored），build.sh 打包进
agent-compose-runtime-*.tar.gz。`RUNTIME_DIST=runtime/javascript bash build.sh`
才会带上它。
