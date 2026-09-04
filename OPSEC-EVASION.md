# rtx OPSEC & Evasion 加固手册

> 来源：PEN-200 (2024.11) Ch.14 AV Evasion / Ch.19 DPI Tunneling + PEN-300 OSEP (2025.4) 实战学习。
> 目标：让 rtx agent 在真实目标环境（有 AV/EDR/DPI）落地与通信更隐蔽稳定。

## 一、检测原理（学到的）

**AV/EDR 检测方法**（PEN-200 14.1.3）：
| 检测 | 机制 | 对策 |
|---|---|---|
| Signature-based | 文件 hash / YARA 字节序列 | 消除特征串、每次构建随机化 |
| Heuristic | 可疑结构/加壳识别 | 控制熵、避免已知壳特征 |
| Behavioral | 运行行为（进程/API/网络） | 静默、低噪、合法通道落地 |
| Machine Learning | 云端未知文件分类 | 特征工程对抗 |

**规避分层**（PEN-200 14.2 + OSEP）：
- **On-Disk**：packer（加壳）→ crypter（磁盘只留加密代码 + 解密 stub 内存还原——PEN-200 称加密是**最有效 on-disk 规避**）
- **In-Memory**：shellcode runner（VirtualAlloc→WriteProcessMemory→CreateRemoteThread，OSEP payload_x64.cs 即 C# P/Invoke 版）、AMSI/ETW patch（OSEP 反射绕过示例）
- **行为层**：避免服务创建（真实 EDR 教训：psexec 服务创建当场告警）、合法通道落地

## 二、rtx 现状特征（已审计 bin/agent-linux-amd64）

| 特征 | 危害 | 位置 |
|---|---|---|
| `rtx/cmd/agent`、`rtx/internal/proto` | Go module 路径，YARA 可精确匹配 | 二进制 7 处 |
| `/bin/sh` | shell 路径特征 | 1 处 |
| `unknown task` | 项目专属错误串 | 1 处 |
| `socks5://` | flag 帮助文本 | 4 处 |

## 三、加固优先级（按收益/成本）

| 级别 | 项 | 做法 | 状态 |
|---|---|---|---|
| **P0-1** | module 改名 | `go.mod` module `rtx` → `coreutil`，import 全改 | ✅ 已完成 |
| **P0-2** | 关键串混淆 | `/bin/sh`、`unknown task` 等 5 字节 key XOR 运行时解密 + flag 帮助静默 | ✅ 已完成 |
| **P1** | TLS 信道 | server `-tls`(自动自签+打印指纹) / agent `-tls -pin <fp>` 证书 pin 校验 | ✅ 已完成 |
| P2 | 行为低噪 | 默认进程名中性、启动延迟选项、Windows 落地走计划任务/RDP（勿服务创建） | 部分(静默/jitter 已有) |
| P3 | 完整 crypter/loader | 仿 OSEP loader：agent 主体加密 + stub 加载（大改，暂缓） | 不做 |

## 四、实施要点

> v1.1 已实施 P0-1/P0-2/P1；P2/P3 见下。TLS 用法：server 加 `-tls`（自动生成 `rtx-server.crt/key` 并打印 `pin(fp)`），agent 加 `-tls -pin <fp>` 回连；走已有加密隧道时 TLS 作纵深，直连公网必开。

### P0-1 module 改名
```bash
cd /Users/z1m3s/tools/rtx
sed -i 's|module rtx|module coreutil|' go.mod
grep -rl '"rtx/' cmd internal | xargs sed -i 's|"rtx/|"coreutil/|g'
# 验证: strings bin/agent | grep rtx → 应只剩运行时无关
```

### P0-2 关键串混淆（轻量）
- 维护一张 `map[明文串]XOR字节` 表（构建期生成），运行时按需解密
- 覆盖：`/bin/sh`、`-c`、`unknown task`、`socks5`（flag 帮助可删/缩写）

### P1 TLS
- server：`-tls` 启动自签证书（或 -cert/-key）；agent：`-tls` 内嵌 CA 校验
- 复用现有 4 字节长度 + JSON 帧，外包 TLS 层即可
- 注意：走已有隧道（chisel/ssh -R）时隧道已加密，TLS 可作纵深；直连公网时必开

## 五、真实环境教训（已实测）

- Windows EDR 目标：psexec 服务创建 / wmiexec SMB 执行 cmd **当场告警**（172.20.205.175 两次）；纯 SMB 文件上传低噪 → agent 落地优先合法通道
- Apple Silicon Windows VM 是 ARM64，需对应架构产物
- 交叉编译必须命令前缀形式（`GOOS=.. GOARCH=.. go build`），`file` 验证格式

## 六、验证方法

- 静态：`strings bin/agent | grep -E 'rtx|/bin/sh|unknown task'` → 应 0 命中
- 在线查杀（可选）：上传 VirusTotal（注意：上传即样本公开，谨慎）
- 行为：目标机器上任务管理器/进程树观察，确认无高噪动作
