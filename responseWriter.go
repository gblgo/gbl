// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import "net/http"

// responseWriter is the internal response sink of gbl, handed to handlers,
// middleware and the panic path. It embeds http.ResponseWriter and adds
// unexported accessors the framework uses to coordinate deferred writeback —
// whether a status or body has been committed, the effective status and the
// byte count.
// The type stays unexported so none of those internals surface in the public api.
type responseWriter interface {
	http.ResponseWriter
	written() bool // reports whether any body byte has been written
	headed() bool  // reports whether a status has been recorded
	writeHeader()  // flushes the recorded status exactly once
	status() int   // effective status, defaulting to 200
	size() int     // body bytes written so far
}

// pooledResponseWriter is the pooled wrapper the engine hands down. It buffers the
// status and byte count so a panic or post-handler middleware can still flush the
// right status, and mirrors net/http's rule that the first body write commits
// whatever status was set beforehand.
type pooledResponseWriter struct {
	// 16-byte fields (interface)
	w http.ResponseWriter // wrapped sink, reset per request; never retained across requests

	// 8-byte fields (int)
	statusCode   int // recorded status code
	bytesWritten int // total body bytes passed to the underlying writer

	// 1-byte fields (bool)
	writtenStatus bool // a body byte has been written
	headedStatus  bool // WriteHeader has recorded a status
	flushedStatus bool // the status has reached the underlying writer
}

// WriteHeader records the status. It reaches the wire only when the header is
// flushed, by Write or an explicit writeHeader, mirroring net/http, where the
// first Write commits whatever status was set beforehand.
func (rw *pooledResponseWriter) WriteHeader(code int) {
	if rw.headedStatus {
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

// headed reports whether a status code has been recorded.
func (rw *pooledResponseWriter) headed() bool {
	return rw.headedStatus
}

// writeHeader sends the recorded status to the underlying writer exactly once;
// with no recorded status it defaults to 200, as net/http does. The guard makes
// it safe to call before a Write that would otherwise flush again.
func (rw *pooledResponseWriter) writeHeader() {
	if rw.flushedStatus {
		return
	}
	rw.flushedStatus = true

	if rw.headedStatus {
		rw.w.WriteHeader(rw.statusCode)
		return
	}
	rw.w.WriteHeader(http.StatusOK)
}
