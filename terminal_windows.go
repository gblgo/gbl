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

// isTerminal reports whether f is attached to a Windows console. GetConsoleMode
// rejects regular files, pipes and non-console character devices such as NUL.
func isTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	handle := syscall.Handle(f.Fd())
	var mode uint32
	result, _, _ := procGetConsoleMode.Call(uintptr(handle), uintptr(unsafe.Pointer(&mode)))
	return result != 0
}
