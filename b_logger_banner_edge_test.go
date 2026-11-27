// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

//go:build windows

package gbl

import (
	"bytes"
	"errors"
	"log"
	"os"
	"strings"
	"syscall"
	"testing"
	"unsafe"
)

type loggerBannerEdgeErrorWriter struct {
	err          error
	calls        int
	bytesWritten int
}

func (w *loggerBannerEdgeErrorWriter) Write(p []byte) (int, error) {
	w.calls++
	w.bytesWritten += len(p)
	return 0, w.err
}

func TestLoggerBannerEdgeSinkErrorsAreIgnored(t *testing.T) {
	sink := &loggerBannerEdgeErrorWriter{err: errors.New("sink closed")}
	logger := stdLogger{l: log.New(sink, "", 0)}
	logger.Info("info", "attempt", 1)
	logger.Warn("warn", "retry", true)
	logger.Error("error", "reason", "closed")
	logger.Banner("banner\nblock")
	if sink.calls != 4 {
		t.Fatalf("sink write calls = %d, want 4", sink.calls)
	}
	if sink.bytesWritten == 0 {
		t.Fatal("sink received no log bytes")
	}
}

func TestLoggerBannerEdgeEmptyEngineRender(t *testing.T) {
	e := newEngine()
	t.Cleanup(e.cancelStreams)
	block := e.renderBanner(defaultConfig(), nil)
	for _, want := range []string{bannerArt, "listening", "http://?", "routes", "0"} {
		if !strings.Contains(block, want) {
			t.Errorf("rendered banner does not contain %q:\n%s", want, block)
		}
	}
	if !strings.HasSuffix(block, "\n") {
		t.Errorf("rendered banner lost its trailing newline: %q", block)
	}
}

func TestLoggerBannerEdgeWindowsVTInvalidAndClosedHandles(t *testing.T) {
	t.Run("invalid handle", func(t *testing.T) {
		// A NULL handle is invalid but still produces a non-nil *os.File;
		// os.NewFile(^uintptr(0), ...) would return nil instead.
		file := os.NewFile(0, "invalid-console-handle")
		if file == nil {
			t.Fatal("os.NewFile returned nil")
		}
		defer file.Close()
		if isTerminal(file) {
			t.Fatal("invalid handle was classified as a terminal")
		}
		if enableConsoleVT(file) {
			t.Fatal("enableConsoleVT(invalid handle) = true, want GetConsoleMode failure")
		}
	})

	t.Run("closed handle", func(t *testing.T) {
		file, err := os.CreateTemp(t.TempDir(), "gbl-closed-console-*")
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		if isTerminal(file) {
			t.Fatal("closed handle was classified as a terminal")
		}
		if enableConsoleVT(file) {
			t.Fatal("enableConsoleVT(closed handle) = true, want GetConsoleMode failure")
		}
	})
}

func loggerBannerEdgeConsoleMode(file *os.File) (uint32, error) {
	var mode uint32
	handle := syscall.Handle(file.Fd())
	result, _, err := procGetConsoleMode.Call(uintptr(handle), uintptr(unsafe.Pointer(&mode)))
	if result == 0 {
		return 0, err
	}
	return mode, nil
}

func loggerBannerEdgeSetConsoleMode(file *os.File, mode uint32) error {
	handle := syscall.Handle(file.Fd())
	result, _, err := procSetConsoleMode.Call(uintptr(handle), uintptr(mode))
	if result == 0 {
		return err
	}
	return nil
}

func TestLoggerBannerEdgeWindowsVTSetFailureWithReadOnlyConsole(t *testing.T) {
	console, err := os.OpenFile("CONOUT$", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("interactive Windows console unavailable: %v", err)
	}
	defer console.Close()
	if !isTerminal(console) {
		t.Skip("CONOUT$ is not attached to an interactive console")
	}
	original, err := loggerBannerEdgeConsoleMode(console)
	if err != nil {
		t.Skipf("cannot query console mode: %v", err)
	}
	defer func() {
		if err := loggerBannerEdgeSetConsoleMode(console, original); err != nil {
			t.Errorf("restore console mode: %v", err)
		}
	}()

	if original&enableVirtualTerminalProcessing != 0 {
		if err := loggerBannerEdgeSetConsoleMode(console, original&^enableVirtualTerminalProcessing); err != nil {
			t.Skipf("console does not permit disabling VT processing: %v", err)
		}
	}
	readOnly, err := os.OpenFile("CONOUT$", os.O_RDONLY, 0)
	if err != nil {
		t.Skipf("cannot open read-only console handle: %v", err)
	}
	defer readOnly.Close()
	if !isTerminal(readOnly) {
		t.Skip("read-only CONOUT$ handle is not a console")
	}
	// GetConsoleMode succeeds with read access, but SetConsoleMode requires
	// write access. This isolates the SetConsoleMode failure return.
	if enableConsoleVT(readOnly) {
		t.Skip("this console accepted SetConsoleMode on a read-only handle; cannot force the failure without changing the environment")
	}
	t.Log("GetConsoleMode succeeded and SetConsoleMode failed on a read-only console handle")
}

func TestLoggerBannerEdgeWindowsVTRealConsoleSuccess(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	console, err := os.OpenFile("CONOUT$", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("interactive Windows console unavailable: %v", err)
	}
	defer console.Close()
	if !isTerminal(console) {
		t.Skip("CONOUT$ is not attached to an interactive console")
	}
	original, err := loggerBannerEdgeConsoleMode(console)
	if err != nil {
		t.Skipf("cannot query console mode: %v", err)
	}
	defer func() {
		if err := loggerBannerEdgeSetConsoleMode(console, original); err != nil {
			t.Errorf("restore console mode: %v", err)
		}
	}()

	if original&enableVirtualTerminalProcessing != 0 {
		if err := loggerBannerEdgeSetConsoleMode(console, original&^enableVirtualTerminalProcessing); err != nil {
			t.Skipf("console does not permit disabling VT processing: %v", err)
		}
	}
	if !enableConsoleVT(console) {
		t.Skip("console does not permit enabling VT processing")
	}
	mode, err := loggerBannerEdgeConsoleMode(console)
	if err != nil {
		t.Fatalf("query enabled console mode: %v", err)
	}
	if mode&enableVirtualTerminalProcessing == 0 {
		t.Fatal("VT processing was not enabled")
	}
	// Calling again with the flag already set covers the no-Set success path.
	if !enableConsoleVT(console) {
		t.Fatal("enableConsoleVT() = false when VT processing was already enabled")
	}
	writer, ok := consoleWriter(console).(colorWriter)
	if !ok || !writer.enabled {
		t.Fatalf("consoleWriter(console) = %#v, want enabled colorWriter", writer)
	}

	var out bytes.Buffer
	colored := colorWriter{w: &out, enabled: writer.enabled}
	if _, err := colored.Write([]byte("INFO real console\n")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), ansiGreen+"INFO"+ansiReset) {
		t.Fatalf("real-console color writer output = %q", out.String())
	}
	t.Log("interactive Windows console exercised GetConsoleMode, SetConsoleMode, and enabled console coloring")
}
