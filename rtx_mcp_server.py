#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
rtx_mcp_server.py — rtx C2 的 Claude Code MCP 工具服务器（stdio）

让 claude 打开即带内网/远程机器控制能力（等价打开 C2）：
  rtx_ls / rtx_enter / rtx_exec / rtx_read / rtx_write /
  rtx_list / rtx_upload / rtx_download / rtx_info / rtx_exit
  rtx_bgexec / rtx_bgstatus / rtx_bgcancel
  rtx_socks_up / rtx_socks_down / rtx_socks_list（穿透：控制侧起 SOCKS，流量经 agent 网络栈）

实现: MCP stdio (newline-delimited JSON-RPC 2.0)，工具内部调用 rtxctl
（token/默认 agent 持久化在 ~/.claude/rtx/）。无第三方依赖。
"""

import json
import os
import subprocess
import sys

RTXCTL = "rtxctl"  # PATH 中（软链至 py311/bin）


def rtxctl(*args):
    """调 rtxctl，返回 (stdout, returncode)。encoding=utf-8 兜底非 ASCII banner。"""
    try:
        p = subprocess.run(
            [RTXCTL] + list(args),
            capture_output=True, text=True,
            encoding="utf-8", errors="replace",
            timeout=600,
        )
        out = (p.stdout or "") + (p.stderr or "")
        return out.strip(), p.returncode
    except subprocess.TimeoutExpired:
        return "rtxctl 超时（>600s）", 1
    except FileNotFoundError:
        return "rtxctl 不存在（PATH 需含 py311/bin）", 1
    except Exception as e:
        return f"rtxctl 错误: {e}", 1


# ---------------- MCP 工具定义 ----------------

TOOLS = [
    {
        "name": "rtx_ls",
        "description": "列出所有在线 rtx agent（内网/远程上线的机器）",
        "inputSchema": {"type": "object", "properties": {}},
    },
    {
        "name": "rtx_enter",
        "description": "一键进入指定 agent 的执行环境：之后 rtx_exec/read/write 等自动在该机器执行（AI 穿透，本地无感）",
        "inputSchema": {"type": "object", "properties": {
            "agent": {"type": "string", "description": "agent 名（rtx_ls 查看）"},
        }, "required": ["agent"]},
    },
    {
        "name": "rtx_exit",
        "description": "退出远程执行环境，恢复本地后端",
        "inputSchema": {"type": "object", "properties": {}},
    },
    {
        "name": "rtx_exec",
        "description": "在 agent 机器上执行命令（默认当前 enter 的 agent；可指定）。阻塞直到命令完成。",
        "inputSchema": {"type": "object", "properties": {
            "cmd": {"type": "string", "description": "要在目标机器执行的命令"},
            "agent": {"type": "string", "description": "可选，指定 agent"},
        }, "required": ["cmd"]},
    },
    {
        "name": "rtx_bgexec",
        "description": "后台派发长任务（编译/扫描/安装），立即返回 bg task id，不阻塞 agent 与 MCP。用 rtx_bgstatus 轮询进度，rtx_bgcancel 取消。",
        "inputSchema": {"type": "object", "properties": {
            "cmd": {"type": "string", "description": "要在 agent 机器后台执行的命令"},
            "agent": {"type": "string", "description": "可选，指定 agent"},
        }, "required": ["cmd"]},
    },
    {
        "name": "rtx_bgstatus",
        "description": "查询后台任务进度（rtx_bgexec 派发的）",
        "inputSchema": {"type": "object", "properties": {
            "task_id": {"type": "string", "description": "rtx_bgexec 返回的 task id"},
        }, "required": ["task_id"]},
    },
    {
        "name": "rtx_bgcancel",
        "description": "取消正在运行的后台任务",
        "inputSchema": {"type": "object", "properties": {
            "task_id": {"type": "string", "description": "rtx_bgexec 返回的 task id"},
        }, "required": ["task_id"]},
    },
    {
        "name": "rtx_info",
        "description": "获取 agent 机器信息（os/arch/host/user）",
        "inputSchema": {"type": "object", "properties": {
            "agent": {"type": "string", "description": "可选，指定 agent"},
        }},
    },
    {
        "name": "rtx_read",
        "description": "读取 agent 机器上的文件内容",
        "inputSchema": {"type": "object", "properties": {
            "path": {"type": "string", "description": "远程文件路径"},
            "agent": {"type": "string"},
        }, "required": ["path"]},
    },
    {
        "name": "rtx_list",
        "description": "列出 agent 机器上的目录",
        "inputSchema": {"type": "object", "properties": {
            "path": {"type": "string", "description": "远程目录路径"},
            "agent": {"type": "string"},
        }, "required": ["path"]},
    },
    {
        "name": "rtx_write",
        "description": "向 agent 机器写文件（内容或本地文件）",
        "inputSchema": {"type": "object", "properties": {
            "path": {"type": "string", "description": "远程目标路径"},
            "content": {"type": "string", "description": "要写入的内容（与 file 二选一）"},
            "file": {"type": "string", "description": "本地文件路径（与 content 二选一）"},
            "agent": {"type": "string"},
        }, "required": ["path"]},
    },
    {
        "name": "rtx_upload",
        "description": "上传本地文件到 agent 机器",
        "inputSchema": {"type": "object", "properties": {
            "path": {"type": "string", "description": "远程目标路径"},
            "file": {"type": "string", "description": "本地文件路径"},
            "agent": {"type": "string"},
        }, "required": ["path", "file"]},
    },
    {
        "name": "rtx_download",
        "description": "从 agent 机器下载文件到本地",
        "inputSchema": {"type": "object", "properties": {
            "path": {"type": "string", "description": "远程文件路径"},
            "out": {"type": "string", "description": "本地保存路径（默认当前目录同名）"},
            "agent": {"type": "string"},
        }, "required": ["path"]},
    },
    {
        "name": "rtx_socks_up",
        "description": "在【控制侧】起一个 SOCKS5 穿透监听，流量经目标 agent 的网络栈出去：目标机上不开监听端口、不新增连接。之后用 --socks5-hostname 指向该监听即可直达 agent 侧可达的内网（含远端域名解析）。用 rtx_socks_list 查 mux，rtx_socks_down 停。",
        "inputSchema": {"type": "object", "properties": {
            "agent": {"type": "string", "description": "目标 agent id（先 rtx_ls 查看）"},
            "listen": {"type": "string", "description": "控制侧监听地址，默认 127.0.0.1:1080。⚠️ 勿用 0.0.0.0 —— 无认证 SOCKS 会变成开放代理"},
        }, "required": ["agent"]},
    },
    {
        "name": "rtx_socks_down",
        "description": "停止指定的穿透监听（按 mux id）",
        "inputSchema": {"type": "object", "properties": {
            "mux": {"type": "integer", "description": "rtx_socks_list 返回的 mux id"},
        }, "required": ["mux"]},
    },
    {
        "name": "rtx_socks_list",
        "description": "列出当前穿透监听（mux / agent / 监听地址）",
        "inputSchema": {"type": "object", "properties": {}},
    },
]


def handle_tool(name, args):
    a = args or {}
    agent = a.get("agent")
    if name == "rtx_ls":
        return rtxctl("ls")
    if name == "rtx_enter":
        return rtxctl("enter", a.get("agent", ""))
    if name == "rtx_exit":
        return rtxctl("exit")
    if name == "rtx_bgexec":
        if agent:
            return rtxctl("bgexec", "-agent", agent, "-cmd", a.get("cmd", ""))
        return rtxctl("bgexec", "-cmd", a.get("cmd", ""))
    if name == "rtx_socks_up":
        if agent:
            return rtxctl("socks", "-agent", agent, "-listen", a.get("listen") or "127.0.0.1:1080")
        return rtxctl("socks", "-listen", a.get("listen") or "127.0.0.1:1080")
    if name == "rtx_socks_down":
        return rtxctl("socks", "-stop", str(a.get("mux", "")))
    if name == "rtx_socks_list":
        return rtxctl("socks", "-list")
    if name == "rtx_bgstatus":
        return rtxctl("bgstatus", a.get("task_id", ""))
    if name == "rtx_bgcancel":
        return rtxctl("bgcancel", a.get("task_id", ""))
    if name == "rtx_exec":
        if agent:
            return rtxctl("exec", "-agent", agent, "-cmd", a.get("cmd", ""))
        return rtxctl("exec", "-cmd", a.get("cmd", ""))
    if name == "rtx_info":
        return rtxctl("info") if not agent else rtxctl("info", "-agent", agent)
    if name == "rtx_read":
        return rtxctl("read", "-path", a.get("path", "")) if not agent else rtxctl("read", "-agent", agent, "-path", a.get("path", ""))
    if name == "rtx_list":
        return rtxctl("list", "-path", a.get("path", "")) if not agent else rtxctl("list", "-agent", agent, "-path", a.get("path", ""))
    if name == "rtx_write":
        if a.get("content") is not None:
            return rtxctl("write", "-path", a.get("path", ""), a.get("content"))
        if a.get("file"):
            return rtxctl("write", "-path", a.get("path", ""), "-file", a.get("file"))
        return "rtx_write 需要 content 或 file", 1
    if name == "rtx_upload":
        return rtxctl("upload", "-path", a.get("path", ""), "-file", a.get("file", ""))
    if name == "rtx_download":
        if a.get("out"):
            return rtxctl("download", "-path", a.get("path", ""), "-out", a.get("out"))
        return rtxctl("download", "-path", a.get("path", ""))
    return f"未知工具: {name}", 1


# ---------------- MCP stdio 主循环 ----------------

def send(msg):
    sys.stdout.write(json.dumps(msg) + "\n")
    sys.stdout.flush()


def main():
    server_info = {"name": "rtx-mcp", "version": "1.1.0"}
    initialized = False
    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        try:
            msg = json.loads(line)
        except json.JSONDecodeError:
            continue
        mid = msg.get("id")
        method = msg.get("method")

        if method == "initialize":
            initialized = True
            send({"jsonrpc": "2.0", "id": mid, "result": {
                "protocolVersion": msg.get("params", {}).get("protocolVersion", "2024-11-05"),
                "capabilities": {"tools": {}},
                "serverInfo": server_info,
            }})
        elif method == "notifications/initialized":
            pass
        elif method == "tools/list":
            send({"jsonrpc": "2.0", "id": mid, "result": {"tools": TOOLS}})
        elif method == "tools/call":
            params = msg.get("params", {})
            name = params.get("name", "")
            args = params.get("arguments", {})
            text, rc = handle_tool(name, args)
            send({"jsonrpc": "2.0", "id": mid, "result": {
                "content": [{"type": "text", "text": text}],
                "isError": rc != 0,
            }})
        elif method == "ping":
            send({"jsonrpc": "2.0", "id": mid, "result": {}})
        else:
            if mid is not None:
                send({"jsonrpc": "2.0", "id": mid, "error": {"code": -32601, "message": f"未知方法 {method}"}})


if __name__ == "__main__":
    main()