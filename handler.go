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
// fallback to GET, raw-body limit preparation, then the frozen route middleware
// chain, whose terminal endpoint runs the business handler; finally the staged
// result is written. Any panic raised below is recovered and rendered by the
// deferred catchRuntimePanic.
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

	// An unregistered HEAD borrows the GET route: ordinary handlers run normally
	// and net/http drops the body, while SSE routes only commit their headers.
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

	// Raw handlers do not enter parameter parsing. Reject a known oversized body
	// before application code runs, but leave an unknown-length body unread and
	// wrap it with MaxBytesReader so the handler can stream up to the same cap.
	if method.isBodyMethod() && !rh.plan.needsParse {
		if err := prepareRequestBody(w, r); err != nil {
			writeRequestError(err, e.logger)
			return
		}
	}

	c := newContext(rw, r, pathParams, rh, method, e.logger)
	defer c.recycle()

	// Return-value handlers expose a provisional media type before middleware or
	// the handler can flush. Serialization replaces it immediately after staging;
	// interface returns defer an in-handler flush until that replacement is known.
	if rh.t.NumOut() > 0 {
		rw.Header().Set(contentType, rh.contentType)
	}

	// Multipart temp files are removed by Context.recycle using the original
	// request pointer, even if middleware later replaces c.Request.

	c.Next()

	if !c.handled && rw.headed() && !rw.flushed() {
		rw.writeHeader()
	}

	// The chain has fully unwound. If the handler produced a result and nothing
	// wrote a response — the handler directly or a post-handler middleware —
	// render the staged body. Its runtime Content-Type was set just after
	// serialization; a no-return handler keeps an explicit header or gets the
	// plain-text default when none was supplied.
	//
	// Write through rw, not the raw w: middleware (or the framework) may have
	// already recorded a status via rw.WriteHeader, and that choice must survive.
	// rw.WriteHeader is a no-op once a status is recorded, so the 200 here only
	// applies as a default when nothing set one, and rw.Write flushes the recorded
	// status before the body.
	if c.handled && !c.skipWriteback && !rw.written() && !rw.hijacked() {
		if !bodyAllowedForStatus(rw.status()) {
			rw.writeHeader()
		} else {
			if rh.t.NumOut() == 0 && rw.Header().Get(contentType) == empty {
				rw.Header().Set(contentType, c.contentType)
			}
			if !rw.headed() {
				rw.WriteHeader(http.StatusOK)
			}
			_, _ = rw.Write(c.bytes)
		}
	}
}

// cleanupMultipart releases temporary files from the original parsed request.
// Cleanup is best-effort: the response has already been decided, so removal
// failures cannot change it and are intentionally ignored.
func cleanupMultipart(r *http.Request) {
	if r != nil && r.MultipartForm != nil {
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

// writeRequestError maps a request failure to a client-safe response. Explicit
// client parse failures retain their useful 400 message, while internal failures
// are logged through the supplied engine logger (or the default) and hidden
// behind a fixed 500.
func writeRequestError(err error, loggers ...Logger) {
	var maxBytesErr *http.MaxBytesError
	if errors.Is(err, requestBodyTooLargeError) || errors.As(err, &maxBytesErr) {
		abort(http.StatusRequestEntityTooLarge, http.StatusText(http.StatusRequestEntityTooLarge))
		return
	}
	status, body := requestErrorResponse(err)
	if status == http.StatusInternalServerError {
		logger := defaultEngine.logger
		if len(loggers) > 0 && loggers[0] != nil {
			logger = loggers[0]
		}
		if logger != nil {
			logger.Error("request parsing failed", "error", err)
		}
	}
	abort(status, body)
}
