// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"errors"
	"net/http"
	"strings"
	"sync"
)

// This file is the core request dispatch: match a route, run its frozen
// middleware chain whose terminal endpoint invokes the business handler, then
// write the staged result. Parameter binding lives in binding.go, request
// parsing in request.go, and result serialization in response.go.

// Per-request scratch reused across the whole process. The wrapper holds only
// plain fields and the map only strings, so both reset cheaply and carry no
// request state between uses.
var (
	// responseWriterPool recycles the per-request writer wrapper.
	responseWriterPool = sync.Pool{
		New: func() any { return &pooledResponseWriter{} },
	}
	// pathParamsPool recycles the map of captured path parameters, sized for
	// typical routes.
	pathParamsPool = sync.Pool{
		New: func() any { return make(map[string]string, 4) },
	}
)

// recycle clears all per-request state — including the wrapped writer and any
// hijack marker — so the pool never pins a finished request or leaks connection
// ownership into the next one. A named method deferred directly (defer
// rw.recycle()) avoids the heap-allocated closure that defer func(){...}() would
// cost on every request.
func (rw *pooledResponseWriter) recycle() {
	*rw = pooledResponseWriter{}
	responseWriterPool.Put(rw)
}

// releasePathParams empties and recycles a pooled path-parameter map.
func releasePathParams(params map[string]string) {
	clear(params)
	pathParamsPool.Put(params)
}

// serveHTTP is the full request pipeline: method check, route match, HEAD
// fallback to GET, then the route's frozen middleware chain, whose terminal
// endpoint runs the business handler; finally the staged result is written.
// Any panic raised below is recovered and rendered by the deferred
// catchRuntimePanic.
//
// The response wrapper and Context are drawn from pools and returned on every
// exit path. Dynamic routes also borrow a path-parameter map, while static hits
// avoid that pool entirely. The wrapper handed to middleware and handlers
// therefore must not be retained past the request.
func (e *engine) serveHTTP(w http.ResponseWriter, r *http.Request) {
	rw := responseWriterPool.Get().(*pooledResponseWriter)
	*rw = pooledResponseWriter{w: w, hijacks: e.hijacks}

	// Registered first so it runs last: the recover/deferred render below still
	// needs rw.w, and only afterward may the wrapper be cleared and recycled.
	defer rw.recycle()
	defer e.catchRuntimePanic(rw)

	method, known := methodOf(r.Method)
	if !known {
		// TRACE, CONNECT and custom verbs are not implemented at all, so this is
		// not a routing failure and 405 would be the wrong answer.
		abort(http.StatusNotImplemented, http.StatusText(http.StatusNotImplemented))
	}

	rh, pathParams := e.matchNode(method, r.URL.Path, nil)

	// An unregistered HEAD borrows the GET route: the handler runs normally and
	// net/http drops the body while keeping Content-Length.
	if rh == nil && method == methodHead {
		rh, pathParams = e.matchNode(methodGet, r.URL.Path, pathParams)
	}
	if pathParams != nil {
		defer releasePathParams(pathParams)
	}

	if rh == nil {
		e.abortUnmatched(rw, method, r.URL.Path)
		return
	}

	c := newContext(rw, r, pathParams, rh, method, e.logger)
	defer c.recycle()

	// Return-value handlers commit a provisional media type before middleware or
	// the handler can flush. Runtime serialization may still refine it before the
	// automatic write when the response has not been flushed.
	if rh.t.NumOut() > 0 {
		rw.Header().Set(contentType, rh.contentType)
	}

	// Multipart temp files, if any were parsed, are removed once the whole chain
	// has completed, whatever the outcome and including a panic unwinding it.
	defer cleanupMultipart(r)

	c.Next()

	// The chain has fully unwound. If the handler produced a result and nothing
	// wrote a response — the handler directly or a post-handler middleware —
	// render the staged body with the content type chosen at registration.
	//
	// Write through rw, not the raw w: middleware (or the framework) may have
	// already recorded a status via rw.WriteHeader, and that choice must survive.
	// rw.WriteHeader is a no-op once a status is recorded, so the 200 here only
	// applies as a default when nothing set one, and rw.Write flushes the recorded
	// status before the body.
	if c.handled && !rw.written() && !rw.hijacked() && bodyAllowedForStatus(rw.status()) {
		if rh.t.NumOut() == 0 || c.contentType != rh.contentType {
			rw.Header().Set(contentType, c.contentType)
		}
		if !rw.headed() {
			rw.WriteHeader(http.StatusOK)
		}
		_, _ = rw.Write(c.bytes)
	}
}

// cleanupMultipart releases any temporary files a parsed multipart request left
// behind. It is deferred once per request and is a no-op unless the body was
// actually parsed as multipart, so removing files is not actionable here: the
// response is already decided and removal failures are silently dropped.
func cleanupMultipart(r *http.Request) {
	if r.MultipartForm != nil {
		_ = r.MultipartForm.RemoveAll()
	}
}

// abortUnmatched answers a request that matched no route, telling "no such
// path" apart from "path exists, wrong verb".
func (e *engine) abortUnmatched(rw *pooledResponseWriter, method httpMethod, path string) {
	allowed := e.allowFor(path)
	if len(allowed) == 0 {
		abort(http.StatusNotFound, http.StatusText(http.StatusNotFound))
	}

	rw.Header().Set(allow, strings.Join(allowed, headerComma))

	if method == methodOptions {
		// OPTIONS succeeded, and a 204 must not carry a body, so the recorded
		// status is flushed here instead of going through the panic path.
		rw.WriteHeader(http.StatusNoContent)
		rw.writeHeader()
		return
	}

	abort(http.StatusMethodNotAllowed, http.StatusText(http.StatusMethodNotAllowed))
}

// writeRequestError maps a request-parsing failure to its status code and
// aborts the pipeline. Every error reaching here describes the client's own
// malformed request (bad Content-Type, unparseable body, oversize payload), so
// its message is safe to send back.
func writeRequestError(err error) {
	if errors.Is(err, requestBodyTooLargeError) {
		abort(http.StatusRequestEntityTooLarge, http.StatusText(http.StatusRequestEntityTooLarge))
	}
	abort(http.StatusBadRequest, err.Error())
}
