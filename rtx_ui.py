#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
rtx_ui.py — rtx 远程 agent 连接选择器（TUI，模仿 pi-remote "Saved remote connections" 样式）
用法: rtxctl connect   （内部调本脚本）
界面: 列出在线/已保存 agent → ↑↓/j/k 导航 → Enter 连接(进入该 agent 执行环境) → q/Esc 退出
"""

import curses
import subprocess
import sys


def get_agents():
    """调 rtxctl ls 拿 agent 列表 → [(name, detail, online)]"""
    try:
        p = subprocess.run(["rtxctl", "ls"], capture_output=True, text=True, timeout=30)
        out = p.stdout.strip()
    except Exception as e:
        return [], f"rtxctl ls 失败: {e}"
    agents = []
    for line in out.splitlines():
        line = line.strip()
        if not line or line.startswith("(") or "no agents" in line:
            continue
        parts = line.split()
        if not parts:
            continue
        name = parts[0]
        online = "online=true" in line or line.strip().endswith("online=true")
        # 去掉 online=true 尾巴做 detail
        detail = line.replace(" online=true", "").strip()
        agents.append((name, detail, online))
    return agents, ""


def run(stdscr):
    curses.curs_set(0)
    agents, err = get_agents()
    if err:
        stdscr.addstr(0, 0, err)
        stdscr.refresh()
        stdscr.getch()
        return None
    if not agents:
        stdscr.addstr(0, 0, "(no agents online — 先部署 agent 反连 server)")
        stdscr.addstr(1, 0, "按任意键退出")
        stdscr.refresh()
        stdscr.getch()
        return None

    # 颜色
    curses.start_color()
    curses.use_default_colors()
    curses.init_pair(1, curses.COLOR_CYAN, -1)    # 标题
    curses.init_pair(2, curses.COLOR_GREEN, -1)   # online
    curses.init_pair(3, curses.COLOR_RED, -1)     # offline
    curses.init_pair(4, curses.COLOR_BLACK, curses.COLOR_WHITE)  # 选中行
    curses.init_pair(5, curses.COLOR_YELLOW, -1)  # 提示

    idx = 0
    h, w = stdscr.getmaxyx()
    title = "rtx remote agents"
    while True:
        stdscr.erase()
        # 标题
        try:
            stdscr.addstr(0, 1, title, curses.A_BOLD | curses.color_pair(1))
            stdscr.addstr(0, len(title) + 3, "(Saved remote connections)", curses.color_pair(5))
        except curses.error:
            pass
        # 列表（从第 2 行开始，选中行高亮）
        max_rows = h - 4
        start = max(0, idx - max_rows + 1)
        for i in range(start, min(len(agents), start + max_rows)):
            name, detail, online = agents[i]
            y = 2 + (i - start)
            prefix = "> " if i == idx else "  "
            line = f"{prefix}{name:<22} {detail}"
            try:
                if i == idx:
                    stdscr.addstr(y, 1, line[:w - 2], curses.color_pair(4))
                else:
                    stdscr.addstr(y, 1, line[:w - 2])
                    # 状态列（行尾显示 online/offline 颜色）
                    status = "online" if online else "offline"
                    sx = min(len(line), w - 12)
                    color = curses.color_pair(2) if online else curses.color_pair(3)
                    stdscr.addstr(y, max(sx, 25), status, color)
            except curses.error:
                pass
        # 底部提示
        hint = "j/k or ↑/↓ navigate · Enter connect · q/Esc quit"
        try:
            stdscr.addstr(h - 1, 1, hint, curses.color_pair(5))
        except curses.error:
            pass
        stdscr.refresh()

        key = stdscr.getch()
        if key in (ord("q"), 27):  # q / Esc
            return None
        if key in (ord("j"), curses.KEY_DOWN):
            idx = min(idx + 1, len(agents) - 1)
        elif key in (ord("k"), curses.KEY_UP):
            idx = max(idx - 1, 0)
        elif key in (10, 13, ord(" ")):  # Enter/Space
            try:
                stdscr.erase(); stdscr.refresh()
            except curses.error:
                pass
            return agents[idx][0]


def main():
    import re
    import os
    chosen = curses.wrapper(run)
    if chosen:
        clean = re.sub(r"[^\w.\-]", "", str(chosen))
        if clean:
            # 结果写临时文件（curses 占着终端 stdout，调用方从文件读）
            d = os.path.expanduser("~/.claude/rtx")
            os.makedirs(d, exist_ok=True)
            with open(os.path.join(d, ".ui_choice"), "w") as f:
                f.write(clean)
            try:
                sys.stdout.write(clean + "\n")
                sys.stdout.flush()
            except Exception:
                pass


if __name__ == "__main__":
    main()
