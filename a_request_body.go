// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
)

// This file holds request body limits, buffering, replay, and read tracking.

// ByteSize is a human-readable byte count used by the size setters: the
// exported type name tells a caller, at the signature, that the value is bytes.
type ByteSize int

var (
	// surfaced when an incoming body exceeds the configured cap
	requestBodyTooLargeError = errors.New("request body too large")
	// live total request body cap, read per request
	maxBodySize atomic.Int64
	// live multipart file-content memory threshold, read per request and capped by
	// the per-file upload limit before parsing
	maxFileMemory atomic.Int64
	// live per-file upload cap, read per file
	maxUploadFileSize atomic.Int64
)

// Shared constants: the default limits applied at startup and
// re-applied defensively if a stored value has been corrupted into a
// non-positive number, and the request body media types used by parsing. JSON
// is matched case-insensitively after trimming parameters; forms arrive under
// the W3C spelling.
const (
	// Maximum total request body size: 5 MiB.
	defaultMaxBodySize = 5 << 20
	// Multipart in-memory threshold, not total upload limit.
	defaultMaxFileMemory = 2 << 20
	// Hard cap on each individual uploaded file: 1 MiB.
	defaultMaxUploadFileSize = 1 << 20
	// largest request body buffer retained by the pool
	maxPooledBodyCapacity = 1 << 20

	// body type carrying form fields plus uploaded files
	multipartFormData = "multipart/form-data"
	// body type carrying plain HTML form submissions
	xWwwFormUrlencoded = "application/x-www-form-urlencoded"
)

// init installs the default request and upload limits.
func init() {
	maxBodySize.Store(defaultMaxBodySize)
	maxFileMemory.Store(defaultMaxFileMemory)
	maxUploadFileSize.Store(defaultMaxUploadFileSize)
}

// bodyBufferPool recycles the buffer used to read JSON, URL-encoded and
// multipart request bodies, so repeated requests reuse one backing array. sync.Pool drops its contents at GC, so an
// oversized body buffer is not pinned indefinitely.
var bodyBufferPool = sync.Pool{
	New: func() any { return new(bytes.Buffer) },
}

// releaseBodyBuffer returns a body buffer to the pool. The caller must be done
// with every slice aliasing it: the decoded parameters are independent copies,
// so this is safe once binding has finished.
func releaseBodyBuffer(buf *bytes.Buffer) {
	if buf.Cap() > maxPooledBodyCapacity {
		return
	}
	buf.Reset()
	bodyBufferPool.Put(buf)
}

// SetMaxBodySize sets the hard limit on the total request body size, applied
// to every body method (POST, PUT, PATCH), multipart uploads included. Known
// oversized bodies are rejected with 413 before the handler. Unknown-length
// bodies are bounded while they are read: parameter parsing returns 413, while
// a raw handler observes http.MaxBytesError itself. Panics on a non-positive
// size, and once Run is claimed: the limits are assembling input like everything
// else the frozen request pipeline reads.
func SetMaxBodySize(size ByteSize) {
	unlock := defaultEngine.assertAssembling("SetMaxBodySize")
	if size <= 0 {
		unlock()
		defaultEngine.failStartup("gbl: max body size must be greater than zero")
		return
	}
	maxBodySize.Store(int64(size))
	unlock()
}

// SetMaxFileSize sets the multipart in-memory spill threshold for file content:
// file content up to size stays in memory, and excess spills to temporary files.
// The parser also reserves memory for non-file form fields. This does not limit
// the total upload size; see SetMaxBodySize for that. Panics on a non-positive
// size, and once Run is claimed.
func SetMaxFileSize(size ByteSize) {
	unlock := defaultEngine.assertAssembling("SetMaxFileSize")
	if size <= 0 {
		unlock()
		defaultEngine.failStartup("gbl: multipart memory threshold must be greater than zero")
		return
	}
	maxFileMemory.Store(int64(size))
	unlock()
}

// SetMaxUploadFileSize sets a hard, per-file cap on every uploaded file
// (any field, any part): a multipart file whose size exceeds the cap aborts
// the request with 413, before the handler runs. It defaults to
// defaultMaxUploadFileSize (1 MiB). Distinct from SetMaxFileSize, which is
// only the in-memory spill threshold, and from SetMaxBodySize, which caps the
// total body. Panics on a non-positive size, and once Run is claimed.
func SetMaxUploadFileSize(size ByteSize) {
	unlock := defaultEngine.assertAssembling("SetMaxUploadFileSize")
	if size <= 0 {
		unlock()
		defaultEngine.failStartup("gbl: max upload file size must be greater than zero")
		return
	}
	maxUploadFileSize.Store(int64(size))
	unlock()
}

// replayRequestBody restores a body after bounded buffering on the unsupported-
// media parameter-parsing path and retains the original closer so replacing Body
// does not change its lifecycle. Raw handlers keep the streaming body instead.
type replayRequestBody struct {
	// Reader replays the bounded body bytes.
	*bytes.Reader
	// closer owns the original request body lifecycle.
	closer io.Closer
}

// Close closes the original request body.
func (b *replayRequestBody) Close() error {
	if b.closer == nil {
		return nil
	}
	return b.closer.Close()
}

// preparedRequestBody marks a body already wrapped with the framework limit.
// The wrapper makes boundary preparation idempotent while still allowing a
// middleware-replaced body to be limited again during parameter parsing.
type preparedRequestBody struct {
	io.ReadCloser
}

// prepareRequestBody applies the total body cap before media-type parsing. A
// known oversized length is rejected without reading. Every non-empty body is
// wrapped, not only unknown-length bodies, because middleware may replace or
// expand a body without updating ContentLength. The wrapper keeps raw handlers
// streaming and makes repeated preparation idempotent.
func prepareRequestBody(w http.ResponseWriter, r *http.Request) error {
	method, known := methodOf(r.Method)
	if !known || !method.isBodyMethod() {
		return nil
	}

	limit := maxBodySize.Load()
	if limit <= 0 {
		limit = defaultMaxBodySize
	}
	if r.ContentLength > limit {
		return requestBodyTooLargeError
	}
	if r.Body == nil || r.Body == http.NoBody {
		return nil
	}
	if _, ok := r.Body.(*preparedRequestBody); ok {
		return nil
	}

	r.Body = &preparedRequestBody{ReadCloser: http.MaxBytesReader(w, r.Body, limit)}
	return nil
}

// replayUnsupportedRequestBody buffers an unknown-length body after
// prepareRequestBody has installed MaxBytesReader. The bound keeps pre-reading
// finite; a successful read is replayed for application code, while a read
// failure is classified before any handler can observe the partial body.
func replayUnsupportedRequestBody(r *http.Request) error {
	if r.Body == nil {
		return nil
	}
	unknownLength := r.ContentLength < 0 ||
		(r.ContentLength == 0 && r.Body != http.NoBody)
	if !unknownLength {
		return nil
	}

	original := r.Body
	data, err := io.ReadAll(original)
	if err != nil {
		return classifyBodyReadError(err)
	}
	r.Body = &replayRequestBody{Reader: bytes.NewReader(data), closer: original}
	r.ContentLength = int64(len(data))
	return nil
}
