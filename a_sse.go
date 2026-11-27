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
	"runtime/debug"
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
	// sseDefaultWriteTimeout bounds writes when the server has no write timeout.
	sseDefaultWriteTimeout = 30 * time.Second
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
	// byte slices are sent verbatim; other values follow the normal gbl response
	// serialization, including JSON for composite values.
	Send(v any) error
	// Done closes when the client disconnects, a write fails or the handler exits.
	Done() <-chan struct{}
	// OnError sets the callback for the first stream I/O failure. It runs
	// asynchronously after termination and in-flight sends drain, including when
	// registered late with the retained failure. Startup failures happen before
	// the handler runs; normal completion and cancellation do not invoke it.
	OnError(func(error))
	// OnComplete sets the callback invoked once after the handler exits and all
	// in-flight writes drain. Stream failures are reported by OnError. It runs
	// asynchronously, including when registered late. Callbacks must not call Send.
	OnComplete(func())
}

// sseWriter serializes business events with heartbeat comments and owns the
// request-scoped cancellation used to stop the user event loop.
type sseWriter struct {
	// wrapped response sink used for frames and flushes
	w responseWriter
	// logger records stream failures and callback panics
	logger Logger
	// request and engine lifetime combined into one stream context
	ctx context.Context
	// first terminal error retained for callbacks registered after termination
	terminalErr error
	// cancels the stream context with a cause
	cancel context.CancelCauseFunc
	// default write deadline renewed before every frame
	writeTimeout time.Duration
	// error callback invoked once for the first stream failure
	onError func(error)
	// completion callback invoked once after normal exit or failure
	onComplete func()
	// stops the engine-shutdown callback when the stream finishes
	stopShutdown func() bool
	// sendMu guards closing and WaitGroup Add against complete Wait.
	sendMu sync.Mutex
	// sends tracks business writes that may still touch the response writer.
	sends sync.WaitGroup
	// closing rejects new sends before cleanup starts.
	closing bool
	// guards callback registration and terminal state
	callbackMu sync.Mutex
	// serializes heartbeat and business frames
	mu sync.Mutex
	// prevents duplicate terminal callbacks and failure logs
	completed bool
	// drained is true after all in-flight business sends have finished.
	drained bool
	// records a successful header flush so startup errors are not logged twice
	started bool
	// tracks whether the error callback has already run
	errorNotified bool
	// tracks whether the completion callback has already run
	completeNotified bool
}

// newSSEWriter creates the request-scoped stream without committing headers, so
// parameter binding can still answer a normal 400 before streaming starts.
func newSSEWriter(w responseWriter, r *http.Request, writeTimeout time.Duration,
	logger Logger, streamCtx context.Context) *sseWriter {
	ctx, cancel := context.WithCancelCause(r.Context())
	stopShutdown := context.AfterFunc(streamCtx, func() { cancel(streamCtx.Err()) })
	return &sseWriter{
		w:            w,
		ctx:          ctx,
		cancel:       cancel,
		writeTimeout: writeTimeout,
		logger:       logger,
		stopShutdown: stopShutdown,
	}
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
	if err := s.beginSend(); err != nil {
		return err
	}
	defer s.sends.Done()
	if err := context.Cause(s.ctx); err != nil {
		return err
	}
	buf := responseBufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer releaseResponseBuffer(buf)
	if err := serializeSSEValue(buf, v); err != nil {
		return err
	}
	return s.writeData(buf.Bytes())
}

// serializeSSEValue contains panics from user Error and MarshalJSON methods so
// concurrent Send calls cannot escape and terminate the process.
func serializeSSEValue(buf *bytes.Buffer, value any) (err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("SSE value serialization panicked")
		}
	}()
	_, err = convertToBuffer(buf, reflect.ValueOf(value))
	return err
}

// beginSend registers one in-flight business write unless closing has started.
func (s *sseWriter) beginSend() error {
	s.sendMu.Lock()
	if s.closing {
		s.sendMu.Unlock()
		if err := context.Cause(s.ctx); err != nil {
			return err
		}
		return context.Canceled
	}
	s.sends.Add(1)
	s.sendMu.Unlock()
	return nil
}

// beginClosing prevents new sends before cleanup waits for existing ones.
func (s *sseWriter) beginClosing() {
	s.sendMu.Lock()
	s.closing = true
	s.sendMu.Unlock()
}

// Done reports stream cancellation to the user event loop.
func (s *sseWriter) Done() <-chan struct{} { return s.ctx.Done() }

// OnError sets the callback for the first stream failure. A callback registered
// after termination runs once all in-flight sends have drained.
func (s *sseWriter) OnError(fn func(error)) {
	s.callbackMu.Lock()
	s.onError = fn
	if s.completed && s.drained && s.terminalErr != nil && !s.errorNotified && fn != nil {
		s.errorNotified = true
		err := s.terminalErr
		s.callbackMu.Unlock()
		go s.runCallback("error", func() { fn(err) })
		return
	}
	s.callbackMu.Unlock()
}

// OnComplete sets the callback for stream termination. A callback registered
// after termination runs immediately.
func (s *sseWriter) OnComplete(fn func()) {
	s.callbackMu.Lock()
	s.onComplete = fn
	if s.completed && s.drained && !s.completeNotified && fn != nil {
		s.completeNotified = true
		s.callbackMu.Unlock()
		go s.runCallback("complete", fn)
		return
	}
	s.callbackMu.Unlock()
}

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
	if err := http.NewResponseController(s.w).Flush(); err != nil {
		return err
	}
	s.callbackMu.Lock()
	s.started = true
	s.callbackMu.Unlock()
	return nil
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
		if cause := context.Cause(s.ctx); cause != nil && errors.Is(err, cause) {
			// Cancellation is a normal stream exit, not an I/O failure.
			return err
		}
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
		if cause := context.Cause(s.ctx); cause != nil && errors.Is(err, cause) {
			// A canceled queued send never reached the response sink.
			return err
		}
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

// beforeWrite rejects a canceled stream and renews the request write deadline.
// SSE always uses a finite deadline so cleanup can drain blocked writes; a writer
// that cannot set deadlines cannot provide that guarantee and fails startup.
func (s *sseWriter) beforeWrite() error {
	if err := context.Cause(s.ctx); err != nil {
		return err
	}
	timeout := s.writeTimeout
	if timeout <= 0 {
		timeout = sseDefaultWriteTimeout
	}
	return http.NewResponseController(s.w).SetWriteDeadline(time.Now().Add(timeout))
}

// fail records the first non-cancellation stream error as the cancellation cause
// and invokes terminal callbacks once. Callers must return cancellation directly
// rather than reporting it as a stream failure.
func (s *sseWriter) fail(err error) error {
	s.beginClosing()
	if s.stopShutdown != nil {
		s.stopShutdown()
	}
	s.cancel(err)
	s.callbackMu.Lock()
	if s.completed {
		s.callbackMu.Unlock()
		return err
	}
	s.completed = true
	s.terminalErr = err
	started := s.started
	s.callbackMu.Unlock()

	if started && s.logger != nil {
		safeLog(func() { s.logger.Error("SSE stream failed", "error", err) })
	}
	go s.notifyErrorAfterDrain()
	return err
}

// notifyErrorAfterDrain delays OnError until every in-flight Send has finished.
func (s *sseWriter) notifyErrorAfterDrain() {
	s.sends.Wait()
	s.callbackMu.Lock()
	s.drained = true
	if s.terminalErr == nil || s.errorNotified || s.onError == nil {
		s.callbackMu.Unlock()
		return
	}
	s.errorNotified = true
	err := s.terminalErr
	callback := s.onError
	s.callbackMu.Unlock()
	s.runCallback("error", func() { callback(err) })
}

// complete cancels a finished stream, drains business writes and invokes
// OnComplete once.
func (s *sseWriter) complete() {
	s.beginClosing()
	if s.stopShutdown != nil {
		s.stopShutdown()
	}
	s.cancel(nil)
	s.sends.Wait()
	s.callbackMu.Lock()
	s.drained = true
	s.completed = true
	onError := s.onError
	onComplete := s.onComplete
	if s.terminalErr == nil || s.errorNotified {
		onError = nil
	} else if onError != nil {
		s.errorNotified = true
	}
	if s.completeNotified {
		onComplete = nil
	} else if onComplete != nil {
		s.completeNotified = true
	}
	s.callbackMu.Unlock()

	if onError != nil {
		go s.runCallback("error", func() { onError(s.terminalErr) })
	}
	if onComplete != nil {
		go s.runCallback("complete", onComplete)
	}
}

// runCallback isolates user callback panics so a heartbeat goroutine or deferred
// cleanup cannot crash the process.
func (s *sseWriter) runCallback(kind string, fn func()) {
	defer func() {
		if recovered := recover(); recovered != nil {
			if s.logger != nil {
				safeLog(func() {
					s.logger.Error("SSE "+kind+" callback panic", "error", recovered,
						"stack", string(debug.Stack()))
				})
			}
		}
	}()
	fn()
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
