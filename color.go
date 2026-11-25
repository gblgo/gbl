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

// Constants used only by this file: the ANSI SGR sequences that tint the level
// label on a terminal and the bound for the label scan.
const (
	// The SGR sequences are only ever emitted when the console writer is a
	// real TTY, so redirected output and log files never see them.
	// restores default terminal attributes after a tint
	ansiReset = "\x1b[0m"
	// tint of ERROR labels
	ansiRed = "\x1b[31m"
	// tint of INFO labels
	ansiGreen = "\x1b[32m"
	// tint of WARN labels
	ansiYellow = "\x1b[33m"

	// maxLevelScanTokens bounds the level-label scan. logLine always puts the
	// label first, and the standard logger optional header precedes it with
	// at most date, time and file:line tokens. Stopping early also keeps an
	// incidental all-caps word in a message body from ever reaching the tint
	// lookup.
	maxLevelScanTokens = 4
)

// levelColor maps a log level label to its terminal tint: ordinary info in
// green, warnings in yellow, errors in red.
var levelColor = map[string]string{
	"INFO":  ansiGreen,
	"WARN":  ansiYellow,
	"ERROR": ansiRed,
}

// findLevel locates the level label: the first whitespace-delimited token that
// names a known level. It returns the label byte range and its tint, and
// reports false when the line carries none — a label-less line is a foreign
// write (the startup banner, for example) and must pass through untouched.
func findLevel(p []byte) (start, end int, ansi string, found bool) {
	for offset, scanned := 0, 0; offset < len(p) && scanned < maxLevelScanTokens; scanned++ {
		stop := offset
		for stop < len(p) && p[stop] != ' ' && p[stop] != '\n' {
			stop++
		}
		if color, ok := levelColor[string(p[offset:stop])]; ok {
			return offset, stop, color, true
		}
		if stop == len(p) {
			break
		}
		offset = stop + 1
	}
	return 0, 0, "", false
}

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
	// w is the wrapped console destination
	w io.Writer
	// enabled records whether ANSI coloring is active
	enabled bool
}

// Write tints the level label when enabled and forwards the line to the
// underlying console. It reports the caller original byte count so log.Logger
// sees the whole line consumed; the extra escape bytes belong to this writer.
// business.
func (c colorWriter) Write(p []byte) (int, error) {
	if !c.enabled {
		return c.w.Write(p)
	}

	levelStart, levelEnd, color, found := findLevel(p)
	if !found {
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
// file or a pipe. The Stat-based check is the standard library own idiom, so
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

// noColorRequested reports whether the de facto NO_COLOR convention asks for
// plain output: the variable is set and non-empty, whatever it holds.
func noColorRequested() bool {
	return os.Getenv("NO_COLOR") != empty
}

// consoleWriter returns f wrapped in a colorWriter when f is a terminal that can
// render ANSI, and f itself otherwise. On Windows the VT-processing mode must be
// switched on first; if that fails the writer stays plain rather than leaking
// raw escape codes into a console that would not interpret them. It is the
// framework own development aid for the default console, not an offered entry
// point: users who integrate a logging component take that library coloring.
func consoleWriter(f *os.File) io.Writer {
	if noColorRequested() || !isTerminal(f) {
		return f
	}
	return colorWriter{w: f, enabled: enableConsoleVT(f)}
}
