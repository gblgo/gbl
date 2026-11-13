// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

//go:build !windows

package gbl

import "os"

// enableConsoleVT is a no-op off Windows: POSIX terminals interpret ANSI escape
// sequences natively, so a TTY is always color-capable.
func enableConsoleVT(*os.File) bool { return true }
