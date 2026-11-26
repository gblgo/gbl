// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

type sseEdgeFailWriter struct {
	header http.Header
	body   strings.Builder
	calls  int
	failAt int
	err    error
}

func (w *sseEdgeFailWriter) Header() http.Header { return w.header }

func (w *sseEdgeFailWriter) WriteHeader(int) {}

func (w *sseEdgeFailWriter) Write(p []byte) (int, error) {
	w.calls++
	if w.calls == w.failAt {
		return 0, w.err
	}
	return w.body.Write(p)
}

func TestSSEEdgeWriteSSEDataEveryErrorPoint(t *testing.T) {
	tests := []struct {
		name      string
		data      []byte
		wantCalls int
	}{
		{name: "single line", data: []byte("payload"), wantCalls: 4},
		{name: "multiline CRLF CR LF", data: []byte("first\r\nsecond\rthird\n"), wantCalls: 13},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			success := &sseEdgeFailWriter{header: make(http.Header)}
			if err := writeSSEData(success, tt.data); err != nil {
				t.Fatalf("writeSSEData() = %v", err)
			}
			if success.calls != tt.wantCalls {
				t.Fatalf("successful write calls = %d, want %d", success.calls, tt.wantCalls)
			}

			for failAt := 1; failAt <= tt.wantCalls; failAt++ {
				t.Run(fmt.Sprintf("write call %d", failAt), func(t *testing.T) {
					failure := errors.New("fragment write failed")
					sink := &sseEdgeFailWriter{
						header: make(http.Header),
						failAt: failAt,
						err:    failure,
					}
					if err := writeSSEData(sink, tt.data); !errors.Is(err, failure) {
						t.Fatalf("writeSSEData() error = %v, want %v", err, failure)
					}
					if sink.calls != failAt {
						t.Fatalf("write calls = %d, want stop at %d", sink.calls, failAt)
					}
				})
			}
		})
	}
}

func TestSSEEdgeBeginSendClosingWithAndWithoutCause(t *testing.T) {
	t.Run("normal close", func(t *testing.T) {
		writer, sink := sseTestNewWriter(t, time.Second, nil, nil)
		writer.beginClosing()
		if err := writer.Send("never written"); !errors.Is(err, context.Canceled) {
			t.Fatalf("Send() = %v, want context.Canceled", err)
		}
		_, body, _, flushes, deadlines, _ := sink.snapshot()
		if body != empty || flushes != 0 || deadlines != 0 {
			t.Fatalf("rejected send touched sink: body=%q flushes=%d deadlines=%d", body, flushes, deadlines)
		}
	})

	t.Run("custom cause", func(t *testing.T) {
		writer, sink := sseTestNewWriter(t, time.Second, nil, nil)
		cause := errors.New("stream stopped by shutdown")
		writer.beginClosing()
		writer.cancel(cause)
		if err := writer.Send("never written"); !errors.Is(err, cause) {
			t.Fatalf("Send() = %v, want custom cause %v", err, cause)
		}
		_, body, _, flushes, deadlines, _ := sink.snapshot()
		if body != empty || flushes != 0 || deadlines != 0 {
			t.Fatalf("rejected send touched sink: body=%q flushes=%d deadlines=%d", body, flushes, deadlines)
		}
	})
}

func TestSSEEdgeStartFlushFailure(t *testing.T) {
	writer, sink := sseTestNewWriter(t, time.Second, nil, nil)
	failure := errors.New("startup flush failed")
	sink.setFlushErr(failure)
	if err := writer.start(); !errors.Is(err, failure) {
		t.Fatalf("start() = %v, want %v", err, failure)
	}

	writer.callbackMu.Lock()
	started, completed := writer.started, writer.completed
	writer.callbackMu.Unlock()
	if started || completed {
		t.Fatalf("failed startup state: started=%t completed=%t, want false false", started, completed)
	}
	if cause := context.Cause(writer.ctx); cause != nil {
		t.Fatalf("failed startup canceled stream with %v", cause)
	}
	header, body, status, flushes, deadlines, _ := sink.snapshot()
	if header.Get(contentType) != sseContentType || header.Get("Cache-Control") != sseCacheControl {
		t.Errorf("startup headers = %v, want SSE headers", header)
	}
	if body != empty || status != http.StatusOK || flushes != 1 || deadlines != 1 {
		t.Errorf("startup sink: body=%q status=%d flushes=%d deadlines=%d", body, status, flushes, deadlines)
	}
}

func TestSSEEdgeBeforeWriteDefaultDeadlineAndCancellation(t *testing.T) {
	t.Run("default finite deadline", func(t *testing.T) {
		writer, sink := sseTestNewWriter(t, 0, nil, nil)
		before := time.Now().Add(sseDefaultWriteTimeout)
		if err := writer.beforeWrite(); err != nil {
			t.Fatalf("beforeWrite() = %v", err)
		}
		after := time.Now().Add(sseDefaultWriteTimeout)
		_, _, _, _, deadlines, deadline := sink.snapshot()
		if deadlines != 1 || deadline.Before(before) || deadline.After(after) {
			t.Fatalf("default deadline: calls=%d deadline=%v, want one within [%v, %v]", deadlines, deadline, before, after)
		}
	})

	t.Run("cancellation skips deadline", func(t *testing.T) {
		writer, sink := sseTestNewWriter(t, time.Second, nil, nil)
		cause := errors.New("request canceled")
		writer.cancel(cause)
		if err := writer.beforeWrite(); !errors.Is(err, cause) {
			t.Fatalf("beforeWrite() = %v, want %v", err, cause)
		}
		_, _, _, _, deadlines, _ := sink.snapshot()
		if deadlines != 0 {
			t.Fatalf("deadline calls = %d, want 0 after cancellation", deadlines)
		}
	})
}

func sseEdgeRequireOutcome(t *testing.T, writer *sseWriter, gotErr, wantErr error, wantFailed bool) {
	t.Helper()
	if !errors.Is(gotErr, wantErr) {
		t.Fatalf("frame error = %v, want %v", gotErr, wantErr)
	}
	if cause := context.Cause(writer.ctx); !errors.Is(cause, wantErr) {
		t.Errorf("stream cause = %v, want %v", cause, wantErr)
	}
	writer.callbackMu.Lock()
	completed, terminalErr := writer.completed, writer.terminalErr
	writer.callbackMu.Unlock()
	writer.sendMu.Lock()
	closing := writer.closing
	writer.sendMu.Unlock()
	if completed != wantFailed || closing != wantFailed {
		t.Errorf("terminal state: completed=%t closing=%t, want %t", completed, closing, wantFailed)
	}
	if wantFailed && !errors.Is(terminalErr, wantErr) {
		t.Errorf("retained error = %v, want %v", terminalErr, wantErr)
	}
	if !wantFailed && terminalErr != nil {
		t.Errorf("cancellation retained terminal error %v", terminalErr)
	}
}

func TestSSEEdgeWriteDataDeadlineWriteFlushAndCancellation(t *testing.T) {
	deadlineFailure := errors.New("data deadline failed")
	writeFailure := errors.New("data write failed")
	flushFailure := errors.New("data flush failed")
	cancelCause := errors.New("queued data canceled")
	tests := []struct {
		name       string
		setup      func(*sseTestSink)
		cancel     error
		wantErr    error
		wantFailed bool
		wantBody   string
		wantFlush  int
	}{
		{name: "deadline error", setup: func(s *sseTestSink) { s.setDeadlineErr(deadlineFailure) }, wantErr: deadlineFailure, wantFailed: true},
		{name: "write error", setup: func(s *sseTestSink) { s.setWriteErr(writeFailure) }, wantErr: writeFailure, wantFailed: true},
		{name: "flush error", setup: func(s *sseTestSink) { s.setFlushErr(flushFailure) }, wantErr: flushFailure, wantFailed: true, wantBody: "data: payload\n\n", wantFlush: 1},
		{name: "matching cancellation cause", cancel: cancelCause, wantErr: cancelCause},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			writer, sink := sseTestNewWriter(t, time.Second, nil, nil)
			if tt.setup != nil {
				tt.setup(sink)
			}
			if tt.cancel != nil {
				writer.cancel(tt.cancel)
			}
			err := writer.writeData([]byte("payload"))
			sseEdgeRequireOutcome(t, writer, err, tt.wantErr, tt.wantFailed)
			_, body, _, flushes, _, _ := sink.snapshot()
			if body != tt.wantBody || flushes != tt.wantFlush {
				t.Errorf("sink body=%q flushes=%d, want body=%q flushes=%d", body, flushes, tt.wantBody, tt.wantFlush)
			}
		})
	}
}

func TestSSEEdgeHeartbeatDeadlineWriteFlushAndCancellation(t *testing.T) {
	deadlineFailure := errors.New("heartbeat deadline failed")
	writeFailure := errors.New("heartbeat write failed")
	flushFailure := errors.New("heartbeat flush failed")
	cancelCause := errors.New("heartbeat canceled")
	tests := []struct {
		name       string
		setup      func(*sseTestSink)
		cancel     error
		wantErr    error
		wantFailed bool
		wantBody   string
		wantFlush  int
	}{
		{name: "deadline error", setup: func(s *sseTestSink) { s.setDeadlineErr(deadlineFailure) }, wantErr: deadlineFailure, wantFailed: true},
		{name: "write error", setup: func(s *sseTestSink) { s.setWriteErr(writeFailure) }, wantErr: writeFailure, wantFailed: true},
		{name: "flush error", setup: func(s *sseTestSink) { s.setFlushErr(flushFailure) }, wantErr: flushFailure, wantFailed: true, wantBody: string(sseHeartbeat), wantFlush: 1},
		{name: "matching cancellation cause", cancel: cancelCause, wantErr: cancelCause},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			writer, sink := sseTestNewWriter(t, time.Second, nil, nil)
			if tt.setup != nil {
				tt.setup(sink)
			}
			if tt.cancel != nil {
				writer.cancel(tt.cancel)
			}
			err := writer.writeHeartbeat()
			sseEdgeRequireOutcome(t, writer, err, tt.wantErr, tt.wantFailed)
			_, body, _, flushes, _, _ := sink.snapshot()
			if body != tt.wantBody || flushes != tt.wantFlush {
				t.Errorf("sink body=%q flushes=%d, want body=%q flushes=%d", body, flushes, tt.wantBody, tt.wantFlush)
			}
		})
	}
}

func TestSSEEdgeHeartbeatLoopExitsOnFailureAndCancellation(t *testing.T) {
	for _, failureKind := range []string{"write", "flush"} {
		t.Run(failureKind+" failure", func(t *testing.T) {
			failure := errors.New("heartbeat loop " + failureKind + " failed")
			writer, sink := sseTestNewWriter(t, 5*time.Millisecond, nil, nil)
			if failureKind == "write" {
				sink.setWriteErr(failure)
			} else {
				sink.setFlushErr(failure)
			}
			stop := writer.startHeartbeat()
			stopped := false
			defer func() {
				if !stopped {
					stop()
				}
			}()
			sseTestWait(t, writer.Done(), "heartbeat loop cancellation")
			stop()
			stopped = true
			if cause := context.Cause(writer.ctx); !errors.Is(cause, failure) {
				t.Fatalf("heartbeat cause = %v, want %v", cause, failure)
			}
		})
	}

	t.Run("cancellation", func(t *testing.T) {
		writer, sink := sseTestNewWriter(t, time.Hour, nil, nil)
		cause := errors.New("stop heartbeat loop")
		writer.cancel(cause)
		stop := writer.startHeartbeat()
		stop()
		sseEdgeRequireOutcome(t, writer, cause, cause, false)
		_, body, _, flushes, deadlines, _ := sink.snapshot()
		if body != empty || flushes != 0 || deadlines != 0 {
			t.Fatalf("canceled heartbeat touched sink: body=%q flushes=%d deadlines=%d", body, flushes, deadlines)
		}
	})
}

func TestSSEEdgeSendSerializationFailureThenRecovery(t *testing.T) {
	writer, sink := sseTestNewWriter(t, time.Second, nil, nil)
	sseTestStart(t, writer, sink)

	err := writer.Send(map[string]any{"unsupported": make(chan int)})
	var unsupported *json.UnsupportedTypeError
	if !errors.As(err, &unsupported) {
		t.Fatalf("Send() error = %v, want json.UnsupportedTypeError", err)
	}
	if cause := context.Cause(writer.ctx); cause != nil {
		t.Fatalf("serialization failure canceled stream with %v", cause)
	}
	_, body, _, flushes, deadlines, _ := sink.snapshot()
	if body != empty || flushes != 1 || deadlines != 1 {
		t.Fatalf("serialization failure touched sink: body=%q flushes=%d deadlines=%d", body, flushes, deadlines)
	}

	if err := writer.Send("after serialization error"); err != nil {
		t.Fatalf("Send() after serialization error = %v", err)
	}
	_, body, _, flushes, deadlines, _ = sink.snapshot()
	if body != "data: after serialization error\n\n" || flushes != 2 || deadlines != 2 {
		t.Fatalf("recovered sink: body=%q flushes=%d deadlines=%d", body, flushes, deadlines)
	}
}
