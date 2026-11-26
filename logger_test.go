// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

type logTestRecord struct {
	level string
	msg   string
	kv    []any
}

type logTestCaptureLogger struct {
	records []logTestRecord
}

func (l *logTestCaptureLogger) Info(msg string, kv ...any) {
	l.records = append(l.records, logTestRecord{level: "INFO", msg: msg, kv: append([]any(nil), kv...)})
}

func (l *logTestCaptureLogger) Warn(msg string, kv ...any) {
	l.records = append(l.records, logTestRecord{level: "WARN", msg: msg, kv: append([]any(nil), kv...)})
}

func (l *logTestCaptureLogger) Error(msg string, kv ...any) {
	l.records = append(l.records, logTestRecord{level: "ERROR", msg: msg, kv: append([]any(nil), kv...)})
}

type logTestStringer struct{ text string }

func (s logTestStringer) String() string { return s.text }

type logTestError struct{ text string }

func (e logTestError) Error() string { return e.text }

type logTestErrorStringer struct{}

func (logTestErrorStringer) Error() string  { return "error-value" }
func (logTestErrorStringer) String() string { return "stringer-value" }

type logTestFormatter struct{ text string }

func (f logTestFormatter) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, "formatted<"+f.text+">")
}

type logTestFormatterErrorStringer struct{}

func (logTestFormatterErrorStringer) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, "formatter-value")
}

func (logTestFormatterErrorStringer) Error() string  { return "error-value" }
func (logTestFormatterErrorStringer) String() string { return "stringer-value" }

type logTestPanicStringer struct{}

func (*logTestPanicStringer) String() string { panic("stringer boom") }

type logTestPanicError struct{}

func (*logTestPanicError) Error() string { panic("error boom") }

type logTestPanicFormatter struct{}

func (*logTestPanicFormatter) Format(fmt.State, rune) { panic("formatter boom") }

type logTestPanicLogger struct{}

func (*logTestPanicLogger) Info(string, ...any)  { panic("info boom") }
func (*logTestPanicLogger) Warn(string, ...any)  { panic("warn boom") }
func (*logTestPanicLogger) Error(string, ...any) { panic("error boom") }

func logTestPanicValue(fn func()) (recovered any) {
	defer func() { recovered = recover() }()
	fn()
	return nil
}

func logTestAppend(value any) (line string, recovered any) {
	defer func() { recovered = recover() }()
	var b strings.Builder
	b.WriteString("prefix:")
	appendLogValue(&b, value)
	return b.String(), nil
}

func TestSetLoggerRejectsNilValues(t *testing.T) {
	e := newEngine()
	t.Cleanup(e.cancelStreams)
	original := &logTestCaptureLogger{}
	e.logger = original
	var typedNil *logTestCaptureLogger
	for _, tc := range []struct {
		name   string
		logger Logger
	}{
		{name: "nil", logger: nil},
		{name: "typed-nil pointer", logger: typedNil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if recovered := logTestPanicValue(func() { e.setLogger(tc.logger) }); recovered == nil {
				t.Fatalf("SetLogger accepted %s", tc.name)
			}
			if e.logger != original {
				t.Fatalf("SetLogger(%s) replaced the installed logger", tc.name)
			}
		})
	}
}

func TestSafeLogContainsSinkPanic(t *testing.T) {
	safeLog(func() { panic("logger boom") })
}

func TestFailStartupPreservesReasonWhenLoggerPanics(t *testing.T) {
	e := newEngine()
	t.Cleanup(e.cancelStreams)
	e.logger = &logTestPanicLogger{}
	reason := errors.New("original startup failure")
	if recovered := logTestPanicValue(func() { e.failStartup(reason) }); recovered != reason {
		t.Fatalf("failStartup panic = %v, want original reason %v", recovered, reason)
	}
}

func TestLogLineFormatAndOddPairs(t *testing.T) {
	tests := []struct {
		name  string
		level string
		msg   string
		kv    []any
		want  string
	}{
		{name: "no fields", level: "INFO", msg: "ready", want: "INFO ready"},
		{name: "even pairs", level: "WARN", msg: "slow", kv: []any{"route", "/users", "attempt", 3}, want: "WARN slow route=/users attempt=3"},
		{name: "odd trailing value", level: "ERROR", msg: "failed", kv: []any{"reason", "boom", 7}, want: "ERROR failed reason=boom 7"},
		{name: "single dangling value", level: "INFO", msg: "event", kv: []any{"tail"}, want: "INFO event tail"},
		{name: "scalar keys and nil", level: "INFO", msg: "values", kv: []any{1, true, "missing", nil}, want: "INFO values 1=true missing=<nil>"},
		{name: "message is verbatim", level: "INFO", msg: "two words", kv: []any{"key", "two words"}, want: "INFO two words key=two words"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := logLine(tt.level, tt.msg, tt.kv); got != tt.want {
				t.Fatalf("logLine() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAppendLogValueScalars(t *testing.T) {
	tests := []struct {
		name  string
		value any
		want  string
	}{
		{name: "nil", value: nil, want: "<nil>"},
		{name: "string", value: "hello", want: "hello"},
		{name: "empty string", value: "", want: ""},
		{name: "bool true", value: true, want: "true"},
		{name: "bool false", value: false, want: "false"},
		{name: "int", value: int(-42), want: "-42"},
		{name: "int8", value: int8(-8), want: "-8"},
		{name: "int16", value: int16(-1600), want: "-1600"},
		{name: "int32", value: int32(-32000), want: "-32000"},
		{name: "int64", value: int64(math.MinInt64), want: "-9223372036854775808"},
		{name: "uint", value: uint(42), want: "42"},
		{name: "uint8", value: uint8(8), want: "8"},
		{name: "uint16", value: uint16(1600), want: "1600"},
		{name: "uint32", value: uint32(math.MaxUint32), want: "4294967295"},
		{name: "uint64", value: uint64(math.MaxUint64), want: "18446744073709551615"},
		{name: "uintptr", value: uintptr(1234), want: "1234"},
		{name: "float32", value: float32(0.1), want: "0.1"},
		{name: "float64", value: float64(0.1), want: "0.1"},
		{name: "positive infinity", value: math.Inf(1), want: "+Inf"},
		{name: "negative infinity", value: math.Inf(-1), want: "-Inf"},
		{name: "NaN", value: math.NaN(), want: "NaN"},
		{name: "slice fallback", value: []int{1, 2}, want: "[1 2]"},
		{name: "struct fallback", value: struct{ A string }{"x"}, want: "{x}"},
		{name: "duration fallback", value: 1500 * time.Millisecond, want: "1.5s"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var b strings.Builder
			b.WriteString("before:")
			appendLogValue(&b, tt.value)
			if got, want := b.String(), "before:"+tt.want; got != want {
				t.Fatalf("appendLogValue() appended %q, want %q", got, want)
			}
		})
	}
}

func TestAppendLogValueInterfacesAndPrecedence(t *testing.T) {
	tests := []struct {
		name  string
		value any
		want  string
	}{
		{name: "error", value: errors.New("read failed"), want: "read failed"},
		{name: "custom error", value: logTestError{text: "custom failure"}, want: "custom failure"},
		{name: "stringer", value: logTestStringer{text: "string value"}, want: "string value"},
		{name: "error precedes stringer", value: logTestErrorStringer{}, want: "error-value"},
		{name: "formatter", value: logTestFormatter{text: "value"}, want: "formatted<value>"},
		{name: "formatter precedes error and Stringer", value: logTestFormatterErrorStringer{}, want: "formatter-value"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var b strings.Builder
			appendLogValue(&b, tt.value)
			if got := b.String(); got != tt.want {
				t.Fatalf("appendLogValue() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAppendLogValueTypedNilAndPanickingMethods(t *testing.T) {
	var nilStringer *logTestPanicStringer
	var nilError *logTestPanicError
	var nilFormatter *logTestPanicFormatter

	tests := []struct {
		name  string
		value any
	}{
		{name: "typed nil Stringer", value: nilStringer},
		{name: "typed nil error", value: nilError},
		{name: "typed nil Formatter", value: nilFormatter},
		{name: "panicking Stringer", value: &logTestPanicStringer{}},
		{name: "panicking error", value: &logTestPanicError{}},
		{name: "panicking Formatter", value: &logTestPanicFormatter{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, recovered := logTestAppend(tt.value)
			if recovered != nil {
				t.Errorf("appendLogValue panicked: %v", recovered)
			}
			want := "prefix:" + fmt.Sprint(tt.value)
			if got != want {
				t.Errorf("appendLogValue() = %q, want fmt.Sprint-compatible %q", got, want)
			}
		})
	}
}

func TestStdLoggerLevelsAndBanner(t *testing.T) {
	var out bytes.Buffer
	logger := stdLogger{l: log.New(&out, "", 0)}

	logger.Info("ready", "attempt", 1)
	logger.Warn("slow", "route", "/x", "duration", time.Second)
	logger.Error("failed", "error", errors.New("boom"), "retry", true)
	logger.Banner("line one\nline two")

	want := "INFO ready attempt=1\n" +
		"WARN slow route=/x duration=1s\n" +
		"ERROR failed error=boom retry=true\n" +
		"\nline one\nline two\n"
	if got := out.String(); got != want {
		t.Fatalf("stdLogger output = %q, want %q", got, want)
	}
}

func TestStdLoggerDoesNotPanicForPanickingValues(t *testing.T) {
	var nilError *logTestPanicError
	var nilStringer *logTestPanicStringer
	tests := []struct {
		name string
		log  func(stdLogger)
		want string
	}{
		{name: "Info", log: func(l stdLogger) { l.Info("event", "value", &logTestPanicStringer{}) }, want: "INFO event value=%!v(PANIC=String method: stringer boom)\n"},
		{name: "Warn", log: func(l stdLogger) { l.Warn("event", "value", &logTestPanicError{}) }, want: "WARN event value=%!v(PANIC=Error method: error boom)\n"},
		{name: "Error", log: func(l stdLogger) { l.Error("event", "value", &logTestPanicFormatter{}) }, want: "ERROR event value=%!v(PANIC=Format method: formatter boom)\n"},
		{name: "typed nil error", log: func(l stdLogger) { l.Error("event", "value", nilError) }, want: "ERROR event value=<nil>\n"},
		{name: "typed nil Stringer", log: func(l stdLogger) { l.Info("event", "value", nilStringer) }, want: "INFO event value=<nil>\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			logger := stdLogger{l: log.New(&out, "", 0)}
			if recovered := logTestPanicValue(func() { tt.log(logger) }); recovered != nil {
				t.Errorf("logger panicked: %v", recovered)
			}
			if got := out.String(); got != tt.want {
				t.Errorf("logger output = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNewStdLoggerUsesPrivateSink(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldStderr := os.Stderr
	oldOutput := log.Writer()
	oldFlags := log.Flags()
	oldPrefix := log.Prefix()
	defer func() {
		os.Stderr = oldStderr
		log.SetOutput(oldOutput)
		log.SetFlags(oldFlags)
		log.SetPrefix(oldPrefix)
		_ = reader.Close()
		_ = writer.Close()
	}()

	os.Stderr = writer
	logger := newStdLogger()
	if logger.l == nil {
		t.Fatal("newStdLogger returned a nil private logger")
	}
	if got := logger.l.Flags(); got != log.LstdFlags {
		t.Fatalf("private logger flags = %d, want %d", got, log.LstdFlags)
	}
	if got := logger.l.Prefix(); got != "" {
		t.Fatalf("private logger prefix = %q, want empty", got)
	}

	var application bytes.Buffer
	log.SetOutput(&application)
	log.SetFlags(0)
	log.SetPrefix("application: ")
	logger.Info("framework event", "key", "value")
	log.Print("application event")

	if got, want := application.String(), "application: application event\n"; got != want {
		t.Fatalf("global logger output = %q, want %q", got, want)
	}
	_ = writer.Close()
	os.Stderr = oldStderr
	framework, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} INFO framework event key=value\n$`).Match(framework) {
		t.Fatalf("private logger output has unexpected format: %q", framework)
	}
	if strings.Contains(string(framework), "application event") {
		t.Fatalf("framework sink captured global logger output: %q", framework)
	}
}

func TestSetLoggerRoutesLevelsAndRejectsNil(t *testing.T) {
	previous := defaultEngine.logger
	previousRunState := defaultEngine.runState.Load()
	defaultEngine.runState.Store(runStateIdle)
	t.Cleanup(func() {
		defaultEngine.logger = previous
		defaultEngine.runState.Store(previousRunState)
	})

	first := &logTestCaptureLogger{}
	SetLogger(first)
	if defaultEngine.logger != first {
		t.Fatal("SetLogger did not install the supplied logger")
	}
	Info("info event", "key", 1)
	Warn("warn event", "key", 2)
	Error("error event", "key", 3)
	if recovered := logTestPanicValue(func() { SetLogger(nil) }); recovered == nil {
		t.Fatal("SetLogger(nil) did not panic")
	}
	if defaultEngine.logger != first {
		t.Fatal("rejected SetLogger(nil) replaced the working logger")
	}

	second := &logTestCaptureLogger{}
	SetLogger(second)
	Info("replacement", "ok", true)

	want := []logTestRecord{
		{level: "INFO", msg: "info event", kv: []any{"key", 1}},
		{level: "WARN", msg: "warn event", kv: []any{"key", 2}},
		{level: "ERROR", msg: "error event", kv: []any{"key", 3}},
		{level: "ERROR", msg: "startup aborted", kv: []any{"reason", "gbl: SetLogger requires a non-nil Logger"}},
	}
	if len(first.records) != len(want) {
		t.Fatalf("first logger received %d records, want %d", len(first.records), len(want))
	}
	for i, record := range first.records {
		if record.level != want[i].level || record.msg != want[i].msg || fmt.Sprint(record.kv) != fmt.Sprint(want[i].kv) {
			t.Errorf("record %d = %#v, want %#v", i, record, want[i])
		}
	}
	if len(second.records) != 1 || second.records[0].msg != "replacement" {
		t.Fatalf("replacement logger records = %#v", second.records)
	}
}

func TestSetLoggerIsFrozenAfterRunClaim(t *testing.T) {
	previous := defaultEngine.logger
	previousRunState := defaultEngine.runState.Load()
	defaultEngine.runState.Store(runStateRunning)
	t.Cleanup(func() {
		defaultEngine.logger = previous
		defaultEngine.runState.Store(previousRunState)
	})

	recovered := logTestPanicValue(func() { SetLogger(&logTestCaptureLogger{}) })
	if recovered != "gbl.SetLogger must be called before Run; the engine is read-only while serving" {
		t.Fatalf("SetLogger panic = %#v", recovered)
	}
	if defaultEngine.logger != previous {
		t.Fatal("rejected SetLogger changed the logger")
	}
}

func TestEngineSetLoggerRejectsNilAndFailStartup(t *testing.T) {
	e := newEngine()
	defer e.cancelStreams()
	initial := &logTestCaptureLogger{}
	e.logger = initial
	if recovered := logTestPanicValue(func() { e.setLogger(nil) }); recovered == nil {
		t.Fatal("engine.setLogger(nil) did not panic")
	}
	if e.logger != initial {
		t.Fatal("rejected engine.setLogger(nil) replaced the logger")
	}

	capture := &logTestCaptureLogger{}
	e.setLogger(capture)
	reason := errors.New("startup failure")
	recovered := logTestPanicValue(func() { e.failStartup(reason) })
	if recovered != reason {
		t.Fatalf("failStartup panic = %#v, want original reason %#v", recovered, reason)
	}
	if len(capture.records) != 1 {
		t.Fatalf("failStartup logged %d records, want 1", len(capture.records))
	}
	record := capture.records[0]
	if record.level != "ERROR" || record.msg != "startup aborted" || len(record.kv) != 2 || record.kv[0] != "reason" || record.kv[1] != reason {
		t.Fatalf("failStartup record = %#v", record)
	}
}
