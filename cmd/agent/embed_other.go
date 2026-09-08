//go:build !windows

package main

// 非 Windows 平台无捆绑 busybox（系统自带 bash/coreutils）
var busyBoxExe []byte
