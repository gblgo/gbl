// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"fmt"
	"log"
	"os"
	"strings"
)

// Logger is the minimal, zero-dependency logging contract of gbl. The framework
// routes every message it emits internally — a recovered panic, a validation
// failure, an access-log line — through the engine's Logger. It never binds
// hard to a specific package. So the destination, the format and (where the
// implementation supports them) the structured fields are all the
// application's choice.
//
// Each method takes trailing key/value pairs: msg describes the event, then
// alternating keys and their values. A structured backend such as zap or slog
// can consume them directly, while the default keeps a plain-text shape.
// Implement the interface and install it with SetLogger; a one-line adapter is
// enough. File logging is one such integration: the framework itself only
// prints to the console, and an application that wants files installs a Logger
// (or wraps AccessLog in middleware) that writes there.
type Logger interface {
	Info(msg string, keysAndValues ...any)
	Warn(msg string, keysAndValues ...any)
	Error(msg string, keysAndValues ...any)
}

// BannerLogger is an optional companion of Logger. A Logger that implements it
// is handed the startup banner as a pre-rendered, multi-line block and is free
// to emit it verbatim — without a level label or timestamp crowding the ASCII
// art. printBanner prefers Banner when present and otherwise falls back to
// Info, so a plain Logger implementation still receives the banner, just as an
// ordinary record.
type BannerLogger interface {
	Banner(block string)
}

// stdLogger is the default Logger. It writes through the standard library's log
// package, so output keeps the familiar date/time prefix and can be redirected
// globally with log.SetOutput. A nil l means "use the package-level logger".
//
// The package-level output is upgraded to a level-tinting writer at init while it
// still points at os.Stderr, so the default console is colored on a real terminal
// without giving up log.SetOutput redirection.
type stdLogger struct {
	l *log.Logger
}

// newStdLogger builds the default logger backed by the log package's global
// logger.
func newStdLogger() stdLogger { return stdLogger{} }

// init upgrades the standard library's default log output to a level-tinting
// writer, but only while it still points at os.Stderr — that is, the program has
// not redirected logging itself. The wrapper is a pure pass-through unless stderr
// is a real terminal, so redirected output, pipes and tests are unaffected, and
// calling log.SetOutput later replaces the wrapper outright.
func init() {
	if log.Writer() == os.Stderr {
		log.SetOutput(consoleWriter(os.Stderr))
	}
}

// NewStdLogger adapts any *log.Logger to the gbl Logger interface, so callers
// who already configure a standard logger (a specific writer, flags, prefix) can
// hand it to gbl without writing an implementation:
//
//	gbl.SetLogger(gbl.NewStdLogger(myLogger))
//
// This is also the shortest path to file logging: point a *log.Logger at an
// open file (or an io.MultiWriter) and install it.
func NewStdLogger(l *log.Logger) Logger { return stdLogger{l: l} }

func (s stdLogger) Info(msg string, kv ...any)  { s.emit("INFO", msg, kv) }
func (s stdLogger) Warn(msg string, kv ...any)  { s.emit("WARN", msg, kv) }
func (s stdLogger) Error(msg string, kv ...any) { s.emit("ERROR", msg, kv) }

// Banner writes the startup block straight to the sink, bypassing logLine so the
// art keeps its alignment and no date/level label is glued to its first line.
// Only the underlying writer is reused, so SetOutput and a NewStdLogger target
// are still honored.
func (s stdLogger) Banner(block string) {
	w := log.Writer()
	if s.l != nil {
		w = s.l.Writer()
	}
	_, _ = fmt.Fprint(w, "\n"+block+"\n")
}

// emit renders "gbl: LEVEL msg key=value ..." and writes it at the caller's
// depth. All levels share the standard logger because log itself is
// level-free; the label preserves the distinction for readers.
func (s stdLogger) emit(level, msg string, kv []any) {
	line := logLine(level, msg, kv)
	if s.l != nil {
		_ = s.l.Output(3, line)
		return
	}
	_ = log.Output(3, line)
}

// logLine assembles the plain-text form. Pairs print as key=value; a dangling
// trailing value (odd count) is appended bare so no information is dropped.
func logLine(level, msg string, kv []any) string {
	var b strings.Builder
	b.WriteString("gbl: ")
	b.WriteString(level)
	b.WriteByte(' ')
	b.WriteString(msg)
	for i := 0; i+1 < len(kv); i += 2 {
		b.WriteByte(' ')
		b.WriteString(fmt.Sprint(kv[i]))
		b.WriteByte('=')
		b.WriteString(fmt.Sprint(kv[i+1]))
	}
	if len(kv)%2 == 1 {
		b.WriteByte(' ')
		b.WriteString(fmt.Sprint(kv[len(kv)-1]))
	}
	return b.String()
}

// SetLogger installs a Logger on the default engine. Passing nil is ignored, so
// the framework always keeps a working logger.
func SetLogger(l Logger) { defaultEngine.SetLogger(l) }

// SetLogger installs a Logger on this engine. It is safe to call before or after
// routes and middleware are registered; passing nil leaves the current logger
// untouched.
func (e *engine) SetLogger(l Logger) *engine {
	if l != nil {
		e.logger = l
	}
	return e
}

// failStartup records a startup, configuration or registration failure through
// the engine Logger and then panics with the very same value. Logging before the
// process dies means a deployment that never reaches serving still leaves a
// durable, sink-routed reason instead of only a bare stderr trace; the panic
// value is unchanged, so any recover-based caller observes exactly what it would
// have without the logging. It is deliberately not used for the request-time
// responseAbort control flow, which is a normal answer rather than a failure.
func (e *engine) failStartup(reason any) {
	e.logger.Error("startup aborted", "reason", reason)
	panic(reason)
}
