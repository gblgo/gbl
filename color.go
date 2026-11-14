// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"bytes"
	"io"
	"os"
	"sync"
)

// ANSI SGR sequences used to tint the level label on a terminal. They are only
// ever emitted when the console writer is a real TTY, so redirected output and
// log files never see them.
const (
	ansiReset  = "\x1b[0m"
	ansiRed    = "\x1b[31m"
	ansiGreen  = "\x1b[32m"
	ansiYellow = "\x1b[33m"
)

// levelColor maps a log level label to its terminal tint: ordinary info in
// green, warnings in yellow, errors in red.
var levelColor = map[string]string{
	"INFO":  ansiGreen,
	"WARN":  ansiYellow,
	"ERROR": ansiRed,
}

// logMarker is the fixed prefix logLine puts before the level label. colorWriter
// locates the level by finding it, so tinting never has to reparse the rest of
// the line.
var logMarker = []byte("gbl: ")

// colorBufPool recycles the scratch buffer colorWriter assembles a tinted line
// in, so an enabled console adds no per-line allocation once warm.
var colorBufPool = sync.Pool{
	New: func() any { return new(bytes.Buffer) },
}

// colorWriter wraps a console sink and injects ANSI tinting around the level
// label of each log line. When enabled is false — a redirected writer, a pipe,
// CI, or a Windows console that could not turn on VT processing — it is a pure
// pass-through. The same type therefore serves every console path at no cost
// beyond one bool check.
//
// Lines that carry no level label (the startup banner, written verbatim) are
// always passed through untouched.
type colorWriter struct {
	w       io.Writer
	enabled bool
}

// Write tints the level label when enabled and forwards the line to the
// underlying console. It reports the caller's original byte count so log.Logger
// sees the whole line consumed; the extra escape bytes are this writer's own
// business.
func (c colorWriter) Write(p []byte) (int, error) {
	if !c.enabled {
		return c.w.Write(p)
	}

	idx := bytes.Index(p, logMarker)
	if idx < 0 {
		return c.w.Write(p)
	}
	levelStart := idx + len(logMarker)
	rel := bytes.IndexByte(p[levelStart:], ' ')
	if rel < 0 {
		return c.w.Write(p)
	}
	levelEnd := levelStart + rel

	color, ok := levelColor[string(p[levelStart:levelEnd])]
	if !ok {
		return c.w.Write(p)
	}

	buf := colorBufPool.Get().(*bytes.Buffer)
	buf.Reset()
	buf.Write(p[:levelStart])
	buf.WriteString(color)
	buf.Write(p[levelStart:levelEnd])
	buf.WriteString(ansiReset)
	buf.Write(p[levelEnd:])
	_, err := c.w.Write(buf.Bytes())
	colorBufPool.Put(buf)
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

// isTerminal reports whether f is an interactive console rather than a regular
// file or a pipe. The Stat-based check is the standard library's own idiom, so
// it stays dependency-free across platforms.
func isTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

// consoleWriter returns f wrapped in a colorWriter when f is a terminal that can
// render ANSI, and f itself otherwise. On Windows the VT-processing mode must be
// switched on first; if that fails the writer stays plain rather than leaking
// raw escape codes into a console that would not interpret them.
func consoleWriter(f *os.File) io.Writer {
	if !isTerminal(f) {
		return f
	}
	return colorWriter{w: f, enabled: enableConsoleVT(f)}
}
