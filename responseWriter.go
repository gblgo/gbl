// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"bufio"
	"net"
	"net/http"
	"strconv"
	"sync"
)

// responseWriter is the internal response sink of gbl, handed to handlers,
// middleware and the panic path. It embeds http.ResponseWriter and adds
// unexported accessors the framework uses to coordinate deferred writeback —
// whether a status or body has been committed, the effective status and the
// byte count.
// The type stays unexported so none of those internals surface in the public api.
type responseWriter interface {
	http.ResponseWriter
	// reports whether any body byte has been written
	written() bool
	// reports whether the final status has reached the underlying writer
	flushed() bool
	// reports whether connection ownership has been transferred
	hijacked() bool
	// reports whether a status has been recorded
	headed() bool
	// flushes the recorded status exactly once
	writeHeader()
	// effective status, defaulting to 200
	status() int
	// body bytes written so far
	size() int
}

// pooledResponseWriter is the pooled wrapper the engine hands down. It buffers the
// status and byte count so a panic or post-handler middleware can still flush the
// right status, and mirrors net/http's rule that the first body write commits
// whatever status was set beforehand.
type pooledResponseWriter struct {
	// 16-byte fields (interface and pointer)
	// wrapped sink, reset per request; never retained across requests
	w http.ResponseWriter
	// registry that owns hijacked connections until user Close or Shutdown
	hijacks *hijackRegistry

	// 8-byte fields (int)
	// recorded status code
	statusCode int
	// total body bytes passed to the underlying writer
	bytesWritten int

	// 1-byte fields (bool)
	// a body byte has been written
	writtenStatus bool
	// WriteHeader has recorded a status
	headedStatus bool
	// the status has reached the underlying writer
	flushedStatus bool
	// connection ownership has been transferred by Hijack
	hijackedStatus bool
}

// hijackRegistry tracks connections removed from net/http by a successful Hijack.
type hijackRegistry struct {
	// mu guards the tracked connections and closed state.
	mu sync.Mutex
	// live user-owned connections.
	connections map[*hijackedConn]struct{}
	// closed rejects new Hijack calls after shutdown starts closing connections.
	closed bool
}

// hijackedConn removes itself from the registry when the user closes it.
type hijackedConn struct {
	// Conn is the underlying user-owned connection.
	net.Conn
	// registry is the owner that must stop tracking this connection.
	registry *hijackRegistry
}

// newHijackRegistry creates an empty connection registry.
func newHijackRegistry() *hijackRegistry {
	return &hijackRegistry{connections: make(map[*hijackedConn]struct{})}
}

// track registers conn unless shutdown has already closed the registry.
func (r *hijackRegistry) track(conn net.Conn) (net.Conn, bool) {
	if conn == nil {
		return nil, false
	}
	tracked := &hijackedConn{Conn: conn, registry: r}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		_ = conn.Close()
		return nil, false
	}
	r.connections[tracked] = struct{}{}
	r.mu.Unlock()
	return tracked, true
}

// remove stops tracking a connection closed by its user.
func (r *hijackRegistry) remove(conn *hijackedConn) {
	r.mu.Lock()
	delete(r.connections, conn)
	r.mu.Unlock()
}

// closeAll rejects new connections and closes every currently tracked connection.
func (r *hijackRegistry) closeAll() {
	r.mu.Lock()
	r.closed = true
	connections := make([]*hijackedConn, 0, len(r.connections))
	for conn := range r.connections {
		connections = append(connections, conn)
	}
	clear(r.connections)
	r.mu.Unlock()

	for _, conn := range connections {
		_ = conn.Conn.Close()
	}
}

// Close removes the connection from its registry before closing it.
func (c *hijackedConn) Close() error {
	c.registry.remove(c)
	return c.Conn.Close()
}

// WriteHeader forwards informational responses immediately and records the first
// final status for deferred writeback. A protocol switch is final and is sent at
// once because no regular response body follows it.
func (rw *pooledResponseWriter) WriteHeader(code int) {
	if rw.headedStatus || rw.flushedStatus || rw.hijackedStatus {
		return
	}
	if code < 100 || code > 999 {
		panic("invalid WriteHeader code " + strconv.Itoa(code))
	}

	if code == http.StatusSwitchingProtocols {
		rw.headedStatus = true
		rw.statusCode = code
		rw.writeHeader()
		return
	}

	if code < 200 {
		rw.w.WriteHeader(code)
		return
	}

	rw.headedStatus = true
	rw.statusCode = code
}

// Header exposes the underlying writer's live header map. Headers are not staged
// like the status and byte count: net/http snapshots them when the status is
// flushed, so they must already be on the underlying writer by then.
func (rw *pooledResponseWriter) Header() http.Header {
	return rw.w.Header()
}

// Write flushes any recorded status before the body, so a status set earlier
// (for example by a middleware) is not silently downgraded to 200 once the
// first byte commits the response.
func (rw *pooledResponseWriter) Write(data []byte) (int, error) {
	if rw.hijackedStatus {
		return 0, http.ErrHijacked
	}
	rw.writtenStatus = true
	rw.writeHeader()
	n, err := rw.w.Write(data)
	rw.bytesWritten += n
	return n, err
}

// status reports the response code as it stands or was sent: the recorded one
// once WriteHeader has run, otherwise net/http's implicit 200 default.
func (rw *pooledResponseWriter) status() int {
	if rw.headedStatus {
		return rw.statusCode
	}
	return http.StatusOK
}

// size reports how many body bytes have been written so far.
func (rw *pooledResponseWriter) size() int { return rw.bytesWritten }

// written reports whether any body byte has been committed to the wire.
func (rw *pooledResponseWriter) written() bool {
	return rw.writtenStatus
}

// flushed reports whether the final status has reached the underlying writer.
func (rw *pooledResponseWriter) flushed() bool {
	return rw.flushedStatus
}

// hijacked reports whether Hijack transferred connection ownership.
func (rw *pooledResponseWriter) hijacked() bool {
	return rw.hijackedStatus
}

// headed reports whether a status code has been recorded.
func (rw *pooledResponseWriter) headed() bool {
	return rw.headedStatus
}

// writeHeader sends the final status exactly once, defaulting to 200 when a body
// or flush commits the response before an explicit final status.
func (rw *pooledResponseWriter) writeHeader() {
	if rw.flushedStatus || rw.hijackedStatus {
		return
	}
	rw.flushedStatus = true

	if !rw.headedStatus {
		rw.headedStatus = true
		rw.statusCode = http.StatusOK
	}
	rw.w.WriteHeader(rw.statusCode)
}

// Flush commits the current status and forwards to the underlying flusher when
// the sink supports streaming.
func (rw *pooledResponseWriter) Flush() {
	_ = rw.FlushError()
}

// FlushError lets http.ResponseController distinguish a sink that cannot flush
// from one whose flush succeeded.
func (rw *pooledResponseWriter) FlushError() error {
	if rw.hijackedStatus {
		return http.ErrHijacked
	}
	if flusher, ok := rw.w.(interface{ FlushError() error }); ok {
		rw.writeHeader()
		return flusher.FlushError()
	}
	if flusher, ok := rw.w.(http.Flusher); ok {
		rw.writeHeader()
		flusher.Flush()
		return nil
	}
	return http.ErrNotSupported
}

// Hijack forwards connection ownership when the underlying sink supports it.
func (rw *pooledResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if rw.hijackedStatus {
		return nil, nil, http.ErrHijacked
	}
	hijacker, ok := rw.w.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	conn, buffered, err := hijacker.Hijack()
	if err != nil {
		return nil, nil, err
	}
	rw.hijackedStatus = true
	if rw.hijacks != nil && conn != nil {
		tracked, ok := rw.hijacks.track(conn)
		if !ok {
			return nil, nil, http.ErrServerClosed
		}
		conn = tracked
	}
	return conn, buffered, nil
}

// Push forwards HTTP/2 server pushes when the underlying sink supports them.
func (rw *pooledResponseWriter) Push(target string, opts *http.PushOptions) error {
	pusher, ok := rw.w.(http.Pusher)
	if !ok {
		return http.ErrNotSupported
	}
	return pusher.Push(target, opts)
}

// Unwrap returns the wrapped writer so http.ResponseController can reach
// capabilities this wrapper does not forward, such as SetWriteDeadline.
func (rw *pooledResponseWriter) Unwrap() http.ResponseWriter {
	return rw.w
}
