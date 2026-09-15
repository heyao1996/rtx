# CLAUDE.md — rtx 项目指南（Claude Code）

本文件用于协助 Claude Code 在本仓库内高效、安全地工作。

## 项目是什么

**rtx** — 反向 RPC 远程执行器（Reverse RPC Remote Executor）。核心思路：
内网/隔离目标机器无法被本地直接访问时，在目标机上部署反向连接的 agent，
agent 主动回连公网 VPS 上的 server，本地通过 SSH 隧道访问 server 的控制
API 并下发任务（执行命令 / 读写文件 / 传输数据），如同直接操作目标机。

架构链路：

```
本地(AI/终端) ──SSH 隧道──▶ VPS server ──反连──▶ 内网 agent ──▶ 目标机执行
```

> **授权边界**：依照 README 声明，本工具仅供**授权安全测试 / 红队演练 /
> CTF / 防御性研究**使用。任何超出授权范围的使用建议都不要提供。

## 目录结构

- `cmd/agent` — agent 主程序：回连 server、执行 exec/read/write/list/upload/
  download/kill；Windows 构建内嵌 `cmd/agent/bb/busybox.exe`，支持 Unix 语法
- `cmd/server` — 控制器：接收 agent 回连 + 控制 HTTP API + 任务路由
- `cmd/rtx` — CLI：通过控制 API 下发任务
- `internal/proto` — 线上协议（任务帧）
- `internal/tlsx` — TLS 通道（自签证书 + pin 校验）
- `internal/ws` — WebSocket 回调通道
- `rtxctl` — bash 便捷封装：token 管理 / 默认 agent / TUI picker
- `rtx_ui.py` — curses 节点选择器
- `rtx_mcp_server.py` — stdio MCP 服务器，向 Claude Code/MCP 客户端暴露
  `rtx_ls / rtx_enter / rtx_exec / rtx_read / rtx_write / ...`（内部调用 rtxctl）
- `bin/` — 构建产物（gitignored）

## 构建与校验

纯 Go 标准库、零第三方依赖，离线可构建：

```bash
go build -ldflags="-s -w" -trimpath -o bin/server ./cmd/server
go build -ldflags="-s -w" -trimpath -o bin/rtx ./cmd/rtx
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -trimpath -o bin/agent-linux-amd64 ./cmd/agent

go vet ./...
gofmt -l .        # 应无输出
```

提交 Go 改动前必须保证 `go vet ./...` 与 `gofmt` 通过。

## 代码约定

- **零第三方依赖**：只用标准库；新功能尽量不引入外部模块。
- 协议改动需向后兼容或版本化；`rtxctl` 与 `cmd/rtx` 之间的 flag 属于公开接口。
- Windows agent 的特殊逻辑在 `cmd/agent/embed_windows.go`（busybox 嵌入），
  保持 Unix 语法命令（`ls`/`cat`/`grep`/管道）在 Windows 上可用。
- 不提交任何密钥 / token / 真实部署细节；敏感操作笔记在 gitignored 文件
  （`REDTEAM.md`、`OPSEC-EVASION.md`）中，不要引用或外泄其内容。
- 中文注释为主（与现有代码一致），README 英文为主。

## 常用本地工作流

- 改完立即 `go build` 验证（项目很小，全量构建很快）。
- 端到端推理顺序：agent 注册 → server 路由 → `rtxctl` 下发 → 结果回调。
- MCP 服务器：保持工具名与 inputSchema 稳定（Claude Code 会话内会缓存）。