// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// that can be found in the LICENSE file.

package gbl

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"reflect"
	"sync"
	"time"
)

const (
	// sseContentType is the media type required by the SSE protocol.
	sseContentType = "text/event-stream"
	// sseCacheControl prevents intermediaries from caching an event stream.
	sseCacheControl = "no-cache"
	// sseMaxHeartbeatInterval caps idle heartbeats when WriteTimeout is disabled
	// or much longer than the interval.
	sseMaxHeartbeatInterval = 15 * time.Second
)

var (
	// sseDataPrefix starts one data line in an SSE event.
	sseDataPrefix = []byte("data: ")
	// sseNewline terminates a data line or the event itself.
	sseNewline = []byte{'\n'}
	// sseHeartbeat is an SSE comment, so browsers keep the connection without
	// delivering a business message.
	sseHeartbeat = []byte(": ping\n\n")
)

// SSEWriter sends server-sent events on the current HTTP response. Declaring it
// on a GET handler opts the route into the SSE pipeline; declaring it on any
// other method fails route generation. Its concrete implementation is private:
// heartbeat, write deadlines, flushing and disconnect cancellation are managed
// by the framework.
type SSEWriter interface {
	// Send encodes v as event data, writes it and flushes immediately. Strings and
	// byte slices are sent verbatim; other values follow gbl's normal response
	// serialization, including JSON for composite values.
	Send(v any) error
	// Done closes when the client disconnects, a write fails or the handler exits.
	Done() <-chan struct{}
}

// sseWriter serializes business events with heartbeat comments and owns the
// request-scoped cancellation used to stop the user's event loop.
type sseWriter struct {
	// 16-byte fields (interfaces)
	w responseWriter
	// ctx is derived from the request and canceled on any stream failure.
	ctx context.Context
	// 8-byte fields (function, duration)
	cancel       context.CancelCauseFunc
	writeTimeout time.Duration
	// mu prevents heartbeat and business writes from interleaving frames.
	mu sync.Mutex
}

// newSSEWriter creates the request-scoped stream without committing headers, so
// parameter binding can still answer a normal 400 before streaming starts.
func newSSEWriter(w responseWriter, r *http.Request, writeTimeout time.Duration) *sseWriter {
	ctx, cancel := context.WithCancelCause(r.Context())
	return &sseWriter{w: w, ctx: ctx, cancel: cancel, writeTimeout: writeTimeout}
}

// sseHeartbeatInterval keeps idle writes ahead of the configured write deadline.
// With no write deadline, a fixed interval still prevents idle proxies from
// dropping the stream.
func sseHeartbeatInterval(writeTimeout time.Duration) time.Duration {
	if writeTimeout <= 0 {
		return sseMaxHeartbeatInterval
	}
	interval := min(writeTimeout/2, sseMaxHeartbeatInterval)
	if interval <= 0 {
		return time.Nanosecond
	}
	return interval
}

// Send converts one value with the same rules as a normal handler result, then
// writes it as one complete SSE event.
func (s *sseWriter) Send(v any) error {
	buf := responseBufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	_, err := convertToBuffer(buf, reflect.ValueOf(v))
	if err != nil {
		responseBufferPool.Put(buf)
		return err
	}
	err = s.writeData(buf.Bytes())
	responseBufferPool.Put(buf)
	return err
}

// Done reports stream cancellation to the user's event loop.
func (s *sseWriter) Done() <-chan struct{} { return s.ctx.Done() }

// setHeaders applies the SSE response headers without committing them.
func (s *sseWriter) setHeaders() {
	s.w.Header().Set(contentType, sseContentType)
	s.w.Header().Set("Cache-Control", sseCacheControl)
}

// start sends the response headers so the client can establish the stream before
// the first business event arrives.
func (s *sseWriter) start() error {
	s.setHeaders()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.beforeWrite(); err != nil {
		return err
	}
	return http.NewResponseController(s.w).Flush()
}

// startHeartbeat launches the idle heartbeat and returns a function that stops
// it and waits for any in-progress write to finish.
func (s *sseWriter) startHeartbeat() func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(sseHeartbeatInterval(s.writeTimeout))
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-s.ctx.Done():
				return
			case <-ticker.C:
				if err := s.writeHeartbeat(); err != nil {
					return
				}
			}
		}
	}()
	return func() {
		close(stop)
		<-done
	}
}

// writeHeartbeat sends an SSE comment and flushes it like a business event.
func (s *sseWriter) writeHeartbeat() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.beforeWrite(); err != nil {
		return s.fail(err)
	}
	if _, err := s.w.Write(sseHeartbeat); err != nil {
		return s.fail(err)
	}
	if err := http.NewResponseController(s.w).Flush(); err != nil {
		return s.fail(err)
	}
	return nil
}

// writeData writes one event while holding the same lock used by heartbeats.
func (s *sseWriter) writeData(data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.beforeWrite(); err != nil {
		return s.fail(err)
	}
	if err := writeSSEData(s.w, data); err != nil {
		return s.fail(err)
	}
	if err := http.NewResponseController(s.w).Flush(); err != nil {
		return s.fail(err)
	}
	return nil
}

// beforeWrite rejects a canceled stream and renews the request's write deadline.
// Writers without deadline support can still stream; only unsupported deadlines
// are ignored, while real deadline failures terminate the stream.
func (s *sseWriter) beforeWrite() error {
	if err := context.Cause(s.ctx); err != nil {
		return err
	}
	if s.writeTimeout <= 0 {
		return nil
	}
	err := http.NewResponseController(s.w).SetWriteDeadline(time.Now().Add(s.writeTimeout))
	if errors.Is(err, http.ErrNotSupported) {
		return nil
	}
	return err
}

// fail records the first stream error as the cancellation cause.
func (s *sseWriter) fail(err error) error {
	s.cancel(err)
	return err
}

// writeSSEData frames every payload line as a data field. CRLF and bare CR are
// normalized to LF, and a trailing line break produces a final empty data field
// so the decoded event preserves the original payload.
func writeSSEData(w http.ResponseWriter, data []byte) error {
	start := 0
	for {
		end := start
		for end < len(data) && data[end] != '\r' && data[end] != '\n' {
			end++
		}
		if _, err := w.Write(sseDataPrefix); err != nil {
			return err
		}
		if _, err := w.Write(data[start:end]); err != nil {
			return err
		}
		if _, err := w.Write(sseNewline); err != nil {
			return err
		}
		if end == len(data) {
			break
		}
		if data[end] == '\r' && end+1 < len(data) && data[end+1] == '\n' {
			end++
		}
		start = end + 1
	}
	_, err := w.Write(sseNewline)
	return err
}
