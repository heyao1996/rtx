# AGENTS.md — rtx project guide for coding agents

This file is read by coding agents (Codex, OpenCode, and similar) to understand
how to work in this repository. Follow it unless a task explicitly overrides it.

## What this project is

**rtx** — a reverse-RPC remote executor. A lightweight Go control-plane that
lets an operator (or an AI) execute commands / read-write files on
inner-network, unreachable machines through reverse-connecting agents,
relayed by a public VPS server. See `README.md` for the full picture.

> **Scope & authorization**: per the repository's README, this tool is for
> authorized security testing / red-team exercises / CTF / defensive research
> ONLY. Do not suggest or perform any use outside the authorized scope of a
> given engagement.

## Repository layout

- `cmd/agent` — the agent binary (executes commands on the target machine,
  dials back to the server). Windows build embeds `cmd/agent/bb/busybox.exe`
  for Unix-syntax exec.
- `cmd/server` — the controller: accepts agent callbacks, holds the control
  HTTP API, routes tasks.
- `cmd/rtx` — CLI client that dispatches tasks through the server control API.
- `internal/proto` — wire protocol (task request/response framing).
- `internal/tlsx` — TLS channel helpers (self-signed cert + pin verification).
- `internal/ws` — WebSocket callback channel helpers.
- `rtxctl` — convenience bash wrapper (token mgmt / default agent / TUI).
- `rtx_ui.py` — node connection picker (curses TUI).
- `rtx_mcp_server.py` — stdio MCP server exposing `rtx_*` tools to Claude Code
  / MCP clients (calls `rtxctl` under the hood).
- `bin/` — build outputs (gitignored).

## Build & verify

Pure Go stdlib — **no third-party dependencies**, so builds always work offline.

```bash
# local binaries
go build -ldflags="-s -w" -trimpath -o bin/server ./cmd/server
go build -ldflags="-s -w" -trimpath -o bin/rtx ./cmd/rtx

# linux agent (typical target platform)
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -trimpath -o bin/agent-linux-amd64 ./cmd/agent

# sanity checks
go vet ./...
gofmt -l .            # should print nothing
```

Before committing any Go change, make sure `go vet ./...` and `gofmt` pass.

## Coding conventions

- Keep the **zero third-party dependency** rule: stdlib only.
- Protocol changes must stay backward compatible, or be versioned in
  `internal/proto`. `rtxctl` communicates with `cmd/rtx` over the control API,
  so flag names are part of the public surface.
- Windows agent behavior is special-cased in `cmd/agent/embed_windows.go` +
  busybox embedding; keep Unix syntax (`ls`/`cat`/`grep`) working there.
- Shell scripts: bash, `set -uo pipefail`, POSIX-ish quoting.
- Do not commit local secrets, tokens, or real deployment details; sensitive
  op notes live in gitignored files (`REDTEAM.md`, `OPSEC-EVASION.md`).

## Workflow hints

- Run `go build` after edits; the project is small — a full build is fast.
- To reason about an end-to-end change, trace: agent registers →
  server routes → `rtxctl`/`cmd/rtx` dispatches → result callback.
- MCP server changes: keep tool schemas stable (names + inputSchema), since
  Claude Code caches them per session.