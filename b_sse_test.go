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
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type sseTestLogEntry struct {
	message string
	keys    []any
}

// sseTestLogger is shared by stream, heartbeat and callback goroutines. Every
// read is a locked snapshot; asynchronous log observations use the channel.
type sseTestLogger struct {
	mu      sync.Mutex
	entries []sseTestLogEntry
	errors  chan sseTestLogEntry
}

func newSSETestLogger() *sseTestLogger {
	return &sseTestLogger{errors: make(chan sseTestLogEntry, 64)}
}

func (l *sseTestLogger) Info(msg string, keysAndValues ...any) {
	l.record(msg, keysAndValues)
}

func (l *sseTestLogger) Warn(msg string, keysAndValues ...any) {
	l.record(msg, keysAndValues)
}

func (l *sseTestLogger) Error(msg string, keysAndValues ...any) {
	l.record(msg, keysAndValues)
	select {
	case l.errors <- sseTestLogEntry{message: msg, keys: append([]any(nil), keysAndValues...)}:
	default:
	}
}

func (l *sseTestLogger) record(msg string, keysAndValues []any) {
	entry := sseTestLogEntry{message: msg, keys: append([]any(nil), keysAndValues...)}
	l.mu.Lock()
	l.entries = append(l.entries, entry)
	l.mu.Unlock()
}

func (l *sseTestLogger) snapshot() []sseTestLogEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]sseTestLogEntry(nil), l.entries...)
}

func sseTestWaitForLog(t *testing.T, logger *sseTestLogger, message string) sseTestLogEntry {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case entry := <-logger.errors:
			if entry.message == message {
				return entry
			}
		case <-timer.C:
			t.Fatalf("timed out waiting for log %q; logs: %+v", message, logger.snapshot())
		}
	}
}

func sseTestCountLog(entries []sseTestLogEntry, message string) int {
	count := 0
	for _, entry := range entries {
		if entry.message == message {
			count++
		}
	}
	return count
}

// sseTestSink models the streaming capabilities of a real net/http connection.
// Its mutable state is guarded because heartbeats and callback logging run on
// independent goroutines.
type sseTestSink struct {
	mu sync.Mutex

	header http.Header
	body   bytes.Buffer
	status int

	writes         int
	flushes        int
	deadlines      int
	lastDeadline   time.Time
	writeErr       error
	flushErr       error
	deadlineErr    error
	failWriteAt    int
	writeGate      <-chan struct{}
	writeStarted   chan struct{}
	writeStartOnce sync.Once
	flushed        chan struct{}
}

func newSSETestSink() *sseTestSink {
	return &sseTestSink{
		header:       make(http.Header),
		writeStarted: make(chan struct{}),
		flushed:      make(chan struct{}, 64),
	}
}

func (s *sseTestSink) Header() http.Header { return s.header }

func (s *sseTestSink) WriteHeader(status int) {
	s.mu.Lock()
	if s.status == 0 {
		s.status = status
	}
	s.mu.Unlock()
}

func (s *sseTestSink) Write(data []byte) (int, error) {
	if s.writeGate != nil {
		s.writeStartOnce.Do(func() { close(s.writeStarted) })
		<-s.writeGate
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes++
	if s.failWriteAt > 0 && s.writes == s.failWriteAt {
		return 0, s.writeErr
	}
	if s.writeErr != nil {
		return 0, s.writeErr
	}
	return s.body.Write(data)
}

func (s *sseTestSink) SetWriteDeadline(deadline time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deadlines++
	s.lastDeadline = deadline
	return s.deadlineErr
}

func (s *sseTestSink) FlushError() error {
	s.mu.Lock()
	s.flushes++
	if s.status == 0 {
		s.status = http.StatusOK
	}
	err := s.flushErr
	s.mu.Unlock()
	select {
	case s.flushed <- struct{}{}:
	default:
	}
	return err
}

func (s *sseTestSink) setWriteErr(err error) {
	s.mu.Lock()
	s.writeErr = err
	s.mu.Unlock()
}

func (s *sseTestSink) setFlushErr(err error) {
	s.mu.Lock()
	s.flushErr = err
	s.mu.Unlock()
}

func (s *sseTestSink) setDeadlineErr(err error) {
	s.mu.Lock()
	s.deadlineErr = err
	s.mu.Unlock()
}

func (s *sseTestSink) setFailWriteAt(n int, err error) {
	s.mu.Lock()
	s.failWriteAt = n
	s.writeErr = err
	s.mu.Unlock()
}

func (s *sseTestSink) setWriteGate(gate <-chan struct{}) {
	s.mu.Lock()
	s.writeGate = gate
	s.mu.Unlock()
}

func (s *sseTestSink) snapshot() (http.Header, string, int, int, int, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.header.Clone(), s.body.String(), s.status, s.flushes, s.deadlines, s.lastDeadline
}

func sseTestNewWriter(t *testing.T, timeout time.Duration, logger Logger, streamCtx context.Context) (*sseWriter, *sseTestSink) {
	t.Helper()
	if streamCtx == nil {
		var cancel context.CancelFunc
		streamCtx, cancel = context.WithCancel(context.Background())
		t.Cleanup(cancel)
	}
	sink := newSSETestSink()
	req := httptest.NewRequest(http.MethodGet, "http://example.test/events", nil)
	writer := newSSEWriter(&pooledResponseWriter{w: sink}, req, timeout, logger, streamCtx)
	t.Cleanup(writer.complete)
	return writer, sink
}

func sseTestStart(t *testing.T, writer *sseWriter, sink *sseTestSink) {
	t.Helper()
	if err := writer.start(); err != nil {
		t.Fatalf("start SSE stream: %v", err)
	}
	select {
	case <-sink.flushed:
	default:
		t.Fatal("start did not signal a header flush")
	}
}

func sseTestWait[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	select {
	case value := <-ch:
		return value
	case <-timer.C:
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}

func sseTestRequireNoValue[T any](t *testing.T, ch <-chan T, duration time.Duration, what string) {
	t.Helper()
	if duration <= 0 {
		select {
		case value := <-ch:
			t.Errorf("unexpected %s: %v", what, value)
		default:
		}
		return
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case value := <-ch:
		t.Errorf("unexpected %s: %v", what, value)
	case <-timer.C:
	}
}

// sseTestAsyncRegistration proves registration returns while the callback is
// still blocked. A synchronous callback implementation cannot pass this check.
func sseTestAsyncRegistration(t *testing.T, register func(func())) {
	t.Helper()
	entered := make(chan struct{})
	release := make(chan struct{})
	returned := make(chan struct{})
	registered := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	go func() {
		register(func() {
			close(entered)
			<-release
			close(returned)
		})
		close(registered)
	}()
	sseTestWait(t, registered, "callback registration to return")
	sseTestWait(t, entered, "asynchronous callback to start")
	sseTestRequireNoValue(t, returned, 20*time.Millisecond, "callback completion before release")
	releaseOnce.Do(func() { close(release) })
	sseTestWait(t, returned, "callback completion")
}

func TestSSEStartup(t *testing.T) {
	writer, sink := sseTestNewWriter(t, 2*time.Second, nil, nil)
	header, body, status, flushes, deadlines, _ := sink.snapshot()
	if status != 0 || flushes != 0 || deadlines != 0 || body != empty {
		t.Fatalf("new writer committed a response: status=%d flushes=%d deadlines=%d body=%q", status, flushes, deadlines, body)
	}
	if got := header.Get(contentType); got != empty {
		t.Errorf("new writer Content-Type = %q, want unset", got)
	}
	sseTestRequireNoValue(t, writer.Done(), 0, "Done to close before startup")

	before := time.Now()
	sseTestStart(t, writer, sink)
	header, body, status, flushes, deadlines, deadline := sink.snapshot()
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}
	if flushes != 1 {
		t.Errorf("flushes = %d, want 1", flushes)
	}
	if deadlines != 1 {
		t.Errorf("deadline calls = %d, want 1", deadlines)
	}
	if body != empty {
		t.Errorf("startup body = %q, want empty", body)
	}
	if got := header.Get(contentType); got != sseContentType {
		t.Errorf("Content-Type = %q, want %q", got, sseContentType)
	}
	if got := header.Get("Cache-Control"); got != sseCacheControl {
		t.Errorf("Cache-Control = %q, want %q", got, sseCacheControl)
	}
	if deadline.Before(before.Add(time.Second)) || deadline.After(time.Now().Add(3*time.Second)) {
		t.Errorf("write deadline = %v, want approximately two seconds ahead", deadline)
	}
	sseTestRequireNoValue(t, writer.Done(), 0, "Done to close after successful startup")
}

func TestSSEStartupFailureRetainsError(t *testing.T) {
	failure := errors.New("deadline unavailable")
	writer, sink := sseTestNewWriter(t, time.Second, nil, nil)
	sink.setDeadlineErr(failure)
	if err := writer.start(); !errors.Is(err, failure) {
		t.Fatalf("start error = %v, want %v", err, failure)
	}
	if got := writer.fail(failure); !errors.Is(got, failure) {
		t.Fatalf("fail error = %v, want %v", got, failure)
	}
	sseTestWait(t, writer.Done(), "stream cancellation after startup failure")
	_, _, status, flushes, _, _ := sink.snapshot()
	if status != 0 || flushes != 0 {
		t.Errorf("failed startup committed status=%d flushes=%d, want 0 and 0", status, flushes)
	}

	errorResult := make(chan error, 1)
	sseTestAsyncRegistration(t, func(callback func()) {
		writer.OnError(func(err error) {
			errorResult <- err
			callback()
		})
	})
	if got := sseTestWait(t, errorResult, "late error callback"); !errors.Is(got, failure) {
		t.Errorf("late callback error = %v, want %v", got, failure)
	}
	writer.complete()
	sseTestAsyncRegistration(t, writer.OnComplete)
}

func TestSSEStartupFailureDoesNotRunHandler(t *testing.T) {
	failure := errors.New("cannot set deadline")
	logger := newSSETestLogger()
	e := newEngine()
	t.Cleanup(e.cancelStreams)
	e.setLogger(logger)
	handlerCalled := make(chan struct{}, 1)
	e.register(methodGet, "/events", func(SSEWriter) { handlerCalled <- struct{}{} })
	e.generateRoute()
	sink := newSSETestSink()
	sink.setDeadlineErr(failure)
	req := httptest.NewRequest(http.MethodGet, "http://example.test/events", nil)
	e.ServeHTTP(sink, req)

	select {
	case <-handlerCalled:
		t.Fatal("SSE handler ran after startup failed")
	default:
	}
	_, body, status, flushes, _, _ := sink.snapshot()
	if status != http.StatusInternalServerError || body != http.StatusText(http.StatusInternalServerError) {
		t.Errorf("response = %d %q, want 500 Internal Server Error", status, body)
	}
	if flushes != 0 {
		t.Errorf("flushes = %d, want 0", flushes)
	}
	entry := sseTestWaitForLog(t, logger, "SSE startup failed")
	if len(entry.keys) < 2 || entry.keys[0] != "error" || !errors.Is(entry.keys[1].(error), failure) {
		t.Errorf("startup log fields = %v, want original error", entry.keys)
	}
	if got := sseTestCountLog(logger.snapshot(), "SSE stream failed"); got != 0 {
		t.Errorf("default stream-failure logs = %d, want 0 before startup succeeds", got)
	}
}

func TestSSEHEADOnlyCommitsHeaders(t *testing.T) {
	e := newEngine()
	t.Cleanup(e.cancelStreams)
	handlerCalled := make(chan struct{}, 1)
	e.register(methodGet, "/events", func(SSEWriter) { handlerCalled <- struct{}{} })
	e.generateRoute()
	sink := newSSETestSink()
	req := httptest.NewRequest(http.MethodHead, "http://example.test/events", nil)
	e.ServeHTTP(sink, req)
	select {
	case <-handlerCalled:
		t.Fatal("SSE handler ran for HEAD")
	default:
	}
	header, body, status, flushes, deadlines, _ := sink.snapshot()
	if status != http.StatusOK || body != empty || flushes != 0 || deadlines != 0 {
		t.Errorf("HEAD response: status=%d body=%q flushes=%d deadlines=%d", status, body, flushes, deadlines)
	}
	if header.Get(contentType) != sseContentType || header.Get("Cache-Control") != sseCacheControl {
		t.Errorf("HEAD SSE headers = %v", header)
	}
}

func TestWriteSSEDataFraming(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want string
	}{
		{name: "single line", data: []byte("hello"), want: "data: hello\n\n"},
		{name: "empty payload", data: nil, want: "data: \n\n"},
		{name: "LF", data: []byte("first\nsecond"), want: "data: first\ndata: second\n\n"},
		{name: "CRLF and CR", data: []byte("first\r\nsecond\rthird"), want: "data: first\ndata: second\ndata: third\n\n"},
		{name: "trailing newline preserved", data: []byte("first\n"), want: "data: first\ndata: \n\n"},
		{name: "single newline preserved", data: []byte("\n"), want: "data: \ndata: \n\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sink := newSSETestSink()
			if err := writeSSEData(sink, tt.data); err != nil {
				t.Fatalf("writeSSEData: %v", err)
			}
			_, body, _, _, _, _ := sink.snapshot()
			if body != tt.want {
				t.Fatalf("frame = %q, want %q", body, tt.want)
			}
		})
	}
}

func TestSSESendSerialization(t *testing.T) {
	tests := []struct {
		name  string
		value any
		want  string
	}{
		{name: "string", value: "hello", want: "data: hello\n\n"},
		{name: "bytes", value: []byte("a\nb"), want: "data: a\ndata: b\n\n"},
		{name: "JSON object", value: map[string]int{"answer": 42}, want: "data: {\"answer\":42}\n\n"},
		{name: "scalar", value: 42, want: "data: 42\n\n"},
		{name: "nil", value: nil, want: "data: \n\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			writer, sink := sseTestNewWriter(t, time.Second, nil, nil)
			sseTestStart(t, writer, sink)
			if err := writer.Send(tt.value); err != nil {
				t.Fatalf("Send: %v", err)
			}
			_, body, _, flushes, deadlines, _ := sink.snapshot()
			if body != tt.want {
				t.Errorf("body = %q, want %q", body, tt.want)
			}
			if flushes != 2 || deadlines != 2 {
				t.Errorf("flushes=%d deadlines=%d, want 2 each", flushes, deadlines)
			}
		})
	}

	t.Run("unsupported value does not terminate stream", func(t *testing.T) {
		writer, sink := sseTestNewWriter(t, time.Second, nil, nil)
		sseTestStart(t, writer, sink)
		if err := writer.Send(make(chan int)); err == nil {
			t.Fatal("Send(channel) succeeded, want serialization error")
		}
		_, body, _, flushes, _, _ := sink.snapshot()
		if body != empty || flushes != 1 {
			t.Errorf("failed serialization wrote body=%q flushes=%d", body, flushes)
		}
		sseTestRequireNoValue(t, writer.Done(), 20*time.Millisecond, "Done after serialization error")
	})
}

func TestSSEHeartbeatInterval(t *testing.T) {
	tests := []struct {
		timeout time.Duration
		want    time.Duration
	}{
		{timeout: 0, want: sseMaxHeartbeatInterval},
		{timeout: -time.Second, want: sseMaxHeartbeatInterval},
		{timeout: time.Nanosecond, want: time.Nanosecond},
		{timeout: 40 * time.Millisecond, want: 20 * time.Millisecond},
		{timeout: time.Minute, want: sseMaxHeartbeatInterval},
	}
	for _, tt := range tests {
		if got := sseHeartbeatInterval(tt.timeout); got != tt.want {
			t.Errorf("sseHeartbeatInterval(%v) = %v, want %v", tt.timeout, got, tt.want)
		}
	}
}

func TestSSEHeartbeat(t *testing.T) {
	const timeout = 80 * time.Millisecond
	writer, sink := sseTestNewWriter(t, timeout, nil, nil)
	sseTestStart(t, writer, sink)
	_, _, _, _, _, startupDeadline := sink.snapshot()
	stop := writer.startHeartbeat()
	sseTestWait(t, sink.flushed, "heartbeat flush")
	_, body, _, flushes, deadlines, heartbeatDeadline := sink.snapshot()
	if body != string(sseHeartbeat) {
		t.Errorf("heartbeat body = %q, want %q", body, sseHeartbeat)
	}
	if flushes != 2 || deadlines != 2 {
		t.Errorf("flushes=%d deadlines=%d, want 2 each", flushes, deadlines)
	}
	if !heartbeatDeadline.After(startupDeadline) {
		t.Errorf("heartbeat did not renew deadline: startup=%v heartbeat=%v", startupDeadline, heartbeatDeadline)
	}
	stop()
	_, _, _, stoppedFlushes, _, _ := sink.snapshot()
	sseTestRequireNoValue(t, sink.flushed, 3*sseHeartbeatInterval(timeout), "heartbeat after stop")
	_, _, _, finalFlushes, _, _ := sink.snapshot()
	if finalFlushes != stoppedFlushes {
		t.Errorf("flushes continued after stop: %d -> %d", stoppedFlushes, finalFlushes)
	}
}

func TestSSEHeartbeatFailure(t *testing.T) {
	for _, failureKind := range []string{"write", "flush"} {
		t.Run(failureKind, func(t *testing.T) {
			failure := errors.New("heartbeat " + failureKind + " failed")
			logger := newSSETestLogger()
			writer, sink := sseTestNewWriter(t, 40*time.Millisecond, logger, nil)
			sseTestStart(t, writer, sink)
			if failureKind == "write" {
				sink.setWriteErr(failure)
			} else {
				sink.setFlushErr(failure)
			}
			errorResult := make(chan error, 1)
			completeResult := make(chan struct{}, 1)
			writer.OnError(func(err error) { errorResult <- err })
			writer.OnComplete(func() { completeResult <- struct{}{} })
			stop := writer.startHeartbeat()
			sseTestWait(t, writer.Done(), "heartbeat failure cancellation")
			if got := sseTestWait(t, errorResult, "heartbeat error callback"); !errors.Is(got, failure) {
				t.Errorf("callback error = %v, want %v", got, failure)
			}
			stop()
			writer.complete()
			sseTestWait(t, completeResult, "completion after heartbeat failure")
			if got := sseTestCountLog(logger.snapshot(), "SSE stream failed"); got != 1 {
				t.Errorf("stream failure logs = %d, want 1", got)
			}
		})
	}
}

func TestSSEWriteFailures(t *testing.T) {
	for _, failWriteAt := range []int{1, 2, 3, 4} {
		t.Run(fmt.Sprintf("write fragment %d", failWriteAt), func(t *testing.T) {
			failure := errors.New("body write failed")
			logger := newSSETestLogger()
			writer, sink := sseTestNewWriter(t, time.Second, logger, nil)
			sink.setFailWriteAt(failWriteAt, failure)
			sseTestStart(t, writer, sink)
			errorResult := make(chan error, 1)
			completeResult := make(chan struct{}, 1)
			writer.OnError(func(err error) { errorResult <- err })
			writer.OnComplete(func() { completeResult <- struct{}{} })
			if err := writer.Send("x"); !errors.Is(err, failure) {
				t.Fatalf("Send error = %v, want %v", err, failure)
			}
			sseTestWait(t, writer.Done(), "write failure cancellation")
			if got := sseTestWait(t, errorResult, "write failure callback"); !errors.Is(got, failure) {
				t.Errorf("callback error = %v, want %v", got, failure)
			}
			writer.complete()
			sseTestWait(t, completeResult, "completion after write failure")
			if got := sseTestCountLog(logger.snapshot(), "SSE stream failed"); got != 1 {
				t.Errorf("stream failure logs = %d, want 1", got)
			}
		})
	}

	t.Run("flush failure", func(t *testing.T) {
		failure := errors.New("flush failed")
		writer, sink := sseTestNewWriter(t, time.Second, nil, nil)
		sseTestStart(t, writer, sink)
		sink.setFlushErr(failure)
		if err := writer.Send("payload"); !errors.Is(err, failure) {
			t.Fatalf("Send error = %v, want %v", err, failure)
		}
		sseTestWait(t, writer.Done(), "flush failure cancellation")
		_, body, _, _, _, _ := sink.snapshot()
		if body != "data: payload\n\n" {
			t.Errorf("body before failed flush = %q", body)
		}
	})
}

func TestSSENormalCompletion(t *testing.T) {
	logger := newSSETestLogger()
	writer, sink := sseTestNewWriter(t, time.Second, logger, nil)
	sseTestStart(t, writer, sink)
	errorResult := make(chan error, 1)
	completeResult := make(chan struct{}, 1)
	writer.OnError(func(err error) { errorResult <- err })
	writer.OnComplete(func() { completeResult <- struct{}{} })
	writer.complete()
	sseTestWait(t, writer.Done(), "normal completion cancellation")
	sseTestWait(t, completeResult, "normal completion callback")
	if cause := context.Cause(writer.ctx); !errors.Is(cause, context.Canceled) {
		t.Errorf("completion cause = %v, want context.Canceled", cause)
	}
	if err := writer.Send("late"); !errors.Is(err, context.Canceled) {
		t.Errorf("Send after completion = %v, want context.Canceled", err)
	}
	sseTestRequireNoValue(t, errorResult, 50*time.Millisecond, "error callback after normal completion")
	if got := sseTestCountLog(logger.snapshot(), "SSE stream failed"); got != 0 {
		t.Errorf("stream failure logs = %d, want 0", got)
	}
	_, body, _, flushes, _, _ := sink.snapshot()
	if body != empty || flushes != 1 {
		t.Errorf("late Send changed body=%q flushes=%d", body, flushes)
	}
}

func TestSSECallbacksRunAsynchronously(t *testing.T) {
	t.Run("late error callback", func(t *testing.T) {
		failure := errors.New("failed")
		writer, sink := sseTestNewWriter(t, time.Second, nil, nil)
		sseTestStart(t, writer, sink)
		sink.setWriteErr(failure)
		if err := writer.Send("x"); !errors.Is(err, failure) {
			t.Fatalf("Send error = %v, want %v", err, failure)
		}
		errorResult := make(chan error, 1)
		sseTestAsyncRegistration(t, func(callback func()) {
			writer.OnError(func(err error) {
				errorResult <- err
				callback()
			})
		})
		if got := sseTestWait(t, errorResult, "late error value"); !errors.Is(got, failure) {
			t.Errorf("late error = %v, want %v", got, failure)
		}
	})

	t.Run("late completion callback", func(t *testing.T) {
		writer, sink := sseTestNewWriter(t, time.Second, nil, nil)
		sseTestStart(t, writer, sink)
		writer.complete()
		sseTestAsyncRegistration(t, writer.OnComplete)
	})

	t.Run("completion does not wait for callback", func(t *testing.T) {
		writer, sink := sseTestNewWriter(t, time.Second, nil, nil)
		sseTestStart(t, writer, sink)
		entered := make(chan struct{})
		release := make(chan struct{})
		returned := make(chan struct{})
		var releaseOnce sync.Once
		t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
		writer.OnComplete(func() {
			close(entered)
			<-release
			close(returned)
		})
		completeReturned := make(chan struct{})
		go func() {
			writer.complete()
			close(completeReturned)
		}()
		sseTestWait(t, completeReturned, "complete to return")
		sseTestWait(t, entered, "completion callback to start")
		sseTestRequireNoValue(t, returned, 20*time.Millisecond, "blocked callback to return")
		releaseOnce.Do(func() { close(release) })
		sseTestWait(t, returned, "completion callback to return")
	})
}

func TestSSELateAndOnceCallbacks(t *testing.T) {
	t.Run("callbacks registered after failure and drain", func(t *testing.T) {
		failure := errors.New("retained failure")
		writer, sink := sseTestNewWriter(t, time.Second, nil, nil)
		sseTestStart(t, writer, sink)
		writer.OnError(nil)
		writer.OnComplete(nil)
		sink.setWriteErr(failure)
		if err := writer.Send("x"); !errors.Is(err, failure) {
			t.Fatalf("Send error = %v, want %v", err, failure)
		}
		writer.complete()

		errorResult := make(chan error, 1)
		sseTestAsyncRegistration(t, func(callback func()) {
			writer.OnError(func(err error) {
				errorResult <- err
				callback()
			})
		})
		if got := sseTestWait(t, errorResult, "retained error"); !errors.Is(got, failure) {
			t.Errorf("retained error = %v, want %v", got, failure)
		}
		sseTestAsyncRegistration(t, writer.OnComplete)

		secondError := make(chan error, 1)
		secondComplete := make(chan struct{}, 1)
		writer.OnError(func(err error) { secondError <- err })
		writer.OnComplete(func() { secondComplete <- struct{}{} })
		sseTestRequireNoValue(t, secondError, 50*time.Millisecond, "second error callback")
		sseTestRequireNoValue(t, secondComplete, 50*time.Millisecond, "second completion callback")
	})

	t.Run("pre-registered callbacks fire once", func(t *testing.T) {
		firstFailure := errors.New("first failure")
		writer, sink := sseTestNewWriter(t, time.Second, nil, nil)
		sseTestStart(t, writer, sink)
		errorResult := make(chan error, 2)
		completeResult := make(chan struct{}, 2)
		writer.OnError(func(err error) { errorResult <- err })
		writer.OnComplete(func() { completeResult <- struct{}{} })
		sink.setWriteErr(firstFailure)
		if err := writer.Send("x"); !errors.Is(err, firstFailure) {
			t.Fatalf("Send error = %v, want %v", err, firstFailure)
		}
		writer.complete()
		writer.complete()
		_ = writer.fail(errors.New("second failure"))
		if got := sseTestWait(t, errorResult, "first error callback"); !errors.Is(got, firstFailure) {
			t.Errorf("first error = %v, want %v", got, firstFailure)
		}
		sseTestWait(t, completeResult, "completion callback")
		sseTestRequireNoValue(t, errorResult, 50*time.Millisecond, "duplicate error callback")
		sseTestRequireNoValue(t, completeResult, 50*time.Millisecond, "duplicate completion callback")
	})

	t.Run("latest callback registered before termination wins", func(t *testing.T) {
		writer, sink := sseTestNewWriter(t, time.Second, nil, nil)
		sseTestStart(t, writer, sink)
		oldError := make(chan error, 1)
		oldComplete := make(chan struct{}, 1)
		newError := make(chan error, 1)
		newComplete := make(chan struct{}, 1)
		writer.OnError(func(err error) { oldError <- err })
		writer.OnComplete(func() { oldComplete <- struct{}{} })
		writer.OnError(func(err error) { newError <- err })
		writer.OnComplete(func() { newComplete <- struct{}{} })
		failure := errors.New("failure")
		sink.setWriteErr(failure)
		if err := writer.Send("x"); !errors.Is(err, failure) {
			t.Fatalf("Send error = %v, want %v", err, failure)
		}
		writer.complete()
		sseTestWait(t, newError, "replacement error callback")
		sseTestWait(t, newComplete, "replacement completion callback")
		sseTestRequireNoValue(t, oldError, 50*time.Millisecond, "replaced error callback")
		sseTestRequireNoValue(t, oldComplete, 50*time.Millisecond, "replaced completion callback")
	})
}

func TestSSECallbackPanicIsolation(t *testing.T) {
	t.Run("completion panic is logged", func(t *testing.T) {
		logger := newSSETestLogger()
		writer, sink := sseTestNewWriter(t, time.Second, logger, nil)
		sseTestStart(t, writer, sink)
		panicReturned := make(chan struct{})
		writer.OnComplete(func() {
			defer close(panicReturned)
			panic("completion callback panic")
		})
		writer.complete()
		sseTestWait(t, panicReturned, "panicking completion callback to unwind")
		entry := sseTestWaitForLog(t, logger, "SSE complete callback panic")
		if len(entry.keys) < 4 || entry.keys[2] != "stack" {
			t.Errorf("panic log fields = %v, want error and stack", entry.keys)
		}
	})

	t.Run("panicking logger cannot escape callback recovery", func(t *testing.T) {
		writer, sink := sseTestNewWriter(t, time.Second, &logTestPanicLogger{}, nil)
		sseTestStart(t, writer, sink)
		panicReturned := make(chan struct{})
		writer.OnComplete(func() {
			defer close(panicReturned)
			panic("completion callback panic")
		})
		writer.complete()
		sseTestWait(t, panicReturned, "callback recovery with a panicking logger")
	})

	t.Run("error panic does not prevent completion", func(t *testing.T) {
		logger := newSSETestLogger()
		writer, sink := sseTestNewWriter(t, time.Second, logger, nil)
		sseTestStart(t, writer, sink)
		panicReturned := make(chan struct{})
		completeResult := make(chan struct{}, 1)
		writer.OnError(func(error) {
			defer close(panicReturned)
			panic("error callback panic")
		})
		writer.OnComplete(func() { completeResult <- struct{}{} })
		failure := errors.New("stream failure")
		sink.setWriteErr(failure)
		if err := writer.Send("x"); !errors.Is(err, failure) {
			t.Fatalf("Send error = %v, want %v", err, failure)
		}
		sseTestWait(t, panicReturned, "panicking error callback to unwind")
		sseTestWaitForLog(t, logger, "SSE error callback panic")
		writer.complete()
		sseTestWait(t, completeResult, "completion after error callback panic")
	})
}

func TestSSEDefaultFailureLogging(t *testing.T) {
	t.Run("started stream logs first failure once", func(t *testing.T) {
		logger := newSSETestLogger()
		writer, sink := sseTestNewWriter(t, time.Second, logger, nil)
		sseTestStart(t, writer, sink)
		failure := errors.New("write failed")
		sink.setWriteErr(failure)
		if err := writer.Send("x"); !errors.Is(err, failure) {
			t.Fatalf("Send error = %v, want %v", err, failure)
		}
		_ = writer.fail(errors.New("later failure"))
		entries := logger.snapshot()
		if got := sseTestCountLog(entries, "SSE stream failed"); got != 1 {
			t.Fatalf("stream failure logs = %d, want 1; entries: %+v", got, entries)
		}
		for _, entry := range entries {
			if entry.message == "SSE stream failed" {
				if len(entry.keys) < 2 || entry.keys[0] != "error" || !errors.Is(entry.keys[1].(error), failure) {
					t.Errorf("failure log fields = %v, want original error", entry.keys)
				}
			}
		}
	})

	t.Run("startup failure is not double logged", func(t *testing.T) {
		logger := newSSETestLogger()
		writer, _ := sseTestNewWriter(t, time.Second, logger, nil)
		failure := errors.New("startup failed")
		_ = writer.fail(failure)
		if got := sseTestCountLog(logger.snapshot(), "SSE stream failed"); got != 0 {
			t.Errorf("stream failure logs = %d, want 0", got)
		}
	})
}

func TestSSEShutdownCancellation(t *testing.T) {
	logger := newSSETestLogger()
	streamCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	writer, sink := sseTestNewWriter(t, time.Second, logger, streamCtx)
	sseTestStart(t, writer, sink)
	errorResult := make(chan error, 1)
	completeResult := make(chan struct{}, 1)
	writer.OnError(func(err error) { errorResult <- err })
	writer.OnComplete(func() { completeResult <- struct{}{} })
	cancel()
	sseTestWait(t, writer.Done(), "shutdown cancellation")
	if cause := context.Cause(writer.ctx); !errors.Is(cause, context.Canceled) {
		t.Errorf("shutdown cause = %v, want context.Canceled", cause)
	}
	if err := writer.Send("late"); !errors.Is(err, context.Canceled) {
		t.Errorf("Send after shutdown = %v, want context.Canceled", err)
	}
	writer.complete()
	sseTestWait(t, completeResult, "completion after shutdown")
	sseTestRequireNoValue(t, errorResult, 50*time.Millisecond, "error callback on clean shutdown")
	if got := sseTestCountLog(logger.snapshot(), "SSE stream failed"); got != 0 {
		t.Errorf("stream failure logs = %d, want 0", got)
	}
	_, body, _, flushes, _, _ := sink.snapshot()
	if body != empty || flushes != 1 {
		t.Errorf("shutdown wrote body=%q flushes=%d", body, flushes)
	}
}

func sseTestServer(t *testing.T, handler any, writeTimeout time.Duration) (*engine, *httptest.Server) {
	t.Helper()
	e := newEngine()
	e.setLogger(newSSETestLogger())
	e.configure(func(config *Config) { config.WriteTimeout = writeTimeout })
	e.register(methodGet, "/events", handler)
	e.generateRoute()
	server := httptest.NewServer(e.buildHandler())
	// Let the real graceful-shutdown path own this test server as well.
	e.server = server.Config
	t.Cleanup(func() {
		e.cancelStreams()
		server.Close()
	})
	return e, server
}

func sseTestReadEvent(t *testing.T, reader *bufio.Reader) string {
	t.Helper()
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read SSE event: %v", err)
	}
	if line != "data: ready\n" {
		t.Fatalf("event line = %q, want data: ready", line)
	}
	terminator, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read SSE event terminator: %v", err)
	}
	if terminator != "\n" {
		t.Fatalf("event terminator = %q, want newline", terminator)
	}
	return line
}

func TestSSEClientDisconnect(t *testing.T) {
	handlerExited := make(chan struct{})
	errorResult := make(chan error, 1)
	completeResult := make(chan struct{}, 1)
	_, server := sseTestServer(t, func(stream SSEWriter) {
		stream.OnError(func(err error) { errorResult <- err })
		stream.OnComplete(func() { completeResult <- struct{}{} })
		if err := stream.Send("ready"); err != nil {
			return
		}
		<-stream.Done()
		close(handlerExited)
	}, time.Second)
	client := server.Client()
	client.Timeout = 3 * time.Second
	response, err := client.Get(server.URL + "/events")
	if err != nil {
		t.Fatalf("connect SSE stream: %v", err)
	}
	reader := bufio.NewReader(response.Body)
	sseTestReadEvent(t, reader)
	if err := response.Body.Close(); err != nil {
		t.Fatalf("close client body: %v", err)
	}
	sseTestWait(t, handlerExited, "handler exit after client disconnect")
	sseTestWait(t, completeResult, "completion after client disconnect")
	sseTestRequireNoValue(t, errorResult, 50*time.Millisecond, "error callback on client disconnect")
}

func TestSSEGracefulShutdown(t *testing.T) {
	handlerExited := make(chan struct{})
	errorResult := make(chan error, 1)
	completeResult := make(chan struct{}, 1)
	e, server := sseTestServer(t, func(stream SSEWriter) {
		stream.OnError(func(err error) { errorResult <- err })
		stream.OnComplete(func() { completeResult <- struct{}{} })
		if err := stream.Send("ready"); err != nil {
			return
		}
		<-stream.Done()
		close(handlerExited)
	}, time.Second)
	client := server.Client()
	client.Timeout = 3 * time.Second
	response, err := client.Get(server.URL + "/events")
	if err != nil {
		t.Fatalf("connect SSE stream: %v", err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	sseTestReadEvent(t, reader)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := e.shutdown(shutdownCtx); err != nil {
		t.Fatalf("graceful shutdown: %v", err)
	}
	sseTestWait(t, handlerExited, "handler exit after graceful shutdown")
	sseTestWait(t, completeResult, "completion after graceful shutdown")
	sseTestRequireNoValue(t, errorResult, 50*time.Millisecond, "error callback on graceful shutdown")
	if _, err := io.ReadAll(response.Body); err != nil {
		t.Fatalf("read stream after shutdown: %v", err)
	}
}

func TestSSEConcurrentSend(t *testing.T) {
	const count = 32
	writer, sink := sseTestNewWriter(t, time.Second, nil, nil)
	sseTestStart(t, writer, sink)
	results := make(chan error, count)
	var wg sync.WaitGroup
	wg.Add(count)
	for i := range count {
		go func() {
			defer wg.Done()
			results <- writer.Send(fmt.Sprintf("event-%d", i))
		}()
	}
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Errorf("concurrent Send: %v", err)
		}
	}
	_, body, _, flushes, _, _ := sink.snapshot()
	frames := strings.Split(strings.TrimSuffix(body, "\n\n"), "\n\n")
	if len(frames) != count {
		t.Fatalf("frame count = %d, want %d; body: %q", len(frames), count, body)
	}
	seen := make(map[string]bool, count)
	for _, frame := range frames {
		if !strings.HasPrefix(frame, "data: event-") || strings.Count(frame, "data: ") != 1 {
			t.Errorf("interleaved or malformed frame %q", frame)
		}
		seen[frame] = true
	}
	if len(seen) != count {
		t.Errorf("unique frames = %d, want %d", len(seen), count)
	}
	if flushes != count+1 {
		t.Errorf("flushes = %d, want %d", flushes, count+1)
	}
}

type sseTestBlockingJSON struct {
	entered chan<- struct{}
	release <-chan struct{}
}

func (v sseTestBlockingJSON) MarshalJSON() ([]byte, error) {
	v.entered <- struct{}{}
	<-v.release
	return []byte(`"queued"`), nil
}

func TestSSEErrorCallbackWaitsForInflightSend(t *testing.T) {
	writer, sink := sseTestNewWriter(t, time.Second, nil, nil)
	sseTestStart(t, writer, sink)
	errorResult := make(chan error, 1)
	writer.OnError(func(err error) { errorResult <- err })

	marshalEntered := make(chan struct{}, 1)
	marshalRelease := make(chan struct{})
	var releaseMarshalOnce sync.Once
	releaseMarshal := func() { releaseMarshalOnce.Do(func() { close(marshalRelease) }) }
	t.Cleanup(releaseMarshal)
	queuedResult := make(chan error, 1)
	go func() {
		queuedResult <- writer.Send(sseTestBlockingJSON{entered: marshalEntered, release: marshalRelease})
	}()
	sseTestWait(t, marshalEntered, "queued Send to enter MarshalJSON")

	sink.mu.Lock()
	sink.writeErr = errors.New("write failed")
	sink.mu.Unlock()
	if err := writer.Send("fail"); err == nil {
		t.Fatal("failing Send returned nil")
	}
	sseTestRequireNoValue(t, errorResult, 50*time.Millisecond, "OnError before in-flight Send drained")

	releaseMarshal()
	sseTestWait(t, queuedResult, "queued Send to finish")
	sseTestWait(t, errorResult, "OnError after in-flight Send drained")
	writer.complete()
}

func TestSSEConcurrentSendDuringNormalClose(t *testing.T) {
	const queued = 8
	logger := newSSETestLogger()
	writer, sink := sseTestNewWriter(t, time.Second, logger, nil)
	writeGate := make(chan struct{})
	var closeGateOnce sync.Once
	closeGate := func() { closeGateOnce.Do(func() { close(writeGate) }) }
	sink.setWriteGate(writeGate)
	t.Cleanup(closeGate)
	sseTestStart(t, writer, sink)
	errorResult := make(chan error, queued+1)
	completeResult := make(chan struct{}, 1)
	writer.OnError(func(err error) { errorResult <- err })
	writer.OnComplete(func() { completeResult <- struct{}{} })

	firstResult := make(chan error, 1)
	go func() { firstResult <- writer.Send("first") }()
	sseTestWait(t, sink.writeStarted, "first Send to block in Write")

	marshalEntered := make(chan struct{}, queued)
	marshalRelease := make(chan struct{})
	var releaseMarshalOnce sync.Once
	releaseMarshal := func() { releaseMarshalOnce.Do(func() { close(marshalRelease) }) }
	t.Cleanup(releaseMarshal)
	queuedResults := make(chan error, queued)
	for range queued {
		go func() {
			queuedResults <- writer.Send(sseTestBlockingJSON{entered: marshalEntered, release: marshalRelease})
		}()
	}
	for range queued {
		sseTestWait(t, marshalEntered, "queued Send to begin")
	}

	completeReturned := make(chan struct{})
	go func() {
		writer.complete()
		close(completeReturned)
	}()
	sseTestRequireNoValue(t, completeReturned, 30*time.Millisecond, "complete before in-flight writes drain")
	sseTestRequireNoValue(t, completeResult, 0, "completion callback before writes drain")
	sseTestRequireNoValue(t, errorResult, 0, "error callback before writes drain")

	releaseMarshal()
	closeGate()
	sseTestWait(t, completeReturned, "normal close to drain sends")
	if err := sseTestWait(t, firstResult, "first Send result"); err != nil {
		t.Errorf("first in-flight Send = %v, want success", err)
	}
	for range queued {
		if err := sseTestWait(t, queuedResults, "queued Send result"); !errors.Is(err, context.Canceled) {
			t.Errorf("queued Send = %v, want context.Canceled", err)
		}
	}
	sseTestWait(t, completeResult, "completion after queued sends drain")
	sseTestRequireNoValue(t, errorResult, 50*time.Millisecond, "error callback for sends canceled by normal close")
	if got := sseTestCountLog(logger.snapshot(), "SSE stream failed"); got != 0 {
		t.Errorf("normal close emitted %d stream-failure logs, want 0", got)
	}
}
