// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"fmt"
	"log"
	"os"
	"reflect"
	"strconv"
	"strings"
)

// Logger is the minimal, zero-dependency logging contract of gbl. The framework
// routes every message it emits internally — a recovered panic, a validation
// failure — through the engine Logger. It never binds
// hard to a specific package. So the destination, the format and (where the
// implementation supports them) the structured fields are all the
// application choice.
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
// It writes through a private *log.Logger rather than the standard library
// package-level logger, which keeps framework and application output isolated.
// Application calls to log.SetOutput and log.SetFlags never move or restyle
// framework lines, and the framework never captures application log.Println
// output. The private logger sits on a level-tinting writer over os.Stderr, so
// framework lines keep the familiar date/time prefix without polluting the
// global logger. Sending framework logs somewhere else has exactly one path:
// install a Logger with SetLogger.
type stdLogger struct {
	// l is the private standard-library logger
	l *log.Logger
}

// newStdLogger builds the default logger. The tinting decision is made once,
// here: consoleWriter hands back the bare os.Stderr when the destination is
// not a color-capable terminal (or NO_COLOR is set), so pipes, redirects and
// CI carry no wrapper layer and no escape codes at all.
func newStdLogger() stdLogger {
	return stdLogger{l: log.New(consoleWriter(os.Stderr), "", log.LstdFlags)}
}

// Info logs a routine event at INFO level with optional key-value pairs.
func (s stdLogger) Info(msg string, kv ...any) { s.emit("INFO", msg, kv) }

// Warn logs a noteworthy but recoverable event at WARN level.
func (s stdLogger) Warn(msg string, kv ...any) { s.emit("WARN", msg, kv) }

// Error logs a failure at ERROR level.
func (s stdLogger) Error(msg string, kv ...any) { s.emit("ERROR", msg, kv) }

// Banner writes the startup block straight to the sink underlying writer,
// bypassing the private logger so the art keeps its alignment and no
// date/level label is glued to its first line. The tinting writer is still
// underneath and passes the block through untouched: it carries no level
// word for the scanner to find.
func (s stdLogger) Banner(block string) {
	_, _ = fmt.Fprint(s.l.Writer(), "\n"+block+"\n")
}

// emit renders "LEVEL msg key=value ..." and writes it through the private
// logger. The fixed call depth names the package-level Info/Warn/Error wrapper;
// the logger flags are pinned to date/time, so no file/line is ever printed.
// All levels share the one logger because log itself is level-free; the label
// preserves the distinction for readers.
func (s stdLogger) emit(level, msg string, kv []any) {
	_ = s.l.Output(3, logLine(level, msg, kv))
}

// logLine assembles the plain-text form. Pairs print as key=value; a dangling
// trailing value (odd count) is appended bare so no information is dropped.
func logLine(level, msg string, kv []any) string {
	var b strings.Builder
	b.Grow(len(level) + len(msg) + len(kv)*8)
	b.WriteString(level)
	b.WriteByte(' ')
	b.WriteString(msg)
	for i := 0; i+1 < len(kv); i += 2 {
		b.WriteByte(' ')
		appendLogValue(&b, kv[i])
		b.WriteByte('=')
		appendLogValue(&b, kv[i+1])
	}
	if len(kv)%2 == 1 {
		b.WriteByte(' ')
		appendLogValue(&b, kv[len(kv)-1])
	}
	return b.String()
}

// appendLogValue avoids interface formatting allocations for common scalar
// values and delegates other types to fmt.Sprint, including its typed-nil and
// panic-recovery behavior.
func appendLogValue(b *strings.Builder, value any) {
	var number [64]byte
	switch v := value.(type) {
	case nil:
		b.WriteString("<nil>")
	case string:
		b.WriteString(v)
	case bool:
		b.WriteString(strconv.FormatBool(v))
	case int:
		b.Write(strconv.AppendInt(number[:0], int64(v), 10))
	case int8:
		b.Write(strconv.AppendInt(number[:0], int64(v), 10))
	case int16:
		b.Write(strconv.AppendInt(number[:0], int64(v), 10))
	case int32:
		b.Write(strconv.AppendInt(number[:0], int64(v), 10))
	case int64:
		b.Write(strconv.AppendInt(number[:0], v, 10))
	case uint:
		b.Write(strconv.AppendUint(number[:0], uint64(v), 10))
	case uint8:
		b.Write(strconv.AppendUint(number[:0], uint64(v), 10))
	case uint16:
		b.Write(strconv.AppendUint(number[:0], uint64(v), 10))
	case uint32:
		b.Write(strconv.AppendUint(number[:0], uint64(v), 10))
	case uint64:
		b.Write(strconv.AppendUint(number[:0], v, 10))
	case uintptr:
		b.Write(strconv.AppendUint(number[:0], uint64(v), 10))
	case float32:
		b.Write(strconv.AppendFloat(number[:0], float64(v), 'g', -1, 32))
	case float64:
		b.Write(strconv.AppendFloat(number[:0], v, 'g', -1, 64))
	default:
		// fmt handles typed-nil values and recovers panics from Error, String and
		// Format methods while preserving their normal formatting precedence.
		b.WriteString(fmt.Sprint(value))
	}
}

// SetLogger installs a Logger on the default engine. Passing nil or a typed-nil
// pointer panics instead of silently keeping a different logger. Until it is
// called the default engine logs through the console helper described above:
// that console logger
// is a development aid, and integrating a real logging component simply means
// plugging its adapter in here.
func SetLogger(l Logger) { defaultEngine.setLogger(l) }

// Info logs at info level through the Logger installed on the default engine.
// The three package-level log functions are the ergonomic entry for business
// code: once a logger is injected with SetLogger it is the framework sink,
// and calling it through the framework API is the intended use — no need
// to keep or fetch a reference. msg describes the event, followed by
// alternating key/value pairs (see Logger).
func Info(msg string, keysAndValues ...any) { defaultEngine.logger.Info(msg, keysAndValues...) }

// Warn logs at warn level through the Logger installed on the default engine.
func Warn(msg string, keysAndValues ...any) { defaultEngine.logger.Warn(msg, keysAndValues...) }

// Error logs at error level through the Logger installed on the default engine.
func Error(msg string, keysAndValues ...any) { defaultEngine.logger.Error(msg, keysAndValues...) }

// setLogger installs a Logger on this engine. It is safe to call at any point
// during assembling, before or after routes and middleware are registered; nil
// and typed-nil pointers are rejected. Once the engine runs the logger is frozen
// like every other assembling input, and calling this again panics.
func (e *engine) setLogger(l Logger) {
	e.assertAssembling("SetLogger")
	if l == nil {
		e.failStartup("gbl: SetLogger requires a non-nil Logger")
	}
	value := reflect.ValueOf(l)
	if value.Kind() == reflect.Pointer && value.IsNil() {
		e.failStartup(fmt.Errorf("gbl: SetLogger received a typed-nil %T", l))
	}
	e.logger = l
}

// safeLog keeps a failing logging sink from replacing the framework error or
// response that was being handled when the sink was called.
func safeLog(fn func()) {
	defer func() { _ = recover() }()
	fn()
}

// failStartup records a startup, configuration or registration failure through
// the engine Logger and then panics with the very same value. Logging before the
// process dies means a deployment that never reaches serving still leaves a
// durable, sink-routed reason instead of only a bare stderr trace; the panic
// value is unchanged, so any recover-based caller observes exactly what it would
// have without the logging. It is deliberately not used for the request-time
// responseAbort control flow, which is a normal answer rather than a failure.
func (e *engine) failStartup(reason any) {
	safeLog(func() { e.logger.Error("startup aborted", "reason", reason) })
	panic(reason)
}
