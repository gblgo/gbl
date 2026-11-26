// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

//go:build darwin

package gbl

import (
	"os"
	"testing"
)

// These tests exercise the safe ENOTTY/EBADF paths without allocating a real
// pseudo-terminal. The successful TIOCGETA branch requires an interactive PTY
// and remains environment-dependent when this package is run on a macOS host.
func TestTerminalDarwinEdgeDevNull(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	file, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if isTerminal(file) {
		t.Fatal("/dev/null was classified as a terminal")
	}
	if got := consoleWriter(file); got != file {
		t.Fatalf("consoleWriter(/dev/null) = %T, want plain *os.File", got)
	}
}

func TestTerminalDarwinEdgeRegularFile(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	file, err := os.CreateTemp(t.TempDir(), "gbl-terminal-*")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString("ordinary file"); err != nil {
		t.Fatal(err)
	}
	if isTerminal(file) {
		t.Fatal("regular file was classified as a terminal")
	}
	if got := consoleWriter(file); got != file {
		t.Fatalf("consoleWriter(regular file) = %T, want plain *os.File", got)
	}
}

func TestTerminalDarwinEdgeClosedFile(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	file, err := os.CreateTemp(t.TempDir(), "gbl-closed-terminal-*")
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if isTerminal(file) {
		t.Fatal("closed file was classified as a terminal")
	}
	if got := consoleWriter(file); got != file {
		t.Fatalf("consoleWriter(closed file) = %T, want plain *os.File", got)
	}
}

func TestTerminalDarwinEdgeNilFile(t *testing.T) {
	if isTerminal(nil) {
		t.Fatal("isTerminal(nil) = true")
	}
}
