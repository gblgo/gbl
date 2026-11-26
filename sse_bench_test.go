// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// sseTestBenchSink discards frame bytes without retaining an ever-growing
// buffer, while implementing the deadline and flush capabilities SSE requires.
type sseTestBenchSink struct {
	header  http.Header
	written int
	flushes int
}

func (s *sseTestBenchSink) Header() http.Header { return s.header }

func (s *sseTestBenchSink) WriteHeader(int) {}

func (s *sseTestBenchSink) Write(data []byte) (int, error) {
	s.written += len(data)
	return len(data), nil
}

func (s *sseTestBenchSink) SetWriteDeadline(time.Time) error { return nil }

func (s *sseTestBenchSink) FlushError() error {
	s.flushes++
	return nil
}

func BenchmarkSSESend(b *testing.B) {
	streamCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sink := &sseTestBenchSink{header: make(http.Header)}
	req := httptest.NewRequest(http.MethodGet, "http://example.test/events", nil)
	writer := newSSEWriter(&pooledResponseWriter{w: sink}, req, time.Second, nil, streamCtx)
	if err := writer.start(); err != nil {
		b.Fatalf("start SSE stream: %v", err)
	}
	payload := "benchmark event"
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := writer.Send(payload); err != nil {
			b.Fatalf("Send: %v", err)
		}
	}
	b.StopTimer()
	writer.complete()
	if want := b.N * (len("data: ") + len(payload) + len("\n\n")); sink.written != want {
		b.Fatalf("written bytes = %d, want %d", sink.written, want)
	}
	if sink.flushes != b.N+1 {
		b.Fatalf("flushes = %d, want %d", sink.flushes, b.N+1)
	}
}
