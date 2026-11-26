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
	"net"
	"os"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
)

type bannerTestAddr string

func (a bannerTestAddr) Network() string { return "tcp" }
func (a bannerTestAddr) String() string  { return string(a) }

type bannerTestRecord struct {
	level string
	msg   string
	kv    []any
}

type bannerTestLogger struct {
	records []bannerTestRecord
}

func (l *bannerTestLogger) Info(msg string, kv ...any) {
	l.records = append(l.records, bannerTestRecord{level: "INFO", msg: msg, kv: append([]any(nil), kv...)})
}

func (l *bannerTestLogger) Warn(msg string, kv ...any) {
	l.records = append(l.records, bannerTestRecord{level: "WARN", msg: msg, kv: append([]any(nil), kv...)})
}

func (l *bannerTestLogger) Error(msg string, kv ...any) {
	l.records = append(l.records, bannerTestRecord{level: "ERROR", msg: msg, kv: append([]any(nil), kv...)})
}

type bannerTestBannerLogger struct {
	*bannerTestLogger
	blocks []string
}

func (l *bannerTestBannerLogger) Banner(block string) {
	l.blocks = append(l.blocks, block)
}

type bannerTestErrorWriter struct{ err error }

func (w bannerTestErrorWriter) Write([]byte) (int, error) { return 0, w.err }

func TestFrameworkVersion(t *testing.T) {
	tests := []struct {
		name string
		info *debug.BuildInfo
		want string
	}{
		{
			name: "main module tagged",
			info: &debug.BuildInfo{Path: modulePath, Main: debug.Module{Path: modulePath, Version: "v1.2.3"}},
			want: "v1.2.3",
		},
		{
			name: "main module untagged",
			info: &debug.BuildInfo{Path: modulePath, Main: debug.Module{Path: modulePath}},
			want: versionDevel,
		},
		{
			name: "dependency tagged",
			info: &debug.BuildInfo{Path: "example.com/app", Main: debug.Module{Path: "example.com/app", Version: "v0.4.0"}, Deps: []*debug.Module{
				{Path: modulePath, Version: "v1.2.3"},
			}},
			want: "v1.2.3",
		},
		{
			name: "dependency untagged",
			info: &debug.BuildInfo{Path: "example.com/app", Deps: []*debug.Module{{Path: modulePath}}},
			want: versionDevel,
		},
		{
			name: "local replacement has no framework tag",
			info: &debug.BuildInfo{Path: "example.com/app", Deps: []*debug.Module{
				{Path: modulePath, Version: "v1.2.3", Replace: &debug.Module{Path: "../gbl"}},
			}},
			want: versionDevel,
		},
		{
			name: "tagged replacement supplies its own version",
			info: &debug.BuildInfo{Path: "example.com/app", Deps: []*debug.Module{
				{Path: modulePath, Version: "v1.2.3", Replace: &debug.Module{Path: "example.com/fork/gbl", Version: "v1.4.0"}},
			}},
			want: "v1.4.0",
		},
		{
			name: "framework absent",
			info: &debug.BuildInfo{Path: "example.com/app", Deps: []*debug.Module{{Path: "example.com/other", Version: "v9.9.9"}}},
			want: versionDevel,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := frameworkVersion(tt.info); got != tt.want {
				t.Fatalf("frameworkVersion() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBannerVersionMatchesBuildInfo(t *testing.T) {
	want := versionDevel
	if info, ok := debug.ReadBuildInfo(); ok {
		want = frameworkVersion(info)
	}
	if got := bannerVersion(); got != want {
		t.Fatalf("bannerVersion() = %q, want %q", got, want)
	}
}

func TestCountRouteNodes(t *testing.T) {
	if got := countRouteNodes(nil); got != 0 {
		t.Fatalf("countRouteNodes(nil) = %d, want 0", got)
	}
	root := &routeNode{
		exist:      true,
		staticNode: map[string]*routeNode{"static": {exist: true}},
		varNode: []*routeNode{
			{exist: true},
			{staticNode: map[string]*routeNode{"child": {exist: true}}},
		},
	}
	if got := countRouteNodes(root); got != 4 {
		t.Fatalf("countRouteNodes() = %d, want 4", got)
	}
}

func TestCountRoutesIncludesDynamicAndStaticMethods(t *testing.T) {
	e := newEngine()
	defer e.cancelStreams()
	e.roots[methodGet] = &routeNode{
		exist:      true,
		staticNode: map[string]*routeNode{"users": {exist: true}},
		varNode:    []*routeNode{{exist: true}},
	}
	e.roots[methodPost] = &routeNode{exist: true}
	e.staticRoutes[methodGet] = map[string]*routeHandler{
		"/health": {},
		"/ready":  {},
	}
	e.staticRoutes[methodDelete] = map[string]*routeHandler{
		"/users/1": {},
	}

	if got := e.countRoutes(); got != 7 {
		t.Fatalf("countRoutes() = %d, want 7", got)
	}
}

func TestCountRoutesAfterGenerationCountsConcreteRoutes(t *testing.T) {
	e := newEngine()
	defer e.cancelStreams()
	handler := func() {}
	e.register(methodGet, "/users/:id", handler)
	e.register(methodGet, "/users/:id/posts", handler)
	e.register(methodPost, "/users/:id", handler)
	e.register(methodGet, "/health", handler)
	e.register(methodHead, "/health", handler)
	e.generateRoute()

	if got := e.countRoutes(); got != 5 {
		t.Fatalf("countRoutes() after generation = %d, want 5", got)
	}
}

func TestRenderBannerHTTPAndHTTPS(t *testing.T) {
	e := newEngine()
	defer e.cancelStreams()
	e.roots[methodGet] = &routeNode{exist: true}
	e.staticRoutes[methodPost] = map[string]*routeHandler{"/health": {}}

	tests := []struct {
		name   string
		cfg    *Config
		addr   net.Addr
		scheme string
		tls    string
	}{
		{name: "http wildcard", cfg: defaultConfig(), addr: bannerTestAddr("0.0.0.0:9801"), scheme: "http", tls: "disabled"},
		{name: "https concrete", cfg: &Config{useTLS: true}, addr: bannerTestAddr("127.0.0.1:8443"), scheme: "https", tls: "enabled"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rows := [][2]string{
				{"version", bannerVersion()},
				{"listening", tt.scheme + "://" + displayHost(tt.addr)},
				{"tls", tt.tls},
				{"routes", "2"},
			}
			lines := make([]string, len(rows))
			for i, row := range rows {
				lines[i] = fmt.Sprintf("  %-10s %s", row[0], row[1])
			}
			want := bannerArt + "\n\n" + strings.Join(lines, "\n") + "\n"
			if got := e.renderBanner(tt.cfg, tt.addr); got != want {
				t.Fatalf("renderBanner() =\n%q\nwant:\n%q", got, want)
			}
		})
	}
}

func TestYesNo(t *testing.T) {
	if got := yesNo(true); got != "enabled" {
		t.Fatalf("yesNo(true) = %q", got)
	}
	if got := yesNo(false); got != "disabled" {
		t.Fatalf("yesNo(false) = %q", got)
	}
}

func TestPrintBannerPrefersBannerLogger(t *testing.T) {
	e := newEngine()
	defer e.cancelStreams()
	logger := &bannerTestBannerLogger{bannerTestLogger: &bannerTestLogger{}}
	e.logger = logger
	cfg := defaultConfig()
	addr := bannerTestAddr(":9801")
	block := e.renderBanner(cfg, addr)

	e.printBanner(cfg, addr)
	if len(logger.blocks) != 1 || logger.blocks[0] != block {
		t.Fatalf("Banner calls = %#v, want one exact block", logger.blocks)
	}
	if len(logger.records) != 1 {
		t.Fatalf("Info calls = %#v, want only the startup success record", logger.records)
	}
	record := logger.records[0]
	if record.level != "INFO" || record.msg != startupSuccessMessage || len(record.kv) != 0 {
		t.Fatalf("startup success record = %#v", record)
	}
}

func TestPrintBannerFallsBackToSingleInfoRecord(t *testing.T) {
	e := newEngine()
	defer e.cancelStreams()
	logger := &bannerTestLogger{}
	e.logger = logger
	cfg := defaultConfig()
	addr := bannerTestAddr("127.0.0.1:9801")
	block := e.renderBanner(cfg, addr)

	e.printBanner(cfg, addr)
	if len(logger.records) != 2 {
		t.Fatalf("Info calls = %#v, want banner and startup success", logger.records)
	}
	if logger.records[0].level != "INFO" || logger.records[0].msg != block || len(logger.records[0].kv) != 0 {
		t.Fatalf("banner fallback record = %#v", logger.records[0])
	}
	if logger.records[1].level != "INFO" || logger.records[1].msg != startupSuccessMessage {
		t.Fatalf("startup success record = %#v", logger.records[1])
	}
}

func TestStdLoggerBannerBypassesTimestamp(t *testing.T) {
	var out bytes.Buffer
	logger := stdLogger{l: log.New(&out, "", log.LstdFlags)}
	block := "art\n  version     v1.0.0"
	logger.Banner(block)
	if got, want := out.String(), "\n"+block+"\n"; got != want {
		t.Fatalf("Banner output = %q, want verbatim %q", got, want)
	}
}

func TestDisplayHost(t *testing.T) {
	tests := []struct {
		name string
		addr net.Addr
		want string
	}{
		{name: "nil address", addr: nil, want: "?"},
		{name: "unparsed address", addr: bannerTestAddr("/tmp/gbl.sock"), want: "/tmp/gbl.sock"},
		{name: "empty host", addr: bannerTestAddr(":9801"), want: "localhost:9801"},
		{name: "IPv4 wildcard", addr: bannerTestAddr("0.0.0.0:80"), want: "localhost:80"},
		{name: "IPv6 wildcard", addr: bannerTestAddr("[::]:8080"), want: "localhost:8080"},
		{name: "IPv4 concrete", addr: bannerTestAddr("127.0.0.1:8080"), want: "127.0.0.1:8080"},
		{name: "DNS host", addr: bannerTestAddr("example.com:443"), want: "example.com:443"},
		{name: "IPv6 loopback", addr: &net.TCPAddr{IP: net.ParseIP("::1"), Port: 8080}, want: "[::1]:8080"},
		{name: "IPv6 zone", addr: bannerTestAddr("[fe80::1%eth0]:443"), want: "[fe80::1%eth0]:443"},
		{name: "named port", addr: bannerTestAddr("localhost:http"), want: "localhost:http"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := displayHost(tt.addr); got != tt.want {
				t.Fatalf("displayHost(%v) = %q, want %q", tt.addr, got, tt.want)
			}
		})
	}
}

func TestFindLevel(t *testing.T) {
	tests := []struct {
		name  string
		line  string
		level string
		found bool
	}{
		{name: "info", line: "INFO ready", level: "INFO", found: true},
		{name: "warn", line: "WARN slow", level: "WARN", found: true},
		{name: "error", line: "ERROR failed", level: "ERROR", found: true},
		{name: "timestamp prefix", line: "2026/11/26 12:34:56 INFO ready", level: "INFO", found: true},
		{name: "file prefix", line: "2026/11/26 12:34:56 logger.go:95 INFO ready", level: "INFO", found: true},
		{name: "level without trailing whitespace", line: "ERROR", level: "ERROR", found: true},
		{name: "first known level wins", line: "INFO WARN ERROR", level: "INFO", found: true},
		{name: "message level after label", line: "INFO request ERROR failed", level: "INFO", found: true},
		{name: "fourth token is scanned", line: "one two three INFO rest", level: "INFO", found: true},
		{name: "fifth token is not scanned", line: "one two three four INFO rest"},
		{name: "tab delimiter", line: "2026/11/26\tINFO\tready", level: "INFO", found: true},
		{name: "carriage return delimiter", line: "prefix\rWARN ready", level: "WARN", found: true},
		{name: "vertical tab delimiter", line: "prefix\vERROR ready", level: "ERROR", found: true},
		{name: "leading whitespace", line: "\tINFO ready", level: "INFO", found: true},
		{name: "repeated spaces", line: "prefix    INFO ready", level: "INFO", found: true},
		{name: "lowercase is not a label", line: "info ready"},
		{name: "substring is not a label", line: "xINFO ready"},
		{name: "empty", line: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start, end, color, found := findLevel([]byte(tt.line))
			if found != tt.found {
				t.Fatalf("findLevel(%q) found = %t, want %t", tt.line, found, tt.found)
			}
			if !tt.found {
				if start != 0 || end != 0 || color != "" {
					t.Fatalf("not-found result = (%d, %d, %q), want zero values", start, end, color)
				}
				return
			}
			wantStart := strings.Index(tt.line, tt.level)
			wantEnd := wantStart + len(tt.level)
			if start != wantStart || end != wantEnd || color != levelColor[tt.level] {
				t.Fatalf("findLevel(%q) = (%d, %d, %q), want (%d, %d, %q)",
					tt.line, start, end, color, wantStart, wantEnd, levelColor[tt.level])
			}
		})
	}
}

func TestColorWriterTintsOnlyLevelLabel(t *testing.T) {
	tests := []struct {
		name  string
		line  string
		want  string
		found bool
	}{
		{name: "info", line: "INFO ready", want: ansiGreen + "INFO" + ansiReset + " ready", found: true},
		{name: "warn", line: "WARN slow", want: ansiYellow + "WARN" + ansiReset + " slow", found: true},
		{name: "error", line: "ERROR failed", want: ansiRed + "ERROR" + ansiReset + " failed", found: true},
		{name: "timestamp", line: "2026/11/26 12:34:56 INFO ready", want: "2026/11/26 12:34:56 " + ansiGreen + "INFO" + ansiReset + " ready", found: true},
		{name: "label-less banner", line: "art\n  version v1.0.0\n", want: "art\n  version v1.0.0\n"},
		{name: "foreign output", line: "application ready", want: "application ready"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			writer := colorWriter{w: &out, enabled: true}
			n, err := writer.Write([]byte(tt.line))
			if err != nil {
				t.Fatal(err)
			}
			if n != len(tt.line) {
				t.Fatalf("Write returned %d, want original length %d", n, len(tt.line))
			}
			if got := out.String(); got != tt.want {
				t.Fatalf("colored output = %q, want %q", got, tt.want)
			}
			if tt.found && strings.Count(out.String(), ansiReset) != 1 {
				t.Fatalf("colored output did not reset exactly once: %q", out.String())
			}
		})
	}
}

func TestColorWriterDisabledAndErrors(t *testing.T) {
	line := []byte("INFO ready\n")
	var plain bytes.Buffer
	writer := colorWriter{w: &plain, enabled: false}
	n, err := writer.Write(line)
	if err != nil || n != len(line) || !bytes.Equal(plain.Bytes(), line) {
		t.Fatalf("disabled Write = (%d, %v, %q)", n, err, plain.Bytes())
	}

	wantErr := errors.New("sink failed")
	writer = colorWriter{w: bannerTestErrorWriter{err: wantErr}, enabled: false}
	if n, err := writer.Write(line); n != 0 || !errors.Is(err, wantErr) {
		t.Fatalf("disabled error Write = (%d, %v), want (0, %v)", n, err, wantErr)
	}
	writer = colorWriter{w: bannerTestErrorWriter{err: wantErr}, enabled: true}
	if n, err := writer.Write(line); n != 0 || !errors.Is(err, wantErr) {
		t.Fatalf("enabled error Write = (%d, %v), want (0, %v)", n, err, wantErr)
	}
}

func TestNoColorRequested(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  bool
	}{
		{name: "unset or empty", value: "", want: false},
		{name: "zero still requests no color", value: "0", want: true},
		{name: "false still requests no color", value: "false", want: true},
		{name: "whitespace is non-empty", value: " ", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("NO_COLOR", tt.value)
			if got := noColorRequested(); got != tt.want {
				t.Fatalf("noColorRequested() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestIsTerminalRejectsNonTerminals(t *testing.T) {
	if isTerminal(nil) {
		t.Fatal("isTerminal(nil) = true")
	}

	t.Run("regular file", func(t *testing.T) {
		file, err := os.CreateTemp(t.TempDir(), "gbl-color-*")
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		if isTerminal(file) {
			t.Fatal("regular file was classified as a terminal")
		}
	})

	t.Run("directory", func(t *testing.T) {
		dir, err := os.Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer dir.Close()
		if isTerminal(dir) {
			t.Fatal("directory was classified as a terminal")
		}
	})

	t.Run("pipe", func(t *testing.T) {
		reader, writer, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer reader.Close()
		defer writer.Close()
		if isTerminal(reader) || isTerminal(writer) {
			t.Fatal("pipe was classified as a terminal")
		}
	})

	t.Run("closed file", func(t *testing.T) {
		file, err := os.CreateTemp(t.TempDir(), "gbl-color-*")
		if err != nil {
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		if isTerminal(file) {
			t.Fatal("closed file was classified as a terminal")
		}
	})

	t.Run("character device is not necessarily a TTY", func(t *testing.T) {
		file, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		if isTerminal(file) {
			t.Fatal("character device was classified as a TTY without a terminal check")
		}
	})
}

func TestConsoleWriterUsesPlainFilesAndNoColor(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "gbl-color-*")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	t.Setenv("NO_COLOR", "")
	if got := consoleWriter(file); got != file {
		t.Fatalf("consoleWriter(regular file) = %T, want the original *os.File", got)
	}
	t.Setenv("NO_COLOR", "1")
	if got := consoleWriter(file); got != file {
		t.Fatalf("consoleWriter with NO_COLOR = %T, want the original *os.File", got)
	}

	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer devNull.Close()
	if got := consoleWriter(devNull); got != devNull {
		t.Fatalf("consoleWriter with NO_COLOR for a character device = %T, want the original *os.File", got)
	}
}

func TestConsoleWriterDoesNotWrapCharacterDevice(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	file, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if got := consoleWriter(file); got != file {
		t.Fatalf("consoleWriter(character device) = %T, want plain *os.File because it is not a TTY", got)
	}
}

func TestEnableConsoleVTPlatformContract(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "gbl-color-*")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	want := runtime.GOOS != "windows"
	if got := enableConsoleVT(file); got != want {
		t.Fatalf("enableConsoleVT(regular file) on %s = %t, want %t", runtime.GOOS, got, want)
	}
}

func TestColorWriterPreservesForeignWrites(t *testing.T) {
	line := []byte("prefix\nart version\n")
	var out bytes.Buffer
	writer := colorWriter{w: &out, enabled: true}
	if _, err := io.Copy(writer, bytes.NewReader(line)); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), line) {
		t.Fatalf("foreign multiline write changed: %q", out.Bytes())
	}
}
