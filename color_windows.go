// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

//go:build windows

package gbl

import (
	"os"
	"syscall"
	"unsafe"
)

// enableVirtualTerminalProcessing is the console mode flag that makes Windows
// interpret ANSI escape sequences instead of printing them literally.
const enableVirtualTerminalProcessing = 0x0004

var (
	kernel32           = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleMode = kernel32.NewProc("GetConsoleMode")
	procSetConsoleMode = kernel32.NewProc("SetConsoleMode")
)

// enableConsoleVT turns on ANSI processing for a Windows console handle and
// reports whether the console can render color. A redirected handle is not a
// console, so GetConsoleMode fails and color stays off; an older Windows that
// rejects the flag likewise falls back to plain output rather than emitting raw
// escape codes a console would print literally.
func enableConsoleVT(f *os.File) bool {
	handle := syscall.Handle(f.Fd())

	var mode uint32
	r, _, _ := procGetConsoleMode.Call(uintptr(handle), uintptr(unsafe.Pointer(&mode)))
	if r == 0 {
		return false
	}
	if mode&enableVirtualTerminalProcessing != 0 {
		return true
	}
	r, _, _ = procSetConsoleMode.Call(uintptr(handle), uintptr(mode|enableVirtualTerminalProcessing))
	return r != 0
}
