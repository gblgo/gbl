// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const responseTestTimeout = 2 * time.Second

type responseTestWriter struct {
	header      http.Header
	statuses    []int
	finalStatus int
	body        bytes.Buffer
	writeCalls  int
	writeN      int
	writeErr    error
}

func responseTestNewWriter() *responseTestWriter {
	return &responseTestWriter{header: make(http.Header), writeN: -1}
}

func (w *responseTestWriter) Header() http.Header { return w.header }

func (w *responseTestWriter) WriteHeader(code int) {
	w.statuses = append(w.statuses, code)
	if code == http.StatusSwitchingProtocols || code >= http.StatusOK {
		w.finalStatus = code
	}
}

func (w *responseTestWriter) Write(p []byte) (int, error) {
	w.writeCalls++
	if len(p) > 0 && !bodyAllowedForStatus(w.finalStatus) {
		return 0, http.ErrBodyNotAllowed
	}
	if w.writeErr != nil {
		n := len(p)
		if w.writeN >= 0 && w.writeN < n {
			n = w.writeN
		}
		if n > 0 {
			_, _ = w.body.Write(p[:n])
		}
		return n, w.writeErr
	}
	return w.body.Write(p)
}

type responseTestFlushWriter struct {
	*responseTestWriter
	flushCalls int
}

func (w *responseTestFlushWriter) Flush() { w.flushCalls++ }

type responseTestFlushErrorWriter struct {
	*responseTestWriter
	flushCalls int
	flushErr   error
}

func (w *responseTestFlushErrorWriter) FlushError() error {
	w.flushCalls++
	return w.flushErr
}

type responseTestUnwrapWriter struct {
	http.ResponseWriter
}

func (w *responseTestUnwrapWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

type responseTestHijackWriter struct {
	*responseTestWriter
	conn     net.Conn
	buffered *bufio.ReadWriter
	err      error
	calls    int
}

func (w *responseTestHijackWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.calls++
	if w.err != nil {
		return nil, nil, w.err
	}
	return w.conn, w.buffered, nil
}

type responseTestPushWriter struct {
	*responseTestWriter
	targets []string
	options []*http.PushOptions
	err     error
}

func (w *responseTestPushWriter) Push(target string, opts *http.PushOptions) error {
	w.targets = append(w.targets, target)
	w.options = append(w.options, opts)
	return w.err
}

type responseTestControllerWriter struct {
	*responseTestWriter
	writeDeadline time.Time
	readDeadline  time.Time
	fullDuplex    bool
	err           error
}

func (w *responseTestControllerWriter) SetWriteDeadline(deadline time.Time) error {
	w.writeDeadline = deadline
	return w.err
}

func (w *responseTestControllerWriter) SetReadDeadline(deadline time.Time) error {
	w.readDeadline = deadline
	return w.err
}

func (w *responseTestControllerWriter) EnableFullDuplex() error {
	w.fullDuplex = true
	return w.err
}

type responseTestConn struct {
	mu            sync.Mutex
	closed        bool
	closeCalls    int
	writeDeadline time.Time
	readDeadline  time.Time
}

func (c *responseTestConn) Read([]byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, net.ErrClosed
	}
	return 0, io.EOF
}

func (c *responseTestConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, net.ErrClosed
	}
	return len(p), nil
}

func (c *responseTestConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	c.closeCalls++
	return nil
}

func (c *responseTestConn) LocalAddr() net.Addr  { return &net.TCPAddr{} }
func (c *responseTestConn) RemoteAddr() net.Addr { return &net.TCPAddr{} }
func (c *responseTestConn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writeDeadline = t
	c.readDeadline = t
	return nil
}
func (c *responseTestConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.readDeadline = t
	return nil
}
func (c *responseTestConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writeDeadline = t
	return nil
}
func (c *responseTestConn) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

type responseTestLogEntry struct {
	msg string
	kv  []any
}

type responseTestLogger struct {
	mu      sync.Mutex
	entries []responseTestLogEntry
}

func (*responseTestLogger) Info(string, ...any) {}
func (*responseTestLogger) Warn(string, ...any) {}
func (l *responseTestLogger) Error(msg string, kv ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, responseTestLogEntry{msg: msg, kv: append([]any(nil), kv...)})
}
func (l *responseTestLogger) snapshot() []responseTestLogEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	entries := make([]responseTestLogEntry, len(l.entries))
	for i, entry := range l.entries {
		entries[i] = responseTestLogEntry{msg: entry.msg, kv: append([]any(nil), entry.kv...)}
	}
	return entries
}

type responseTestString string
type responseTestBytes []byte
type responseTestStrings []string
type responseTestInt int
type responseTestUint uint
type responseTestFloat float64
type responseTestBool bool

type responseTestPayload struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

type responseTestError string

func (e responseTestError) Error() string { return string(e) }

type responseTestNilError struct{}

func (e *responseTestNilError) Error() string {
	if e == nil {
		panic("ctxTest typed-nil Error called")
	}
	return "ctxTest nonnil error"
}

type responseTestPanicError struct{}

func (responseTestPanicError) Error() string { panic("ctxTest panicking Error called") }

type responseTestBadJSON struct{}

func (responseTestBadJSON) MarshalJSON() ([]byte, error) {
	return nil, errors.New("ctxTest JSON secret")
}

type responseTestPanicJSON struct{}

func (responseTestPanicJSON) MarshalJSON() ([]byte, error) { panic("payload marshal boom") }

func responseTestNewEngine(t *testing.T) *engine {
	t.Helper()
	e := newEngine()
	t.Cleanup(func() {
		e.cancelStreams()
		e.hijacks.closeAll()
	})
	return e
}

func responseTestServe(t *testing.T, e *engine, method, path, body, contentType string) (*httptest.ResponseRecorder, any) {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	recorder := httptest.NewRecorder()
	return recorder, responseTestServeWriter(t, e, recorder, request)
}

func responseTestServeWriter(t *testing.T, e *engine, writer http.ResponseWriter, request *http.Request) (panicValue any) {
	t.Helper()
	ctx, cancel := context.WithCancel(request.Context())
	defer cancel()
	request = request.WithContext(ctx)
	type result struct{ panicValue any }
	done := make(chan result, 1)
	go func() {
		var escaped any
		defer func() {
			escaped = recover()
			done <- result{panicValue: escaped}
		}()
		e.ServeHTTP(writer, request)
	}()

	timer := time.NewTimer(responseTestTimeout)
	defer timer.Stop()
	select {
	case got := <-done:
		return got.panicValue
	case <-timer.C:
		cancel()
		select {
		case <-done:
		case <-time.After(responseTestTimeout):
		}
		t.Fatal("request exceeded responseTestTimeout")
		return nil
	}
}

func responseTestCapturePanic(fn func()) (value any) {
	defer func() { value = recover() }()
	fn()
	return nil
}

func responseTestCatchPanic(e *engine, rw responseWriter, fn func()) (escaped any) {
	defer func() { escaped = recover() }()
	defer e.catchRuntimePanic(rw)
	fn()
	return nil
}

func responseTestCheckResponse(t *testing.T, recorder *httptest.ResponseRecorder, status int, body, mediaType string) {
	t.Helper()
	if recorder.Code != status {
		t.Errorf("status = %d, want %d", recorder.Code, status)
	}
	if got := recorder.Body.String(); got != body {
		t.Errorf("body = %q, want %q", got, body)
	}
	if got := recorder.Result().Header.Get(contentType); got != mediaType {
		t.Errorf("Content-Type = %q, want %q", got, mediaType)
	}
}

func responseTestTypedPointerChain(layers int) reflect.Value {
	typ := reflect.TypeFor[string]()
	value := reflect.ValueOf("ok")
	for range layers {
		typ = reflect.PointerTo(typ)
		pointer := reflect.New(typ.Elem())
		pointer.Elem().Set(value)
		value = pointer
	}
	return value
}

func responseTestCyclicMap() map[string]any {
	value := make(map[string]any)
	value["self"] = value
	return value
}

func responseTestRegistrySize(registry *hijackRegistry) int {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	return len(registry.connections)
}

func TestResponseConvertToBufferScalars(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
		want  string
	}{
		{name: "string", value: "hello", want: "hello"},
		{name: "named string", value: responseTestString("named"), want: "named"},
		{name: "bool", value: true, want: "true"},
		{name: "named bool", value: responseTestBool(false), want: "false"},
		{name: "int", value: int(-42), want: "-42"},
		{name: "int8", value: int8(-8), want: "-8"},
		{name: "int16", value: int16(-16), want: "-16"},
		{name: "int32", value: int32(-32), want: "-32"},
		{name: "int64", value: int64(-64), want: "-64"},
		{name: "named int", value: responseTestInt(-7), want: "-7"},
		{name: "uint", value: uint(42), want: "42"},
		{name: "uint8", value: uint8(8), want: "8"},
		{name: "uint16", value: uint16(16), want: "16"},
		{name: "uint32", value: uint32(32), want: "32"},
		{name: "uint64", value: uint64(64), want: "64"},
		{name: "uintptr", value: uintptr(9), want: "9"},
		{name: "named uint", value: responseTestUint(11), want: "11"},
		{name: "float32", value: float32(1.25), want: "1.25"},
		{name: "float64", value: float64(-2.5), want: "-2.5"},
		{name: "named float", value: responseTestFloat(3.5), want: "3.5"},
		{name: "positive infinity", value: math.Inf(1), want: "+Inf"},
		{name: "not a number", value: math.NaN(), want: "NaN"},
		{name: "complex64", value: complex64(complex(1, 2)), want: "(1+2i)"},
		{name: "complex128", value: complex(1, -2), want: "(1-2i)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf := new(bytes.Buffer)
			mediaType, err := convertToBuffer(buf, reflect.ValueOf(tc.value))
			if err != nil {
				t.Fatalf("convertToBuffer returned %v", err)
			}
			if mediaType != textPlainCharset {
				t.Errorf("media type = %q, want %q", mediaType, textPlainCharset)
			}
			if got := buf.String(); got != tc.want {
				t.Errorf("body = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResponseConvertToBufferInvalidAndNil(t *testing.T) {
	var nilInterface any
	var nilPointer *responseTestPayload
	for _, tc := range []struct {
		name  string
		value reflect.Value
	}{
		{name: "invalid", value: reflect.Value{}},
		{name: "nil interface", value: reflect.ValueOf(nilInterface)},
		{name: "nil pointer", value: reflect.ValueOf(nilPointer)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf := new(bytes.Buffer)
			mediaType, err := convertToBuffer(buf, tc.value)
			if err != nil || mediaType != textPlainCharset || buf.Len() != 0 {
				t.Fatalf("convertToBuffer = (%q, %q, %v), want empty text", mediaType, buf.String(), err)
			}
		})
	}
}

func TestResponseConvertToBufferCompositesAndBytes(t *testing.T) {
	payload := responseTestPayload{Name: "alpha", Count: 2}
	var nilBytes []byte
	var nilSlice []int
	var nilMap map[string]int
	for _, tc := range []struct {
		name      string
		value     any
		want      string
		mediaType string
	}{
		{name: "bytes", value: []byte{0x41, 0x00, 0x42}, want: "A\x00B", mediaType: textPlainCharset},
		{name: "named bytes", value: responseTestBytes("raw"), want: "raw", mediaType: textPlainCharset},
		{name: "pointer to bytes", value: &[]byte{0x41, 0x42}, want: "AB", mediaType: textPlainCharset},
		{name: "nil bytes", value: nilBytes, want: "", mediaType: textPlainCharset},
		{name: "int slice", value: []int{1, 2}, want: "[1,2]", mediaType: applicationJsonCharset},
		{name: "named string slice", value: responseTestStrings{"a", "b"}, want: `["a","b"]`, mediaType: applicationJsonCharset},
		{name: "array", value: [3]int{1, 2, 3}, want: "[1,2,3]", mediaType: applicationJsonCharset},
		{name: "byte array is JSON", value: [3]byte{1, 2, 3}, want: "[1,2,3]", mediaType: applicationJsonCharset},
		{name: "map", value: map[string]int{"b": 2, "a": 1}, want: `{"a":1,"b":2}`, mediaType: applicationJsonCharset},
		{name: "struct", value: payload, want: `{"name":"alpha","count":2}`, mediaType: applicationJsonCharset},
		{name: "pointer to struct", value: &payload, want: `{"name":"alpha","count":2}`, mediaType: applicationJsonCharset},
		{name: "nil slice", value: nilSlice, want: "null", mediaType: applicationJsonCharset},
		{name: "nil map", value: nilMap, want: "null", mediaType: applicationJsonCharset},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf := new(bytes.Buffer)
			mediaType, err := convertToBuffer(buf, reflect.ValueOf(tc.value))
			if err != nil {
				t.Fatalf("convertToBuffer returned %v", err)
			}
			if mediaType != tc.mediaType || buf.String() != tc.want {
				t.Fatalf("convertToBuffer = (%q, %q), want (%q, %q)", mediaType, buf.String(), tc.mediaType, tc.want)
			}
		})
	}

	t.Run("interface is dereferenced", func(t *testing.T) {
		var value any = payload
		buf := new(bytes.Buffer)
		mediaType, err := convertToBuffer(buf, reflect.ValueOf(&value).Elem())
		if err != nil || mediaType != applicationJsonCharset || buf.String() != `{"name":"alpha","count":2}` {
			t.Fatalf("interface conversion = (%q, %q, %v)", mediaType, buf.String(), err)
		}
	})
}

func TestResponseConvertToBufferErrors(t *testing.T) {
	var errorValue error = errors.New("interface error")
	var nilError *responseTestError
	pointerError := responseTestError("pointer error")
	for _, tc := range []struct {
		name  string
		value any
		want  string
	}{
		{name: "standard error", value: errors.New("plain error"), want: "plain error"},
		{name: "named error", value: responseTestError("named error"), want: "named error"},
		{name: "pointer error", value: &pointerError, want: "pointer error"},
		{name: "nil error pointer", value: nilError, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf := new(bytes.Buffer)
			mediaType, err := convertToBuffer(buf, reflect.ValueOf(tc.value))
			if err != nil || mediaType != textPlainCharset || buf.String() != tc.want {
				t.Fatalf("error conversion = (%q, %q, %v), want (%q, %q, nil)", mediaType, buf.String(), err, textPlainCharset, tc.want)
			}
		})
	}

	t.Run("error interface", func(t *testing.T) {
		buf := new(bytes.Buffer)
		mediaType, err := convertToBuffer(buf, reflect.ValueOf(&errorValue).Elem())
		if err != nil || mediaType != textPlainCharset || buf.String() != "interface error" {
			t.Fatalf("error interface conversion = (%q, %q, %v)", mediaType, buf.String(), err)
		}
	})
}

func TestResponseConvertToBufferFailuresDoNotWritePartialData(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
	}{
		{name: "channel", value: make(chan int)},
		{name: "function", value: func() {}},
		{name: "cyclic map", value: responseTestCyclicMap()},
		{name: "NaN in slice", value: []float64{math.NaN()}},
		{name: "marshal failure", value: responseTestBadJSON{}},
		{name: "pointer depth", value: responseTestTypedPointerChain(maxResponseDerefDepth + 1).Interface()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buf := bytes.NewBufferString("prefix:")
			mediaType, err := convertToBuffer(buf, reflect.ValueOf(tc.value))
			if err == nil {
				t.Fatal("convertToBuffer succeeded, want an error")
			}
			if mediaType != empty {
				t.Errorf("media type = %q, want empty", mediaType)
			}
			if got := buf.String(); got != "prefix:" {
				t.Errorf("buffer = %q, want unchanged prefix", got)
			}
		})
	}
}

func TestResponseConvertToBufferDepthLimit(t *testing.T) {
	buf := new(bytes.Buffer)
	mediaType, err := convertToBuffer(buf, responseTestTypedPointerChain(maxResponseDerefDepth))
	if err != nil || mediaType != textPlainCharset || buf.String() != "ok" {
		t.Fatalf("depth %d = (%q, %q, %v), want ok text", maxResponseDerefDepth, mediaType, buf.String(), err)
	}

	buf.Reset()
	mediaType, err = convertToBuffer(buf, responseTestTypedPointerChain(maxResponseDerefDepth+1))
	if err == nil || mediaType != empty || buf.Len() != 0 {
		t.Fatalf("excessive depth = (%q, %q, %v), want empty error", mediaType, buf.String(), err)
	}
}

func TestResponsePipelineSerialization(t *testing.T) {
	for _, tc := range []struct {
		name      string
		handler   any
		body      string
		mediaType string
	}{
		{name: "string", handler: func() string { return "hello" }, body: "hello", mediaType: textPlainCharset},
		{name: "named string", handler: func() responseTestString { return "named" }, body: "named", mediaType: textPlainCharset},
		{name: "bool", handler: func() bool { return true }, body: "true", mediaType: textPlainCharset},
		{name: "integer", handler: func() int { return -42 }, body: "-42", mediaType: textPlainCharset},
		{name: "float", handler: func() float64 { return 1.25 }, body: "1.25", mediaType: textPlainCharset},
		{name: "complex", handler: func() complex128 { return complex(1, 2) }, body: "(1+2i)", mediaType: textPlainCharset},
		{name: "bytes", handler: func() []byte { return []byte("AB") }, body: "AB", mediaType: textPlainCharset},
		{name: "named bytes", handler: func() responseTestBytes { return responseTestBytes("raw") }, body: "raw", mediaType: textPlainCharset},
		{name: "slice", handler: func() []int { return []int{1, 2} }, body: "[1,2]", mediaType: applicationJsonCharset},
		{name: "array", handler: func() [2]string { return [2]string{"a", "b"} }, body: `["a","b"]`, mediaType: applicationJsonCharset},
		{name: "map", handler: func() map[string]int { return map[string]int{"a": 1} }, body: `{"a":1}`, mediaType: applicationJsonCharset},
		{name: "struct", handler: func() responseTestPayload { return responseTestPayload{Name: "alpha", Count: 2} }, body: `{"name":"alpha","count":2}`, mediaType: applicationJsonCharset},
		{name: "interface struct", handler: func() any { return responseTestPayload{Name: "alpha", Count: 2} }, body: `{"name":"alpha","count":2}`, mediaType: applicationJsonCharset},
		{name: "error", handler: func() error { return errors.New("handler error") }, body: "handler error", mediaType: textPlainCharset},
		{name: "nil interface", handler: func() any { return nil }, body: "", mediaType: textPlainCharset},
		{name: "typed nil", handler: func() *responseTestPayload { return nil }, body: "", mediaType: textPlainCharset},
		{name: "no return", handler: func() {}, body: "", mediaType: textPlainCharset},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := responseTestNewEngine(t)
			e.register(methodGet, "/responseTest", tc.handler)
			e.generateRoute()
			rec, escaped := responseTestServe(t, e, http.MethodGet, "/responseTest", "", "")
			if escaped != nil {
				t.Fatalf("pipeline leaked a panic: %v", escaped)
			}
			responseTestCheckResponse(t, rec, http.StatusOK, tc.body, tc.mediaType)
		})
	}
}

func TestResponsePipelineSerializationFailuresAreGeneric(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler any
	}{
		{name: "channel", handler: func() chan int { return make(chan int) }},
		{name: "cyclic map", handler: func() map[string]any { return responseTestCyclicMap() }},
		{name: "NaN in composite", handler: func() []float64 { return []float64{math.NaN()} }},
		{name: "marshal failure", handler: func() responseTestBadJSON { return responseTestBadJSON{} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := responseTestNewEngine(t)
			logger := &responseTestLogger{}
			e.setLogger(logger)
			e.register(methodGet, "/responseTest", tc.handler)
			e.generateRoute()
			rec, escaped := responseTestServe(t, e, http.MethodGet, "/responseTest", "", "")
			if escaped != nil {
				t.Fatalf("pipeline leaked a panic: %v", escaped)
			}
			responseTestCheckResponse(t, rec, http.StatusInternalServerError, responseSerializationFailed, textPlainCharset)
			if strings.Contains(rec.Body.String(), "ctxTest JSON secret") {
				t.Fatal("serialization detail leaked into the response")
			}
			if entries := logger.snapshot(); len(entries) != 0 {
				t.Fatalf("serialization abort was logged as a crash: %+v", entries)
			}
		})
	}
}

func TestResponsePipelineMiddlewareObservesStagedResult(t *testing.T) {
	e := responseTestNewEngine(t)
	payload := responseTestPayload{Name: "alpha", Count: 2}
	var gotResult any
	var gotStatus, gotSize int
	var gotWritten bool
	e.use(func(c *Context) {
		c.Next()
		gotResult = c.Result()
		gotStatus = c.StatusCode()
		gotSize = c.Size()
		gotWritten = c.IsWritten()
	})
	e.register(methodGet, "/responseTest", func(w http.ResponseWriter) responseTestPayload {
		w.WriteHeader(http.StatusCreated)
		return payload
	})
	e.generateRoute()

	rec, escaped := responseTestServe(t, e, http.MethodGet, "/responseTest", "", "")
	if escaped != nil {
		t.Fatalf("pipeline leaked a panic: %v", escaped)
	}
	responseTestCheckResponse(t, rec, http.StatusCreated, `{"name":"alpha","count":2}`, applicationJsonCharset)
	if gotResult != payload || gotStatus != http.StatusCreated || gotWritten {
		t.Fatalf("middleware observed result/status/written = (%v, %d, %v)", gotResult, gotStatus, gotWritten)
	}
	if want := len(`{"name":"alpha","count":2}`); gotSize != want {
		t.Fatalf("middleware Size = %d, want %d", gotSize, want)
	}
}

func TestResponsePipelineMiddlewareCanOverrideStagedResult(t *testing.T) {
	e := responseTestNewEngine(t)
	e.use(func(c *Context) {
		c.Next()
		if c.IsWritten() {
			return
		}
		c.writer.Header().Set(contentType, "application/x-responseTest")
		c.writer.WriteHeader(http.StatusAccepted)
		_, _ = c.writer.Write([]byte("override"))
	})
	e.register(methodGet, "/responseTest", func() string { return "staged" })
	e.generateRoute()

	rec, escaped := responseTestServe(t, e, http.MethodGet, "/responseTest", "", "")
	if escaped != nil {
		t.Fatalf("pipeline leaked a panic: %v", escaped)
	}
	responseTestCheckResponse(t, rec, http.StatusAccepted, "override", "application/x-responseTest")
}

func TestResponsePipelineSuppressesBodiesForForbiddenStatuses(t *testing.T) {
	for _, status := range []int{http.StatusNoContent, http.StatusResetContent, http.StatusNotModified} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			e := responseTestNewEngine(t)
			e.register(methodGet, "/responseTest", func(w http.ResponseWriter) string {
				w.WriteHeader(status)
				return "must not be sent"
			})
			e.generateRoute()
			rec, escaped := responseTestServe(t, e, http.MethodGet, "/responseTest", "", "")
			if escaped != nil {
				t.Fatalf("pipeline leaked a panic: %v", escaped)
			}
			responseTestCheckResponse(t, rec, status, "", textPlainCharset)
		})
	}
}

func TestResponsePipelineNoReturnPreservesContentType(t *testing.T) {
	e := responseTestNewEngine(t)
	e.register(methodGet, "/responseTest", func(w http.ResponseWriter) {
		w.Header().Set(contentType, "application/x-responseTest")
		w.WriteHeader(http.StatusCreated)
	})
	e.generateRoute()

	rec, escaped := responseTestServe(t, e, http.MethodGet, "/responseTest", "", "")
	if escaped != nil {
		t.Fatalf("pipeline leaked a panic: %v", escaped)
	}
	responseTestCheckResponse(t, rec, http.StatusCreated, "", "application/x-responseTest")
}

func TestResponsePipelineNoReturnUsesDefaultContentType(t *testing.T) {
	e := responseTestNewEngine(t)
	e.register(methodGet, "/responseTest", func(w http.ResponseWriter) {
		w.Header().Set(contentType, "application/x-responseTest")
		w.Header().Del(contentType)
	})
	e.generateRoute()
	rec, escaped := responseTestServe(t, e, http.MethodGet, "/responseTest", "", "")
	if escaped != nil {
		t.Fatalf("pipeline leaked a panic: %v", escaped)
	}
	responseTestCheckResponse(t, rec, http.StatusOK, "", textPlainCharset)
}

func TestResponsePipelineInterfaceResultFlushCommitsImmediately(t *testing.T) {
	e := responseTestNewEngine(t)
	sink := &responseTestFlushWriter{responseTestWriter: responseTestNewWriter()}
	e.register(methodGet, "/responseTest", func(w http.ResponseWriter) any {
		w.(http.Flusher).Flush()
		if sink.flushCalls != 1 {
			t.Errorf("Flush() did not commit before the handler returned: calls=%d", sink.flushCalls)
		}
		return "plain"
	})
	e.generateRoute()

	req := httptest.NewRequest(http.MethodGet, "/responseTest", nil)
	e.ServeHTTP(sink, req)
	if sink.finalStatus != http.StatusOK || sink.body.String() != "plain" {
		t.Fatalf("response = (%d, %q), want (200, plain)", sink.finalStatus, sink.body.String())
	}
	if got := sink.header.Get(contentType); got != empty {
		t.Fatalf("Content-Type after immediate flush = %q, want no provisional JSON type", got)
	}
}

func TestResponseBodyAllowedForStatus(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   bool
	}{
		{status: 99, want: false},
		{status: 100, want: false},
		{status: 199, want: false},
		{status: 200, want: true},
		{status: 204, want: false},
		{status: 205, want: false},
		{status: 304, want: false},
		{status: 999, want: true},
	} {
		if got := bodyAllowedForStatus(tc.status); got != tc.want {
			t.Errorf("bodyAllowedForStatus(%d) = %v, want %v", tc.status, got, tc.want)
		}
	}
}

func TestResponseForbiddenStatusSkipsHandlerSerialization(t *testing.T) {
	e := responseTestNewEngine(t)
	e.use(func(c *Context) {
		c.writer.WriteHeader(http.StatusNoContent)
		c.Next()
	})
	e.register(methodGet, "/no-content", func() responseTestPanicJSON { return responseTestPanicJSON{} })
	e.generateRoute()

	recorder, escaped := responseTestServe(t, e, http.MethodGet, "/no-content", empty, empty)
	if escaped != nil {
		t.Fatalf("response escaped panic: %v", escaped)
	}
	if recorder.Code != http.StatusNoContent || recorder.Body.Len() != 0 {
		t.Fatalf("response = %d %q, want 204 with empty body", recorder.Code, recorder.Body.String())
	}
}

func TestResponseWriterWriteHeaderTransitions(t *testing.T) {
	t.Run("informational then first final", func(t *testing.T) {
		sink := responseTestNewWriter()
		rw := &pooledResponseWriter{w: sink}
		rw.WriteHeader(http.StatusEarlyHints)
		rw.WriteHeader(http.StatusProcessing)
		if rw.headed() || rw.flushed() || rw.status() != http.StatusOK {
			t.Fatalf("informational state = (%v, %v, %d)", rw.headed(), rw.flushed(), rw.status())
		}
		rw.WriteHeader(http.StatusAccepted)
		rw.WriteHeader(http.StatusCreated)
		if !reflect.DeepEqual(sink.statuses, []int{103, 102}) {
			t.Fatalf("underlying statuses = %v, want [103 102]", sink.statuses)
		}
		if rw.status() != http.StatusAccepted || rw.headed() != true || rw.flushed() {
			t.Fatalf("staged state = (%d, %v, %v)", rw.status(), rw.headed(), rw.flushed())
		}
		rw.writeHeader()
		rw.writeHeader()
		if !reflect.DeepEqual(sink.statuses, []int{103, 102, 202}) {
			t.Fatalf("underlying statuses = %v, want [103 102 202]", sink.statuses)
		}
	})

	t.Run("switching protocols is immediate", func(t *testing.T) {
		sink := responseTestNewWriter()
		rw := &pooledResponseWriter{w: sink}
		rw.WriteHeader(http.StatusSwitchingProtocols)
		rw.WriteHeader(http.StatusOK)
		if !rw.headed() || !rw.flushed() || rw.status() != http.StatusSwitchingProtocols {
			t.Fatalf("101 state = (%v, %v, %d)", rw.headed(), rw.flushed(), rw.status())
		}
		if !reflect.DeepEqual(sink.statuses, []int{http.StatusSwitchingProtocols}) {
			t.Fatalf("underlying statuses = %v, want [101]", sink.statuses)
		}
	})

	t.Run("write commits implicit 200", func(t *testing.T) {
		sink := responseTestNewWriter()
		rw := &pooledResponseWriter{w: sink}
		if _, err := rw.Write([]byte("body")); err != nil {
			t.Fatalf("Write returned %v", err)
		}
		if rw.status() != http.StatusOK || !rw.headed() || !rw.flushed() || !rw.written() {
			t.Fatalf("implicit state = (%d, %v, %v, %v)", rw.status(), rw.headed(), rw.flushed(), rw.written())
		}
		if !reflect.DeepEqual(sink.statuses, []int{http.StatusOK}) {
			t.Fatalf("underlying statuses = %v, want [200]", sink.statuses)
		}
	})

	t.Run("invalid codes panic without recording", func(t *testing.T) {
		for _, code := range []int{0, 99, 1000} {
			sink := responseTestNewWriter()
			rw := &pooledResponseWriter{w: sink}
			got := responseTestCapturePanic(func() { rw.WriteHeader(code) })
			if got == nil || !strings.Contains(fmt.Sprint(got), "invalid WriteHeader code") {
				t.Errorf("WriteHeader(%d) panic = %v", code, got)
			}
			if rw.headed() || rw.flushed() || len(sink.statuses) != 0 {
				t.Errorf("WriteHeader(%d) changed state: statuses=%v", code, sink.statuses)
			}
		}
	})

	t.Run("invalid code after final is ignored", func(t *testing.T) {
		sink := responseTestNewWriter()
		rw := &pooledResponseWriter{w: sink}
		rw.WriteHeader(http.StatusNoContent)
		if got := responseTestCapturePanic(func() { rw.WriteHeader(99) }); got != nil {
			t.Fatalf("ignored WriteHeader panicked: %v", got)
		}
		if rw.status() != http.StatusNoContent {
			t.Fatalf("status = %d, want 204", rw.status())
		}
	})
}

func TestResponseWriterForbiddenBodies(t *testing.T) {
	for _, status := range []int{http.StatusNoContent, http.StatusResetContent, http.StatusNotModified} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			sink := responseTestNewWriter()
			rw := &pooledResponseWriter{w: sink}
			rw.WriteHeader(status)
			n, err := rw.Write([]byte("forbidden"))
			if n != 0 || !errors.Is(err, http.ErrBodyNotAllowed) {
				t.Fatalf("Write = (%d, %v), want (0, ErrBodyNotAllowed)", n, err)
			}
			if !rw.written() || !rw.flushed() || rw.size() != 0 || sink.body.Len() != 0 || sink.writeCalls != 0 {
				t.Fatalf("forbidden write state = (%v, %v, %d, %q, calls=%d)", rw.written(), rw.flushed(), rw.size(), sink.body.String(), sink.writeCalls)
			}
			if rw.status() != status {
				t.Fatalf("status = %d, want %d", rw.status(), status)
			}
		})
	}
}

func TestResponseWriterPartialWriteAndZeroLengthWrite(t *testing.T) {
	t.Run("partial write counts returned bytes", func(t *testing.T) {
		sink := responseTestNewWriter()
		sink.writeN = 3
		sink.writeErr = errors.New("responseTest short write")
		rw := &pooledResponseWriter{w: sink}
		n, err := rw.Write([]byte("abcdef"))
		if n != 3 || !errors.Is(err, sink.writeErr) || rw.size() != 3 || sink.body.String() != "abc" {
			t.Fatalf("partial Write = (%d, %v), size=%d body=%q", n, err, rw.size(), sink.body.String())
		}
	})

	t.Run("zero length write is still written", func(t *testing.T) {
		sink := responseTestNewWriter()
		rw := &pooledResponseWriter{w: sink}
		n, err := rw.Write(nil)
		if n != 0 || err != nil || !rw.written() || rw.size() != 0 || sink.writeCalls != 1 {
			t.Fatalf("zero Write = (%d, %v), written=%v size=%d calls=%d", n, err, rw.written(), rw.size(), sink.writeCalls)
		}
		if !reflect.DeepEqual(sink.statuses, []int{http.StatusOK}) {
			t.Fatalf("statuses = %v, want [200]", sink.statuses)
		}
	})
}

func TestResponseWriterReplaceStatusOnlyBeforeCommit(t *testing.T) {
	t.Run("staged status can be replaced", func(t *testing.T) {
		sink := responseTestNewWriter()
		rw := &pooledResponseWriter{w: sink}
		rw.WriteHeader(http.StatusCreated)
		rw.replaceStatus(http.StatusNoContent)
		rw.writeHeader()
		if rw.status() != http.StatusNoContent || !reflect.DeepEqual(sink.statuses, []int{204}) {
			t.Fatalf("replaced status = %d, underlying=%v", rw.status(), sink.statuses)
		}
	})

	for _, state := range []string{"flushed", "written", "hijacked"} {
		t.Run(state, func(t *testing.T) {
			sink := responseTestNewWriter()
			rw := &pooledResponseWriter{w: sink}
			switch state {
			case "flushed":
				rw.WriteHeader(http.StatusAccepted)
				rw.writeHeader()
			case "written":
				_, _ = rw.Write(nil)
			case "hijacked":
				rw.hijackedStatus = true
			}
			rw.replaceStatus(http.StatusInternalServerError)
			want := http.StatusOK
			if state == "flushed" {
				want = http.StatusAccepted
			}
			if rw.status() != want {
				t.Fatalf("status after replace = %d, want %d", rw.status(), want)
			}
			if state == "hijacked" {
				if len(sink.statuses) != 0 {
					t.Fatalf("hijacked response committed statuses: %v", sink.statuses)
				}
			} else if !reflect.DeepEqual(sink.statuses, []int{want}) {
				t.Fatalf("underlying statuses = %v, want [%d]", sink.statuses, want)
			}
		})
	}
}

func TestResponseWriterFlush(t *testing.T) {
	t.Run("flush error capability commits staged status", func(t *testing.T) {
		sink := responseTestNewWriter()
		stream := &responseTestFlushErrorWriter{responseTestWriter: sink}
		rw := &pooledResponseWriter{w: stream}
		rw.WriteHeader(http.StatusAccepted)
		if err := rw.FlushError(); err != nil {
			t.Fatalf("FlushError returned %v", err)
		}
		if stream.flushCalls != 1 || !rw.flushed() || rw.status() != http.StatusAccepted {
			t.Fatalf("flush state = (%d, %v, %d)", stream.flushCalls, rw.flushed(), rw.status())
		}
		if !reflect.DeepEqual(sink.statuses, []int{http.StatusAccepted}) {
			t.Fatalf("statuses = %v, want [202]", sink.statuses)
		}
	})

	t.Run("flush capability is found through unwrap", func(t *testing.T) {
		sink := responseTestNewWriter()
		stream := &responseTestFlushWriter{responseTestWriter: sink}
		wrapped := &responseTestUnwrapWriter{ResponseWriter: stream}
		rw := &pooledResponseWriter{w: wrapped}
		if err := rw.FlushError(); err != nil {
			t.Fatalf("FlushError returned %v", err)
		}
		if stream.flushCalls != 1 || !rw.flushed() || rw.status() != http.StatusOK {
			t.Fatalf("unwrapped flush state = (%d, %v, %d)", stream.flushCalls, rw.flushed(), rw.status())
		}
	})

	t.Run("unsupported flush does not commit", func(t *testing.T) {
		sink := responseTestNewWriter()
		rw := &pooledResponseWriter{w: sink}
		rw.WriteHeader(http.StatusAccepted)
		if err := rw.FlushError(); !errors.Is(err, http.ErrNotSupported) {
			t.Fatalf("FlushError = %v, want ErrNotSupported", err)
		}
		if rw.flushed() || len(sink.statuses) != 0 || !rw.headed() {
			t.Fatalf("unsupported flush committed response: statuses=%v", sink.statuses)
		}
	})

	t.Run("underlying flush error is propagated", func(t *testing.T) {
		flushErr := errors.New("responseTest flush failure")
		sink := responseTestNewWriter()
		stream := &responseTestFlushErrorWriter{responseTestWriter: sink, flushErr: flushErr}
		rw := &pooledResponseWriter{w: stream}
		rw.WriteHeader(http.StatusAccepted)
		if err := rw.FlushError(); !errors.Is(err, flushErr) {
			t.Fatalf("FlushError = %v, want %v", err, flushErr)
		}
		if !rw.flushed() || !reflect.DeepEqual(sink.statuses, []int{http.StatusAccepted}) {
			t.Fatalf("failed flush state = (%v, %v)", rw.flushed(), sink.statuses)
		}
	})

	t.Run("Flush ignores the underlying error", func(t *testing.T) {
		sink := responseTestNewWriter()
		stream := &responseTestFlushErrorWriter{responseTestWriter: sink, flushErr: errors.New("ignored")}
		rw := &pooledResponseWriter{w: stream}
		rw.WriteHeader(http.StatusCreated)
		rw.Flush()
		if stream.flushCalls != 1 || !rw.flushed() || rw.status() != http.StatusCreated {
			t.Fatalf("Flush state = (%d, %v, %d)", stream.flushCalls, rw.flushed(), rw.status())
		}
	})

	t.Run("hijacked flush is rejected", func(t *testing.T) {
		sink := responseTestNewWriter()
		rw := &pooledResponseWriter{w: sink, hijackedStatus: true}
		if err := rw.FlushError(); !errors.Is(err, http.ErrHijacked) {
			t.Fatalf("FlushError = %v, want ErrHijacked", err)
		}
		rw.Flush()
		if rw.flushed() || len(sink.statuses) != 0 {
			t.Fatalf("hijacked flush committed response: statuses=%v", sink.statuses)
		}
	})
}

func TestResponseWriterHijack(t *testing.T) {
	t.Run("success tracks and releases connection", func(t *testing.T) {
		conn := &responseTestConn{}
		buffered := bufio.NewReadWriter(bufio.NewReader(conn), bufio.NewWriter(conn))
		sink := responseTestNewWriter()
		hijacker := &responseTestHijackWriter{responseTestWriter: sink, conn: conn, buffered: buffered}
		registry := newHijackRegistry()
		rw := &pooledResponseWriter{w: hijacker, hijacks: registry}

		gotConn, gotBuffered, err := rw.Hijack()
		if err != nil || gotConn == nil || gotBuffered != buffered {
			t.Fatalf("Hijack = (%v, %p, %v)", gotConn, gotBuffered, err)
		}
		if !rw.hijacked() || rw.headed() || rw.flushed() || responseTestRegistrySize(registry) != 1 {
			t.Fatalf("hijack state = (%v, %v, %v), tracked=%d", rw.hijacked(), rw.headed(), rw.flushed(), responseTestRegistrySize(registry))
		}
		if hijacker.calls != 1 {
			t.Fatalf("underlying Hijack calls = %d, want 1", hijacker.calls)
		}
		if secondConn, secondBuffered, err := rw.Hijack(); secondConn != nil || secondBuffered != nil || !errors.Is(err, http.ErrHijacked) {
			t.Fatalf("second Hijack = (%v, %v, %v)", secondConn, secondBuffered, err)
		}
		if n, err := rw.Write([]byte("forbidden")); n != 0 || !errors.Is(err, http.ErrHijacked) {
			t.Fatalf("Write after Hijack = (%d, %v)", n, err)
		}
		rw.WriteHeader(http.StatusSwitchingProtocols)
		if err := rw.FlushError(); !errors.Is(err, http.ErrHijacked) {
			t.Fatalf("FlushError after Hijack = %v", err)
		}
		if len(sink.statuses) != 0 || rw.size() != 0 || rw.written() {
			t.Fatalf("hijacked response changed: statuses=%v size=%d written=%v", sink.statuses, rw.size(), rw.written())
		}
		if err := gotConn.Close(); err != nil {
			t.Fatalf("tracked Close returned %v", err)
		}
		if !conn.isClosed() || responseTestRegistrySize(registry) != 0 {
			t.Fatalf("Close state = closed:%v tracked:%d", conn.isClosed(), responseTestRegistrySize(registry))
		}
	})

	t.Run("underlying failure leaves writer usable", func(t *testing.T) {
		hijackErr := errors.New("responseTest hijack failure")
		sink := responseTestNewWriter()
		hijacker := &responseTestHijackWriter{responseTestWriter: sink, err: hijackErr}
		rw := &pooledResponseWriter{w: hijacker}
		conn, buffered, err := rw.Hijack()
		if conn != nil || buffered != nil || !errors.Is(err, hijackErr) || rw.hijacked() {
			t.Fatalf("failed Hijack = (%v, %v, %v), hijacked=%v", conn, buffered, err, rw.hijacked())
		}
		if _, err := rw.Write([]byte("still usable")); err != nil {
			t.Fatalf("Write after failed Hijack returned %v", err)
		}
	})

	t.Run("unsupported hijack", func(t *testing.T) {
		rw := &pooledResponseWriter{w: responseTestNewWriter()}
		conn, buffered, err := rw.Hijack()
		if conn != nil || buffered != nil || !errors.Is(err, http.ErrNotSupported) || rw.hijacked() {
			t.Fatalf("unsupported Hijack = (%v, %v, %v), hijacked=%v", conn, buffered, err, rw.hijacked())
		}
	})

	t.Run("closed registry rejects and closes new connection", func(t *testing.T) {
		conn := &responseTestConn{}
		sink := responseTestNewWriter()
		hijacker := &responseTestHijackWriter{responseTestWriter: sink, conn: conn}
		registry := newHijackRegistry()
		registry.closeAll()
		rw := &pooledResponseWriter{w: hijacker, hijacks: registry}
		gotConn, buffered, err := rw.Hijack()
		if gotConn != nil || buffered != nil || !errors.Is(err, http.ErrServerClosed) || !rw.hijacked() {
			t.Fatalf("closed-registry Hijack = (%v, %v, %v), hijacked=%v", gotConn, buffered, err, rw.hijacked())
		}
		if !conn.isClosed() {
			t.Fatal("rejected connection was not closed")
		}
	})

	t.Run("registry shutdown closes tracked connections", func(t *testing.T) {
		conn := &responseTestConn{}
		sink := responseTestNewWriter()
		hijacker := &responseTestHijackWriter{responseTestWriter: sink, conn: conn}
		registry := newHijackRegistry()
		rw := &pooledResponseWriter{w: hijacker, hijacks: registry}
		tracked, _, err := rw.Hijack()
		if err != nil {
			t.Fatalf("Hijack returned %v", err)
		}
		registry.closeAll()
		if !conn.isClosed() || responseTestRegistrySize(registry) != 0 {
			t.Fatalf("shutdown state = closed:%v tracked:%d", conn.isClosed(), responseTestRegistrySize(registry))
		}
		_ = tracked.Close()
	})
}

func TestResponseWriterPush(t *testing.T) {
	t.Run("forwards target and options through unwrap", func(t *testing.T) {
		sink := responseTestNewWriter()
		pusher := &responseTestPushWriter{responseTestWriter: sink}
		wrapped := &responseTestUnwrapWriter{ResponseWriter: pusher}
		rw := &pooledResponseWriter{w: wrapped}
		opts := &http.PushOptions{Method: http.MethodGet, Header: http.Header{"X-responseTest": []string{"yes"}}}
		if err := rw.Push("/asset.css", opts); err != nil {
			t.Fatalf("Push returned %v", err)
		}
		if !reflect.DeepEqual(pusher.targets, []string{"/asset.css"}) || len(pusher.options) != 1 || pusher.options[0] != opts {
			t.Fatalf("Push arguments = %v, %v", pusher.targets, pusher.options)
		}
		if rw.headed() || rw.flushed() || rw.written() {
			t.Fatal("Push changed response state")
		}
	})

	t.Run("propagates pusher error", func(t *testing.T) {
		pushErr := errors.New("responseTest push failure")
		pusher := &responseTestPushWriter{responseTestWriter: responseTestNewWriter(), err: pushErr}
		rw := &pooledResponseWriter{w: pusher}
		if err := rw.Push("/asset", nil); !errors.Is(err, pushErr) {
			t.Fatalf("Push = %v, want %v", err, pushErr)
		}
	})

	t.Run("unsupported push", func(t *testing.T) {
		rw := &pooledResponseWriter{w: responseTestNewWriter()}
		if err := rw.Push("/asset", nil); !errors.Is(err, http.ErrNotSupported) {
			t.Fatalf("Push = %v, want ErrNotSupported", err)
		}
	})
}

func TestResponseWriterUnwrapAndResponseController(t *testing.T) {
	sink := responseTestNewWriter()
	controllerWriter := &responseTestControllerWriter{responseTestWriter: sink}
	rw := &pooledResponseWriter{w: controllerWriter}
	if got := rw.Unwrap(); got != controllerWriter {
		t.Fatalf("Unwrap = %v, want original writer", got)
	}

	deadline := time.Now().Add(responseTestTimeout).Round(0)
	controller := http.NewResponseController(rw)
	if err := controller.SetWriteDeadline(deadline); err != nil {
		t.Fatalf("SetWriteDeadline returned %v", err)
	}
	if err := controller.SetReadDeadline(deadline); err != nil {
		t.Fatalf("SetReadDeadline returned %v", err)
	}
	if err := controller.EnableFullDuplex(); err != nil {
		t.Fatalf("EnableFullDuplex returned %v", err)
	}
	if !controllerWriter.writeDeadline.Equal(deadline) || !controllerWriter.readDeadline.Equal(deadline) || !controllerWriter.fullDuplex {
		t.Fatalf("controller capabilities were not forwarded: %+v", controllerWriter)
	}

	controllerWriter.err = errors.New("responseTest controller failure")
	if err := controller.SetWriteDeadline(deadline); !errors.Is(err, controllerWriter.err) {
		t.Fatalf("SetWriteDeadline = %v, want underlying error", err)
	}

	basic := &pooledResponseWriter{w: responseTestNewWriter()}
	if err := http.NewResponseController(basic).SetWriteDeadline(deadline); !errors.Is(err, http.ErrNotSupported) {
		t.Fatalf("unsupported SetWriteDeadline = %v, want ErrNotSupported", err)
	}
}

func TestResponseWriterHeaderIsLive(t *testing.T) {
	sink := responseTestNewWriter()
	rw := &pooledResponseWriter{w: sink}
	rw.Header().Set("X-responseTest", "live")
	if got := sink.header.Get("X-responseTest"); got != "live" {
		t.Fatalf("underlying header = %q, want live", got)
	}
}

func TestResponseWriterRecycleClearsState(t *testing.T) {
	rw := &pooledResponseWriter{
		w:              responseTestNewWriter(),
		hijacks:        newHijackRegistry(),
		statusCode:     http.StatusCreated,
		bytesWritten:   12,
		writtenStatus:  true,
		headedStatus:   true,
		flushedStatus:  true,
		hijackedStatus: true,
	}
	rw.recycle()
	if *rw != (pooledResponseWriter{}) {
		t.Fatalf("recycle retained writer state: %+v", rw)
	}
}

func TestResponseErrorWriteback(t *testing.T) {
	integer := 42
	var nilPointer *int
	var nilMap map[string]int
	var nilSlice []int
	for _, tc := range []struct {
		name      string
		value     any
		body      string
		mediaType string
	}{
		{name: "nil", value: nil, body: "", mediaType: textPlainCharset},
		{name: "error", value: errors.New("error message"), body: "error message", mediaType: textPlainCharset},
		{name: "string", value: "text", body: "text", mediaType: textPlainCharset},
		{name: "bool", value: true, body: "true", mediaType: textPlainCharset},
		{name: "int", value: int(-42), body: "-42", mediaType: textPlainCharset},
		{name: "int8", value: int8(-8), body: "-8", mediaType: textPlainCharset},
		{name: "int16", value: int16(-16), body: "-16", mediaType: textPlainCharset},
		{name: "int32", value: int32(-32), body: "-32", mediaType: textPlainCharset},
		{name: "int64", value: int64(-64), body: "-64", mediaType: textPlainCharset},
		{name: "uint", value: uint(42), body: "42", mediaType: textPlainCharset},
		{name: "uint8", value: uint8(8), body: "8", mediaType: textPlainCharset},
		{name: "uint16", value: uint16(16), body: "16", mediaType: textPlainCharset},
		{name: "uint32", value: uint32(32), body: "32", mediaType: textPlainCharset},
		{name: "uint64", value: uint64(64), body: "64", mediaType: textPlainCharset},
		{name: "uintptr", value: uintptr(9), body: "9", mediaType: textPlainCharset},
		{name: "float32", value: float32(1.25), body: "1.25", mediaType: textPlainCharset},
		{name: "float64", value: float64(-2.5), body: "-2.5", mediaType: textPlainCharset},
		{name: "complex64", value: complex64(complex(1, 2)), body: "(1+2i)", mediaType: textPlainCharset},
		{name: "complex128", value: complex(1, -2), body: "(1-2i)", mediaType: textPlainCharset},
		{name: "pointer scalar", value: &integer, body: "42", mediaType: textPlainCharset},
		{name: "struct", value: responseTestPayload{Name: "alpha", Count: 2}, body: `{"name":"alpha","count":2}`, mediaType: applicationJsonCharset},
		{name: "map", value: map[string]int{"a": 1}, body: `{"a":1}`, mediaType: applicationJsonCharset},
		{name: "array", value: [2]int{1, 2}, body: "[1,2]", mediaType: applicationJsonCharset},
		{name: "slice", value: []string{"a"}, body: `["a"]`, mediaType: applicationJsonCharset},
		{name: "bytes are base64 JSON", value: []byte("bytes"), body: `"Ynl0ZXM="`, mediaType: applicationJsonCharset},
		{name: "nil pointer", value: nilPointer, body: "", mediaType: textPlainCharset},
		{name: "nil map", value: nilMap, body: "null", mediaType: applicationJsonCharset},
		{name: "nil slice", value: nilSlice, body: "null", mediaType: applicationJsonCharset},
		{name: "channel", value: make(chan int), body: string(typeRecognitionError), mediaType: textPlainCharset},
		{name: "function", value: func() {}, body: string(typeRecognitionError), mediaType: textPlainCharset},
		{name: "cyclic map", value: responseTestCyclicMap(), body: string(typeRecognitionError), mediaType: textPlainCharset},
		{name: "NaN", value: []float64{math.NaN()}, body: string(typeRecognitionError), mediaType: textPlainCharset},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, mediaType := errorWriteback(tc.value)
			if string(body) != tc.body || mediaType != tc.mediaType {
				t.Fatalf("errorWriteback = (%q, %q), want (%q, %q)", body, mediaType, tc.body, tc.mediaType)
			}
		})
	}
}

func TestResponseErrorWritebackDepthLimit(t *testing.T) {
	body, mediaType := errorWriteback(responseTestTypedPointerChain(maxResponseDerefDepth).Interface())
	if string(body) != "ok" || mediaType != textPlainCharset {
		t.Fatalf("depth %d = (%q, %q), want ok text", maxResponseDerefDepth, body, mediaType)
	}
	body, mediaType = errorWriteback(responseTestTypedPointerChain(maxResponseDerefDepth + 1).Interface())
	if string(body) != string(typeRecognitionError) || mediaType != textPlainCharset {
		t.Fatalf("excessive depth = (%q, %q), want fixed fallback", body, mediaType)
	}
}

func TestResponseErrorWritebackTypedNilError(t *testing.T) {
	var value *responseTestNilError
	var escaped any
	var body []byte
	var mediaType string
	escaped = responseTestCapturePanic(func() {
		body, mediaType = errorWriteback(value)
	})
	if escaped != nil {
		t.Errorf("typed-nil error rendering panicked: %v", escaped)
	}
	if string(body) != empty || mediaType != textPlainCharset {
		t.Errorf("typed-nil error rendering = (%q, %q), want empty text", body, mediaType)
	}
}

func TestResponseErrorWritebackPanickingError(t *testing.T) {
	var escaped any
	var body []byte
	var mediaType string
	escaped = responseTestCapturePanic(func() {
		body, mediaType = errorWriteback(responseTestPanicError{})
	})
	if escaped != nil {
		t.Errorf("panicking Error method escaped rendering: %v", escaped)
	}
	if string(body) != string(typeRecognitionError) || mediaType != textPlainCharset {
		t.Errorf("panicking Error rendering = (%q, %q), want fixed text fallback", body, mediaType)
	}
}

func TestResponseCatchRuntimePanicWithoutPanic(t *testing.T) {
	e := responseTestNewEngine(t)
	sink := responseTestNewWriter()
	rw := &pooledResponseWriter{w: sink}
	escaped := responseTestCatchPanic(e, rw, func() {})
	if escaped != nil || len(sink.statuses) != 0 || rw.headed() || rw.flushed() {
		t.Fatalf("no-panic recovery changed response: escaped=%v statuses=%v", escaped, sink.statuses)
	}
}

func TestResponseCatchRuntimePanicAbort(t *testing.T) {
	e := responseTestNewEngine(t)
	logger := &responseTestLogger{}
	e.setLogger(logger)
	sink := responseTestNewWriter()
	rw := &pooledResponseWriter{w: sink}
	escaped := responseTestCatchPanic(e, rw, func() {
		abort(http.StatusForbidden, "forbidden")
	})
	if escaped != nil {
		t.Fatalf("response abort escaped: %v", escaped)
	}
	if rw.status() != http.StatusForbidden || sink.body.String() != "forbidden" || sink.header.Get(contentType) != textPlainCharset {
		t.Fatalf("abort response = (%d, %q, %q)", rw.status(), sink.body.String(), sink.header.Get(contentType))
	}
	if entries := logger.snapshot(); len(entries) != 0 {
		t.Fatalf("response abort was logged as a crash: %+v", entries)
	}
}

func TestResponseCatchRuntimePanicAbortAfterPriorState(t *testing.T) {
	for _, tc := range []struct {
		name       string
		prepare    func(*pooledResponseWriter, *responseTestWriter)
		wantStatus int
		wantBody   string
	}{
		{
			name: "staged status",
			prepare: func(rw *pooledResponseWriter, sink *responseTestWriter) {
				rw.WriteHeader(http.StatusAccepted)
				sink.header.Set(contentType, "application/x-responseTest")
			},
			wantStatus: http.StatusAccepted,
		},
		{
			name: "written body",
			prepare: func(rw *pooledResponseWriter, _ *responseTestWriter) {
				rw.WriteHeader(http.StatusAccepted)
				_, _ = rw.Write([]byte("partial"))
			},
			wantStatus: http.StatusAccepted,
			wantBody:   "partial",
		},
		{
			name: "flushed without body",
			prepare: func(rw *pooledResponseWriter, _ *responseTestWriter) {
				rw.WriteHeader(http.StatusNoContent)
				rw.writeHeader()
			},
			wantStatus: http.StatusNoContent,
		},
		{
			name: "hijacked",
			prepare: func(rw *pooledResponseWriter, _ *responseTestWriter) {
				rw.hijackedStatus = true
			},
			wantStatus: http.StatusOK,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := responseTestNewEngine(t)
			logger := &responseTestLogger{}
			e.setLogger(logger)
			sink := responseTestNewWriter()
			rw := &pooledResponseWriter{w: sink}
			tc.prepare(rw, sink)
			escaped := responseTestCatchPanic(e, rw, func() {
				abort(http.StatusConflict, "replacement")
			})
			if escaped != nil {
				t.Fatalf("response abort escaped: %v", escaped)
			}
			if rw.status() != tc.wantStatus || sink.body.String() != tc.wantBody {
				t.Fatalf("response = (%d, %q), want (%d, %q)", rw.status(), sink.body.String(), tc.wantStatus, tc.wantBody)
			}
			if entries := logger.snapshot(); len(entries) != 0 {
				t.Fatalf("response abort was logged as a crash: %+v", entries)
			}
		})
	}
}

func TestResponseCatchRuntimePanicUnexpectedDoesNotLeak(t *testing.T) {
	e := responseTestNewEngine(t)
	logger := &responseTestLogger{}
	e.setLogger(logger)
	sink := responseTestNewWriter()
	rw := &pooledResponseWriter{w: sink}
	rw.WriteHeader(http.StatusCreated)
	secret := errors.New("ctxTest internal secret")
	escaped := responseTestCatchPanic(e, rw, func() { panic(secret) })
	if escaped != nil {
		t.Fatalf("unexpected panic escaped: %v", escaped)
	}
	if rw.status() != http.StatusInternalServerError || sink.body.String() != http.StatusText(http.StatusInternalServerError) {
		t.Fatalf("panic response = (%d, %q)", rw.status(), sink.body.String())
	}
	if got := sink.header.Get(contentType); got != textPlainCharset {
		t.Fatalf("Content-Type = %q, want %q", got, textPlainCharset)
	}
	if strings.Contains(sink.body.String(), "ctxTest internal secret") {
		t.Fatal("panic detail leaked into the response")
	}
	entries := logger.snapshot()
	if len(entries) != 1 || entries[0].msg != "recovered panic" || len(entries[0].kv) != 4 {
		t.Fatalf("log entries = %+v, want one structured recovered-panic entry", entries)
	}
	if entries[0].kv[0] != "error" || entries[0].kv[1] != secret || entries[0].kv[2] != "stack" {
		t.Fatalf("log fields = %v", entries[0].kv)
	}
	stack, ok := entries[0].kv[3].(string)
	if !ok || !strings.Contains(stack, "goroutine") || !strings.Contains(stack, "TestResponseCatchRuntimePanicUnexpectedDoesNotLeak") {
		t.Fatalf("stack log = %v", entries[0].kv[3])
	}
}

func TestResponseCatchRuntimePanicAfterCommitOnlyLogs(t *testing.T) {
	for _, state := range []string{"written", "flushed", "hijacked"} {
		t.Run(state, func(t *testing.T) {
			e := responseTestNewEngine(t)
			logger := &responseTestLogger{}
			e.setLogger(logger)
			sink := responseTestNewWriter()
			rw := &pooledResponseWriter{w: sink}
			switch state {
			case "written":
				_, _ = rw.Write([]byte("partial"))
			case "flushed":
				rw.WriteHeader(http.StatusNoContent)
				rw.writeHeader()
			case "hijacked":
				rw.hijackedStatus = true
			}
			secret := errors.New("ctxTest committed panic secret")
			escaped := responseTestCatchPanic(e, rw, func() { panic(secret) })
			if escaped != nil {
				t.Fatalf("committed panic escaped: %v", escaped)
			}
			wantBody := ""
			if state == "written" {
				wantBody = "partial"
			}
			if sink.body.String() != wantBody {
				t.Fatalf("body = %q, want %q", sink.body.String(), wantBody)
			}
			if strings.Contains(sink.body.String(), "committed panic secret") {
				t.Fatal("committed panic detail leaked into the response")
			}
			entries := logger.snapshot()
			if len(entries) != 1 || entries[0].msg != "recovered panic" || len(entries[0].kv) < 2 || entries[0].kv[1] != secret {
				t.Fatalf("log entries = %+v", entries)
			}
		})
	}
}

func TestResponseRecoveredPanicSurvivesLoggerPanic(t *testing.T) {
	e := responseTestNewEngine(t)
	e.logger = &logTestPanicLogger{}
	sink := responseTestNewWriter()
	rw := &pooledResponseWriter{w: sink}
	escaped := responseTestCatchPanic(e, rw, func() { panic("original panic") })
	if escaped != nil {
		t.Fatalf("recovery leaked a panic: %v", escaped)
	}
	if rw.status() != http.StatusInternalServerError || sink.body.String() != http.StatusText(http.StatusInternalServerError) {
		t.Fatalf("response = (%d, %q), want generic 500", rw.status(), sink.body.String())
	}
}

func TestResponsePanicProcessorPanicIsContained(t *testing.T) {
	e := responseTestNewEngine(t)
	e.setLogger(&responseTestLogger{})
	e.setPanicProcessor(func(any) (any, HttpStatus) { panic("processor boom") })
	sink := responseTestNewWriter()
	rw := &pooledResponseWriter{w: sink}
	escaped := responseTestCatchPanic(e, rw, func() { panic("original panic") })
	if escaped != nil {
		t.Fatalf("processor panic escaped: %v", escaped)
	}
	if rw.status() != http.StatusInternalServerError || sink.body.String() != http.StatusText(http.StatusInternalServerError) {
		t.Fatalf("response = (%d, %q), want generic 500", rw.status(), sink.body.String())
	}
}

func TestResponsePanicProcessorPayloadPanicIsContained(t *testing.T) {
	e := responseTestNewEngine(t)
	e.setLogger(&responseTestLogger{})
	e.setPanicProcessor(func(any) (any, HttpStatus) {
		return responseTestPanicJSON{}, HttpStatus(http.StatusBadRequest)
	})
	sink := responseTestNewWriter()
	rw := &pooledResponseWriter{w: sink}
	escaped := responseTestCatchPanic(e, rw, func() { panic("original panic") })
	if escaped != nil {
		t.Fatalf("payload rendering panic escaped: %v", escaped)
	}
	if rw.status() != http.StatusInternalServerError || sink.body.String() != http.StatusText(http.StatusInternalServerError) {
		t.Fatalf("response = (%d, %q), want generic 500", rw.status(), sink.body.String())
	}
}

func TestWriteGeneric500DoesNotRewriteCommittedResponse(t *testing.T) {
	for _, state := range []string{"written", "flushed", "hijacked"} {
		t.Run(state, func(t *testing.T) {
			sink := responseTestNewWriter()
			rw := &pooledResponseWriter{w: sink}
			switch state {
			case "written":
				_, _ = rw.Write([]byte("partial"))
			case "flushed":
				rw.WriteHeader(http.StatusNoContent)
				rw.writeHeader()
			case "hijacked":
				rw.hijackedStatus = true
			}
			statuses := append([]int(nil), sink.statuses...)
			body := sink.body.String()
			writeGeneric500(rw)
			if !reflect.DeepEqual(sink.statuses, statuses) || sink.body.String() != body {
				t.Fatalf("committed response changed: statuses=%v body=%q", sink.statuses, sink.body.String())
			}
		})
	}
}

func TestResponsePipelineAbortAfterNextSuppressesStagedResult(t *testing.T) {
	e := responseTestNewEngine(t)
	var result any
	e.use(func(c *Context) {
		c.Next()
		result = c.Result()
		c.Abort()
	})
	e.register(methodGet, "/responseTest", func() string { return "staged" })
	e.generateRoute()
	rec, escaped := responseTestServe(t, e, http.MethodGet, "/responseTest", "", "")
	if escaped != nil {
		t.Fatalf("pipeline leaked a panic: %v", escaped)
	}
	if result != "staged" {
		t.Fatalf("Result() = %#v, want staged", result)
	}
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("response = (%d, %q), want empty 200", rec.Code, rec.Body.String())
	}
}

func TestResponsePipelineAbortAfterNextCommitsStagedStatus(t *testing.T) {
	e := responseTestNewEngine(t)
	e.use(func(c *Context) {
		c.Next()
		c.writer.WriteHeader(http.StatusCreated)
		c.Abort()
	})
	e.register(methodGet, "/responseTest", func() string { return "suppressed" })
	e.generateRoute()
	rec, escaped := responseTestServe(t, e, http.MethodGet, "/responseTest", "", "")
	if escaped != nil {
		t.Fatalf("pipeline leaked a panic: %v", escaped)
	}
	if rec.Code != http.StatusCreated || rec.Body.Len() != 0 {
		t.Fatalf("response = (%d, %q), want empty 201", rec.Code, rec.Body.String())
	}
}

func TestErrorMediaTypeDepth(t *testing.T) {
	var nilPayload *responseTestPayload
	var deep any = responseTestPayload{}
	for range maxResponseDerefDepth + 1 {
		deep = &deep
	}
	tests := []struct {
		name    string
		payload any
		want    string
	}{
		{name: "nil", payload: nil, want: textPlainCharset},
		{name: "error", payload: errors.New("boom"), want: textPlainCharset},
		{name: "struct", payload: responseTestPayload{}, want: applicationJsonCharset},
		{name: "map", payload: map[string]int{"a": 1}, want: applicationJsonCharset},
		{name: "slice", payload: []int{1}, want: applicationJsonCharset},
		{name: "array", payload: [1]int{1}, want: applicationJsonCharset},
		{name: "string", payload: "text", want: textPlainCharset},
		{name: "pointer", payload: &responseTestPayload{}, want: applicationJsonCharset},
		{name: "nil pointer", payload: nilPayload, want: textPlainCharset},
		{name: "depth fallback", payload: deep, want: textPlainCharset},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := errorMediaType(tt.payload); got != tt.want {
				t.Fatalf("errorMediaType(%T) = %q, want %q", tt.payload, got, tt.want)
			}
		})
	}
}

func TestResponsePanicProcessor(t *testing.T) {
	for _, tc := range []struct {
		name      string
		payload   any
		status    HttpStatus
		body      string
		mediaType string
	}{
		{name: "string", payload: "denied", status: HttpStatus(http.StatusTeapot), body: "denied", mediaType: textPlainCharset},
		{name: "error", payload: errors.New("processor error"), status: HttpStatus(http.StatusConflict), body: "processor error", mediaType: textPlainCharset},
		{name: "struct", payload: responseTestPayload{Name: "alpha", Count: 2}, status: HttpStatus(http.StatusBadRequest), body: `{"name":"alpha","count":2}`, mediaType: applicationJsonCharset},
		{name: "map", payload: map[string]int{"a": 1}, status: HttpStatus(http.StatusOK), body: `{"a":1}`, mediaType: applicationJsonCharset},
		{name: "nil", payload: nil, status: HttpStatus(http.StatusNoContent), body: "", mediaType: textPlainCharset},
		{name: "no content", payload: "hidden", status: HttpStatus(http.StatusNoContent), body: "", mediaType: textPlainCharset},
		{name: "reset content", payload: "hidden", status: HttpStatus(http.StatusResetContent), body: "", mediaType: textPlainCharset},
		{name: "not modified", payload: "hidden", status: HttpStatus(http.StatusNotModified), body: "", mediaType: textPlainCharset},
		{name: "no content skips bad JSON", payload: responseTestPanicJSON{}, status: HttpStatus(http.StatusNoContent), body: "", mediaType: applicationJsonCharset},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := responseTestNewEngine(t)
			calls := 0
			var recovered any
			e.setPanicProcessor(func(value any) (any, HttpStatus) {
				calls++
				recovered = value
				return tc.payload, tc.status
			})
			sink := responseTestNewWriter()
			rw := &pooledResponseWriter{w: sink}
			rw.WriteHeader(http.StatusCreated)
			original := errors.New("ctxTest original panic")
			escaped := responseTestCatchPanic(e, rw, func() { panic(original) })
			if escaped != nil {
				t.Fatalf("processor panic escaped: %v", escaped)
			}
			if calls != 1 || recovered != original {
				t.Fatalf("processor calls/input = (%d, %v)", calls, recovered)
			}
			if rw.status() != int(tc.status) || sink.body.String() != tc.body || sink.header.Get(contentType) != tc.mediaType {
				t.Fatalf("processor response = (%d, %q, %q)", rw.status(), sink.body.String(), sink.header.Get(contentType))
			}
		})
	}
}

func TestResponsePanicProcessorSanitizesStatus(t *testing.T) {
	for _, status := range []HttpStatus{0, 99, 100, 199, 200, 999, 1000} {
		t.Run(fmt.Sprint(int(status)), func(t *testing.T) {
			e := responseTestNewEngine(t)
			e.setPanicProcessor(func(any) (any, HttpStatus) { return "processor", status })
			sink := responseTestNewWriter()
			rw := &pooledResponseWriter{w: sink}
			escaped := responseTestCatchPanic(e, rw, func() { panic("original") })
			if escaped != nil {
				t.Fatalf("processor panic escaped: %v", escaped)
			}
			want := int(status)
			if want < http.StatusOK || want > 999 {
				want = http.StatusInternalServerError
			}
			if rw.status() != want || sink.body.String() != "processor" {
				t.Fatalf("response = (%d, %q), want status %d", rw.status(), sink.body.String(), want)
			}
		})
	}
}

func TestResponsePanicProcessorOwnsFrameworkAborts(t *testing.T) {
	e := responseTestNewEngine(t)
	var recovered any
	e.setPanicProcessor(func(value any) (any, HttpStatus) {
		recovered = value
		return "processor abort", HttpStatus(http.StatusConflict)
	})
	sink := responseTestNewWriter()
	rw := &pooledResponseWriter{w: sink}
	halt := responseAbort{status: http.StatusForbidden, body: "framework halt"}
	escaped := responseTestCatchPanic(e, rw, func() { panic(halt) })
	if escaped != nil || recovered != halt {
		t.Fatalf("processor input/escape = (%v, %v), want original responseAbort", recovered, escaped)
	}
	if rw.status() != http.StatusConflict || sink.body.String() != "processor abort" {
		t.Fatalf("processor response = (%d, %q)", rw.status(), sink.body.String())
	}
}

func TestResponsePanicProcessorSkippedAfterCommit(t *testing.T) {
	for _, state := range []string{"written", "flushed", "hijacked"} {
		t.Run(state, func(t *testing.T) {
			e := responseTestNewEngine(t)
			logger := &responseTestLogger{}
			e.setLogger(logger)
			calls := 0
			e.setPanicProcessor(func(any) (any, HttpStatus) {
				calls++
				return "replacement", HttpStatus(http.StatusTeapot)
			})
			sink := responseTestNewWriter()
			rw := &pooledResponseWriter{w: sink}
			switch state {
			case "written":
				_, _ = rw.Write([]byte("partial"))
			case "flushed":
				rw.WriteHeader(http.StatusNoContent)
				rw.writeHeader()
			case "hijacked":
				rw.hijackedStatus = true
			}
			escaped := responseTestCatchPanic(e, rw, func() { panic("ctxTest committed panic") })
			if escaped != nil || calls != 0 {
				t.Fatalf("committed processor escape/calls = (%v, %d)", escaped, calls)
			}
			if entries := logger.snapshot(); len(entries) != 1 {
				t.Fatalf("committed panic log entries = %+v, want one", entries)
			}
		})
	}
}

func TestResponsePipelinePanicHandling(t *testing.T) {
	t.Run("unexpected panic is generic and logged", func(t *testing.T) {
		e := responseTestNewEngine(t)
		logger := &responseTestLogger{}
		e.setLogger(logger)
		e.register(methodGet, "/responseTest", func() string {
			panic("ctxTest pipeline secret")
		})
		e.generateRoute()
		rec, escaped := responseTestServe(t, e, http.MethodGet, "/responseTest", "", "")
		if escaped != nil {
			t.Fatalf("pipeline leaked a panic: %v", escaped)
		}
		responseTestCheckResponse(t, rec, http.StatusInternalServerError, http.StatusText(http.StatusInternalServerError), textPlainCharset)
		if strings.Contains(rec.Body.String(), "ctxTest pipeline secret") {
			t.Fatal("pipeline panic leaked into the response")
		}
		if entries := logger.snapshot(); len(entries) != 1 || entries[0].msg != "recovered panic" {
			t.Fatalf("log entries = %+v", entries)
		}
	})

	t.Run("staged status is replaced before commit", func(t *testing.T) {
		e := responseTestNewEngine(t)
		logger := &responseTestLogger{}
		e.setLogger(logger)
		e.use(func(c *Context) {
			c.writer.WriteHeader(http.StatusCreated)
			panic("ctxTest staged panic")
		})
		e.register(methodGet, "/responseTest", func() string { return "unreachable" })
		e.generateRoute()
		rec, escaped := responseTestServe(t, e, http.MethodGet, "/responseTest", "", "")
		if escaped != nil {
			t.Fatalf("pipeline leaked a panic: %v", escaped)
		}
		responseTestCheckResponse(t, rec, http.StatusInternalServerError, http.StatusText(http.StatusInternalServerError), textPlainCharset)
	})

	t.Run("written response is preserved", func(t *testing.T) {
		e := responseTestNewEngine(t)
		logger := &responseTestLogger{}
		e.setLogger(logger)
		e.register(methodGet, "/responseTest", func(w http.ResponseWriter) string {
			w.Header().Set(contentType, "application/x-responseTest")
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte("partial"))
			panic("ctxTest after-write secret")
		})
		e.generateRoute()
		rec, escaped := responseTestServe(t, e, http.MethodGet, "/responseTest", "", "")
		if escaped != nil {
			t.Fatalf("pipeline leaked a panic: %v", escaped)
		}
		responseTestCheckResponse(t, rec, http.StatusAccepted, "partial", "application/x-responseTest")
		if entries := logger.snapshot(); len(entries) != 1 {
			t.Fatalf("log entries = %+v, want one", entries)
		}
	})

	t.Run("processor renders recovered panic", func(t *testing.T) {
		e := responseTestNewEngine(t)
		e.setPanicProcessor(func(value any) (any, HttpStatus) {
			if value != "ctxTest processor input" {
				t.Errorf("processor input = %v", value)
			}
			return map[string]string{"error": "handled"}, HttpStatus(http.StatusConflict)
		})
		e.register(methodGet, "/responseTest", func() string {
			panic("ctxTest processor input")
		})
		e.generateRoute()
		rec, escaped := responseTestServe(t, e, http.MethodGet, "/responseTest", "", "")
		if escaped != nil {
			t.Fatalf("pipeline leaked a panic: %v", escaped)
		}
		responseTestCheckResponse(t, rec, http.StatusConflict, `{"error":"handled"}`, applicationJsonCharset)
	})
}

func TestResponsePipelineFrameworkAbort(t *testing.T) {
	e := responseTestNewEngine(t)
	logger := &responseTestLogger{}
	e.setLogger(logger)
	e.register(methodGet, "/responseTest", func() string {
		abort(http.StatusNotFound, "ctxTest safe halt")
		return "unreachable"
	})
	e.generateRoute()
	rec, escaped := responseTestServe(t, e, http.MethodGet, "/responseTest", "", "")
	if escaped != nil {
		t.Fatalf("pipeline leaked a panic: %v", escaped)
	}
	responseTestCheckResponse(t, rec, http.StatusNotFound, "ctxTest safe halt", textPlainCharset)
	if entries := logger.snapshot(); len(entries) != 0 {
		t.Fatalf("framework abort was logged as a crash: %+v", entries)
	}
}

func TestResponsePipelineUnsupportedFlushIsContained(t *testing.T) {
	e := responseTestNewEngine(t)
	e.setLogger(&responseTestLogger{})
	e.register(methodGet, "/responseTest", func(w http.ResponseWriter) string {
		if err := http.NewResponseController(w).Flush(); err != nil {
			panic(err)
		}
		return "unreachable"
	})
	e.generateRoute()

	sink := responseTestNewWriter()
	request := httptest.NewRequest(http.MethodGet, "/responseTest", nil)
	escaped := responseTestServeWriter(t, e, sink, request)
	if escaped != nil {
		t.Fatalf("pipeline leaked a panic: %v", escaped)
	}
	if sink.finalStatus != http.StatusInternalServerError || sink.body.String() != http.StatusText(http.StatusInternalServerError) {
		t.Fatalf("unsupported-flush response = (%d, %q)", sink.finalStatus, sink.body.String())
	}
	if got := sink.header.Get(contentType); got != textPlainCharset {
		t.Fatalf("Content-Type = %q, want %q", got, textPlainCharset)
	}
}
