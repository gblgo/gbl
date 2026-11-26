// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

//go:build darwin

package gbl

import (
	"os"
	"syscall"
	"unsafe"
)

// detectTerminal reports whether f is a terminal by querying its terminal settings.
// The ioctl fails with ENOTTY for regular files, pipes and devices such as
// /dev/null, unlike a ModeCharDevice check.
func detectTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	var termState syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), uintptr(syscall.TIOCGETA),
		uintptr(unsafe.Pointer(&termState)))
	return errno == 0
}
