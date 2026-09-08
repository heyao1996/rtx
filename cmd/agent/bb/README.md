# busybox.exe

Windows 版 busybox（busybox-w32, 32-bit x86 static），用于让 Windows agent 具备类 Unix 工具集（ls/cat/grep/sed/wget 等），AI 以 sh 语法执行命令更高效。

- 来源: https://frippery.org/files/busybox/busybox.exe (busybox-w32)
- 许可证: GPLv2（busybox 项目，源码见 https://busybox.net 或 https://github.com/rmyorston/busybox-w32）
- agent 编译时经 go:embed 打包，运行期释出到临时目录调用
