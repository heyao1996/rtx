//go:build windows

package main

import _ "embed"

//go:embed bb/busybox.exe
var busyBoxExe []byte
