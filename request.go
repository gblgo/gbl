// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"mime"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
)

// This file holds request parsing: turning the inbound request into bindable
// form values, uploaded files and a JSON body buffer, plus the size limits that
// guard it. The binders that consume these inputs live in binding.go.

// ByteSize is a human-readable byte count used by the size setters: the
// exported type name tells a caller, at the signature, that the value is bytes.
type ByteSize int

var (
	requestBodyTooLargeError = errors.New("request body too large")
	maxBodySize              atomic.Int64
	maxFileMemory            atomic.Int64
	maxUploadFileSize        atomic.Int64
)

// Default limits applied at startup and re-applied defensively if a stored
// value has been corrupted into a non-positive number.
const (
	defaultMaxBodySize       = 5 << 20 // Maximum total request body size: 5 MiB.
	defaultMaxFileMemory     = 2 << 20 // Multipart in-memory threshold, not total upload limit.
	defaultMaxUploadFileSize = 1 << 20 // Hard cap on each individual uploaded file: 1 MiB.
)

func init() {
	maxBodySize.Store(defaultMaxBodySize)
	maxFileMemory.Store(defaultMaxFileMemory)
	maxUploadFileSize.Store(defaultMaxUploadFileSize)
}

// bodyBufferPool recycles the buffer used to read a JSON request body, so a
// small-body request reuses one backing array instead of allocating a fresh
// read buffer every time. sync.Pool drops its contents at GC, so an oversized
// body buffer is not pinned indefinitely.
var bodyBufferPool = sync.Pool{
	New: func() any { return new(bytes.Buffer) },
}

// releaseBodyBuffer returns a body buffer to the pool. The caller must be done
// with every slice aliasing it: the decoded parameters are independent copies,
// so this is safe once binding has finished.
func releaseBodyBuffer(buf *bytes.Buffer) {
	buf.Reset()
	bodyBufferPool.Put(buf)
}

// SetMaxBodySize sets the hard limit on the total request body size, applied
// to every body method (POST, PUT, PATCH), multipart uploads included. A body
// over the limit is rejected with 413. Panics on a non-positive size.
func SetMaxBodySize(size ByteSize) {
	if size <= 0 {
		defaultEngine.failStartup("gbl: max body size must be greater than zero")
	}
	maxBodySize.Store(int64(size))
}

// SetMaxFileSize sets the multipart in-memory threshold: file parts smaller
// than size stay in memory, larger ones spill to temporary files. It does not
// limit the total upload size; see SetMaxBodySize for that. Panics on a
// non-positive size.
func SetMaxFileSize(size ByteSize) {
	if size <= 0 {
		defaultEngine.failStartup("gbl: multipart memory threshold must be greater than zero")
	}
	maxFileMemory.Store(int64(size))
}

// SetMaxUploadFileSize sets a hard, per-file cap on every uploaded file
// (any field, any part): a multipart file whose size exceeds the cap aborts
// the request with 413, before the handler runs. It defaults to
// defaultMaxUploadFileSize (1 MiB). Distinct from SetMaxFileSize, which is
// only the in-memory spill threshold, and from SetMaxBodySize, which caps the
// total body. Panics on a non-positive size.
func SetMaxUploadFileSize(size ByteSize) {
	if size <= 0 {
		defaultEngine.failStartup("gbl: max upload file size must be greater than zero")
	}
	maxUploadFileSize.Store(int64(size))
}

// mergeFormBodyWins merges URL query values with parsed body values, letting the
// body win on any key present in both: a POST/PUT/PATCH form field overrides a
// query parameter of the same name. Query-only keys remain available. When either
// side is empty the other is returned as-is, so the common query-less POST adds
// no allocation.
func mergeFormBodyWins(query, body url.Values) url.Values {
	if len(query) == 0 {
		return body
	}
	if len(body) == 0 {
		return query
	}
	merged := make(url.Values, len(query)+len(body))
	maps.Copy(merged, query)
	maps.Copy(merged, body)
	return merged
}

// queryValues returns the parsed URL query, or nil when the request carries no
// query string. Returning nil avoids the empty map url.URL.Query() allocates for
// an empty RawQuery; form lookups and mergeFormBodyWins already treat nil as "no
// query values".
func queryValues(r *http.Request) url.Values {
	if r.URL.RawQuery == null {
		return nil
	}
	return r.URL.Query()
}

// parseRequestParameters collects handler-bindable inputs for the request:
// form values, uploaded files, the JSON body buffer and the parsed media type.
// The body buffer is pooled; the caller must release it with releaseBodyBuffer
// once binding is done, since the decoded parameters are independent copies.
func parseRequestParameters(w http.ResponseWriter, r *http.Request, method httpMethod) (url.Values, filesMap, *bytes.Buffer, string, error) {
	// GBL deliberately applies a strict parameter-source policy by HTTP method.
	// GET, HEAD, DELETE and OPTIONS may read only URL path parameters (matched by
	// the router) and URL query parameters. Their request bodies are never parsed
	// or read, regardless of Content-Type.
	//
	// Only POST, PUT and PATCH may carry a body GBL reads; every other method
	// exposes query parameters and leaves the body untouched.
	if !method.isBodyMethod() {
		// Only path and query parameters bind for these methods; the body is never
		// read. queryValues yields nil for an empty query, so no empty map is
		// allocated and every form lookup treats nil as "not found".
		return queryValues(r), nil, nil, null, nil
	}

	var (
		form      url.Values
		files     filesMap
		bodyBuf   *bytes.Buffer
		mediaType string
	)

	// Apply one hard limit to the entire request body, including multipart files.
	// The fallback re-applies the default if the stored value was corrupted.
	limit := maxBodySize.Load()
	if limit <= 0 {
		limit = defaultMaxBodySize
	}
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, limit)
	}

	contentTypeHeader := r.Header.Get(contentType)
	if contentTypeHeader != null {
		// Fast path: a bare "application/json" or "application/x-www-form-urlencoded"
		// has no parameters, so it skips mime parsing. Anything else (charset,
		// boundary, +json suffix) still goes through ParseMediaType.
		switch contentTypeHeader {
		case applicationJson:
			mediaType = applicationJson
		case xWwwFormUrlencoded:
			mediaType = xWwwFormUrlencoded
		default:
			var err error
			mediaType, _, err = mime.ParseMediaType(contentTypeHeader)
			if err != nil {
				return nil, nil, nil, null, fmt.Errorf("invalid Content-Type: %w", err)
			}
		}
	}

	switch {
	case isJSONMediaType(mediaType):
		var err error
		if r.Body != nil {
			buf := bodyBufferPool.Get().(*bytes.Buffer)
			buf.Reset()
			if _, err = buf.ReadFrom(r.Body); err != nil {
				releaseBodyBuffer(buf)
			} else {
				bodyBuf = buf
			}
		}
		if err != nil {
			if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
				return nil, nil, nil, null, requestBodyTooLargeError
			}
			return nil, nil, nil, null, err
		}

	case mediaType == multipartFormData:
		memory := maxFileMemory.Load()
		if memory <= 0 {
			memory = defaultMaxFileMemory
		}
		if err := r.ParseMultipartForm(memory); err != nil {
			if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
				return nil, nil, nil, null, requestBodyTooLargeError
			}
			return nil, nil, nil, null, err
		}
		// Body text fields win over query on shared keys; query-only keys survive.
		form = mergeFormBodyWins(queryValues(r), r.PostForm)
		if r.MultipartForm != nil {
			files = r.MultipartForm.File
		}
		// Per-file hard cap: any uploaded part larger than the configured
		// threshold aborts the request before the handler runs. Re-applied
		// defensively if the stored value has been corrupted to a non-positive one.
		maxUpload := maxUploadFileSize.Load()
		if maxUpload <= 0 {
			maxUpload = defaultMaxUploadFileSize
		}
		for _, headers := range files {
			for _, hdr := range headers {
				if hdr != nil && hdr.Size > maxUpload {
					return nil, nil, nil, null, requestBodyTooLargeError
				}
			}
		}

	case mediaType == xWwwFormUrlencoded || mediaType == null:
		if err := r.ParseForm(); err != nil {
			if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
				return nil, nil, nil, null, requestBodyTooLargeError
			}
			return nil, nil, nil, null, err
		}
		// Body fields win over query on shared keys; query-only keys survive.
		form = mergeFormBodyWins(queryValues(r), r.PostForm)

	default:
		// Unsupported body media types are not interpreted as parameters.
		// Query parameters are still valid for POST and PUT.
		form = queryValues(r)
	}

	return form, files, bodyBuf, mediaType, nil
}
