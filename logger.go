// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Logger is gbl's minimal, zero-dependency logging contract. The framework
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
// enough.
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
// depth. Both levels share the standard logger because log itself is
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
		e.loggerReplaced = true
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

// failLoggingSetup aborts startup when the user-configured file logging itself
// cannot be initialized — a bad path, missing write permission, a full disk. The
// configured destination is exactly what broke, so the reason cannot be recorded
// there. To keep the failure unmistakable without demanding any log-capture setup
// from the operator, it is delivered three ways at once: a structured line on the
// console through the freshly restored default logger, a plain-language copy in a
// last-resort gbl-start-error.log placed beside the running executable, and the
// panic value. Startup still fails fast — a broken logging configuration must
// never silently run. If even the fallback file cannot be created, the console
// copy still stands.
func (e *engine) failLoggingSetup(err error) {
	// The file config is what failed, so restore the default console logger: it
	// never depends on the destination that just broke.
	e.logger = newStdLogger()

	// Anchor the fallback file next to the binary, a location the operator always
	// knows, rather than in whatever working directory the process was launched
	// from. The resolved path is reported verbatim so the message points at the
	// real file.
	fallback := startErrorFilePath()

	e.logger.Error("file logging could not be initialized",
		"error", err,
		"hint", "check that the logFile/logDir/infoLog/warnLog/errorLog path in application.ini exists and is writable",
		"fallbackFile", fallback)

	e.writeStartErrorFile(fallback, err)

	panic(fmt.Errorf("gbl: file logging could not be initialized: %w (details written to %s)", err, fallback))
}

// startErrorFilePath resolves gbl-start-error.log against the directory holding
// the running executable, so a deployed binary drops the diagnostic beside
// itself instead of into an unpredictable working directory. When the executable
// path cannot be determined it falls back to the bare name, which resolves
// against the current working directory.
func startErrorFilePath() string {
	exe, err := os.Executable()
	if err != nil {
		return startErrorLogFile
	}
	return filepath.Join(filepath.Dir(exe), startErrorLogFile)
}

// writeStartErrorFile drops a plain-language diagnostic into the last-resort
// fallback file, so an operator who turned on file logging — and therefore
// expects to read a file, not a console — still finds out exactly what broke and
// how to fix it. It is best-effort: failing to create even this file is reported
// on the console and otherwise swallowed, because startup is already aborting and
// the reason has been printed there.
func (e *engine) writeStartErrorFile(path string, err error) {
	f, fErr := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if fErr != nil {
		e.logger.Warn("could not create the fallback startup-error file",
			"file", path, "error", fErr)
		return
	}
	defer f.Close()

	_, _ = fmt.Fprintf(f,
		"%s\ngbl: startup aborted - the configured file logging could not be initialized.\nReason: %v\nFix: check the logFile/logDir/infoLog/warnLog/errorLog value in application.ini; the path must exist and be writable. Then restart.\n\n",
		time.Now().Format(time.RFC3339), err)
}

// levelLogger routes the three levels to independent destinations. Each level
// wraps a stdLogger already pointed at its own writer, so formatting, the level
// label and the banner all behave exactly as with the default logger — this
// type only decides which sink a call lands in. The startup banner belongs to
// info, matching the earlier decision that the banner is an info-level record.
type levelLogger struct {
	info stdLogger
	warn stdLogger
	err  stdLogger
}

func (l *levelLogger) Info(msg string, kv ...any)  { l.info.Info(msg, kv...) }
func (l *levelLogger) Warn(msg string, kv ...any)  { l.warn.Warn(msg, kv...) }
func (l *levelLogger) Error(msg string, kv ...any) { l.err.Error(msg, kv...) }
func (l *levelLogger) Banner(block string)         { l.info.Banner(block) }

// loggingConfig resolves only the log-destination keys straight from the raw ini
// snapshot. Those values are plain strings and can never fail to parse, so
// logging can be brought up before the rest of the configuration is validated:
// an invalid config value elsewhere then still lands in the configured log file
// instead of only on stderr. The result is a partial gblConfig carrying just the
// destinations setupLogging needs; Run resolves the full configuration after.
func loggingConfig(ini map[string]map[string]string) *gblConfig {
	cfg := &gblConfig{}
	values := ini["gbl"]
	if values == nil {
		return cfg
	}
	cfg.LogFile = iniValue(values, "LogFile")
	cfg.LogDir = iniValue(values, "LogDir")
	cfg.InfoLog = iniValue(values, "InfoLog")
	cfg.WarnLog = iniValue(values, "WarnLog")
	cfg.ErrorLog = iniValue(values, "ErrorLog")
	return cfg
}

// iniValue returns the raw value stored under a config field's name, accepting
// both spellings formatName produces (lowerCamel and kebab-case); empty when the
// key is absent.
func iniValue(values map[string]string, field string) string {
	lowerFirst, kebabCase := formatName(field)
	if v, ok := values[lowerFirst]; ok {
		return v
	}
	return values[kebabCase]
}

// setupLogging configures level-aware file logging from the resolved
// configuration. Each level's destination is chosen by precedence: an explicit
// per-level path (infoLog/warnLog/errorLog), then a file auto-named inside
// logDir (info.log/warn.log/error.log), then the combined logFile shared by
// every level. A level whose destination resolves to nothing still prints to
// standard output. When no destination is configured at all the default logger
// is left untouched and no file is opened, so the three keys double as the
// on/off switch. Configured levels mirror to stdout AND their file, a path used
// by more than one level opens a single handle, and every opened file is
// remembered so Shutdown closes them. A custom Logger installed via SetLogger
// takes precedence over all of this: when one is present the file keys are
// ignored with a one-time startup warning rather than silently replacing it.
func (e *engine) setupLogging(cfg *gblConfig) error {
	paths := map[string]string{
		"info":  firstNonEmpty(cfg.InfoLog, inDir(cfg.LogDir, "info.log"), cfg.LogFile),
		"warn":  firstNonEmpty(cfg.WarnLog, inDir(cfg.LogDir, "warn.log"), cfg.LogFile),
		"error": firstNonEmpty(cfg.ErrorLog, inDir(cfg.LogDir, "error.log"), cfg.LogFile),
	}
	if paths["info"] == null && paths["warn"] == null && paths["error"] == null {
		return nil
	}

	// An explicit SetLogger means the application already owns log routing and its
	// destination. Overriding it with the built-in file logger would silently drop
	// that sink, so the custom Logger wins. The ignored keys are reported through
	// that same Logger: where the notice lands, or whether it is level-filtered,
	// is the application's own decision, not something the framework overrides.
	if e.loggerReplaced {
		e.logger.Warn("ini log keys ignored because a custom Logger is installed",
			"logFile", cfg.LogFile, "logDir", cfg.LogDir,
			"infoLog", cfg.InfoLog, "warnLog", cfg.WarnLog, "errorLog", cfg.ErrorLog)
		return nil
	}

	// Open each distinct path once; levels sharing a destination share a handle.
	loggers := make(map[string]stdLogger, 3)
	for _, path := range paths {
		if path == null {
			continue
		}
		if _, seen := loggers[path]; seen {
			continue
		}
		lg, err := e.fileStdLogger(path)
		if err != nil {
			return err
		}
		loggers[path] = lg
	}

	// A level with no file still prints to stdout so console output stays whole.
	stdout := stdLogger{l: log.New(consoleWriter(os.Stdout), null, log.LstdFlags)}
	sink := func(path string) stdLogger {
		if path == null {
			return stdout
		}
		return loggers[path]
	}

	e.logger = &levelLogger{
		info: sink(paths["info"]),
		warn: sink(paths["warn"]),
		err:  sink(paths["error"]),
	}
	return nil
}

// fileStdLogger creates the parent directory, opens path for appending, records
// the handle for Shutdown, and returns a stdLogger that mirrors to stdout plus
// the file.
func (e *engine) fileStdLogger(path string) (stdLogger, error) {
	if dir := filepath.Dir(path); dir != null && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return stdLogger{}, err
		}
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return stdLogger{}, err
	}
	e.logFiles = append(e.logFiles, f)

	return stdLogger{l: log.New(io.MultiWriter(consoleWriter(os.Stdout), f), null, log.LstdFlags)}, nil
}

// inDir joins dir and name when dir is set; an empty dir yields an empty path.
func inDir(dir, name string) string {
	if dir == null {
		return null
	}
	return filepath.Join(dir, name)
}

// firstNonEmpty returns the first non-empty value, or the empty string.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != null {
			return v
		}
	}
	return null
}
