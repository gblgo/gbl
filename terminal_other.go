// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

//go:build !windows && !linux && !darwin

package gbl

import "os"

// detectTerminal uses a conservative fallback on platforms without a standard
// library terminal ioctl. Character devices are not necessarily consoles, so
// disabling color is safer than emitting escape sequences to an unknown sink.
func detectTerminal(*os.File) bool { return false }
