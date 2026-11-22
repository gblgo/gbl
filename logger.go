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
// failure — through the engine's Logger. It never binds
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
// that writes there.
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

// stdLogger is the default Logger and a deliberate development aid: it prints
// only to the console (with level tinting on a real terminal) and offers no
// files, rotation or routing — an application that integrates a real logging
// component replaces it through SetLogger.
//
// It writes through a private *log.Logger, not the standard library's
// package-level one, which keeps framework and application isolated in both
// directions: the application's log.SetOutput and log.SetFlags calls never
// move or restyle framework lines, and the framework never reaches into the
// application's own log.Println output. The private logger sits on a
// level-tinting writer over os.Stderr, so framework lines keep the familiar
// date/time prefix and are colored without polluting the global logger.
// Sending framework logs somewhere else has exactly one path: install a
// Logger with SetLogger.
type stdLogger struct {
	l *log.Logger
}

// newStdLogger builds the default logger. The tinting decision is made once,
// here: consoleWriter hands back the bare os.Stderr when the destination is
// not a color-capable terminal (or NO_COLOR is set), so pipes, redirects and
// CI carry no wrapper layer and no escape codes at all.
func newStdLogger() stdLogger {
	return stdLogger{l: log.New(consoleWriter(os.Stderr), "", log.LstdFlags)}
}

func (s stdLogger) Info(msg string, kv ...any)  { s.emit("INFO", msg, kv) }
func (s stdLogger) Warn(msg string, kv ...any)  { s.emit("WARN", msg, kv) }
func (s stdLogger) Error(msg string, kv ...any) { s.emit("ERROR", msg, kv) }

// Banner writes the startup block straight to the sink's underlying writer,
// bypassing the private logger so the art keeps its alignment and no
// date/level label is glued to its first line. The tinting writer is still
// underneath and passes the block through untouched: it carries no level
// word for the scanner to find.
func (s stdLogger) Banner(block string) {
	_, _ = fmt.Fprint(s.l.Writer(), "\n"+block+"\n")
}

// emit renders "LEVEL msg key=value ..." and writes it through the private
// logger at a caller depth that resolves file/line information (when the
// flags ask for it) to the business call site. All levels share the one
// logger because log itself is level-free; the label preserves the
// distinction for readers.
func (s stdLogger) emit(level, msg string, kv []any) {
	_ = s.l.Output(3, logLine(level, msg, kv))
}

// logLine assembles the plain-text form. Pairs print as key=value; a dangling
// trailing value (odd count) is appended bare so no information is dropped.
func logLine(level, msg string, kv []any) string {
	var b strings.Builder
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
// the framework always keeps a working logger. Until it is called the default
// engine logs through the console helper described above: that console logger
// is a development aid, and integrating a real logging component simply means
// plugging its adapter in here.
func SetLogger(l Logger) { defaultEngine.setLogger(l) }

// Info logs at info level through the Logger installed on the default engine.
// The three package-level log functions are the ergonomic entry for business
// code: once a logger is injected with SetLogger it is the framework's sink,
// and calling it through the framework's own API is the intended use — no need
// to keep or fetch a reference. msg describes the event, followed by
// alternating key/value pairs (see Logger).
func Info(msg string, keysAndValues ...any) { defaultEngine.logger.Info(msg, keysAndValues...) }

// Warn logs at warn level through the Logger installed on the default engine.
func Warn(msg string, keysAndValues ...any) { defaultEngine.logger.Warn(msg, keysAndValues...) }

// Error logs at error level through the Logger installed on the default engine.
func Error(msg string, keysAndValues ...any) { defaultEngine.logger.Error(msg, keysAndValues...) }

// setLogger installs a Logger on this engine. It is safe to call at any point
// during assembling, before or after routes and middleware are registered;
// passing nil leaves the current logger untouched. Once the engine runs the
// logger is frozen like every other assembling input, and calling this again
// panics.
func (e *engine) setLogger(l Logger) {
	e.assertAssembling("SetLogger")
	if l != nil {
		e.logger = l
	}
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
