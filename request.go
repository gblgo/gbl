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
	"slices"
	"strings"
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
	// surfaced when an incoming body exceeds the configured cap
	requestBodyTooLargeError = errors.New("request body too large")
	// live total request body cap, read per request
	maxBodySize atomic.Int64
	// live multipart file-content memory threshold, read per request
	maxFileMemory atomic.Int64
	// live per-file upload cap, read per file
	maxUploadFileSize atomic.Int64
)

// Constants used only by this file: the default limits applied at startup and
// re-applied defensively if a stored value has been corrupted into a
// non-positive number, and the request body media types this file parses. JSON
// is matched case-insensitively after trimming parameters; forms arrive under
// the W3C spelling.
const (
	// Maximum total request body size: 5 MiB.
	defaultMaxBodySize = 5 << 20
	// Multipart in-memory threshold, not total upload limit.
	defaultMaxFileMemory = 2 << 20
	// Hard cap on each individual uploaded file: 1 MiB.
	defaultMaxUploadFileSize = 1 << 20
	// largest JSON body buffer retained by the pool
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
	if buf.Cap() > maxPooledBodyCapacity {
		return
	}
	buf.Reset()
	bodyBufferPool.Put(buf)
}

// SetMaxBodySize sets the hard limit on the total request body size, applied
// to every body method (POST, PUT, PATCH), multipart uploads included. A body
// over the limit is rejected with 413. Panics on a non-positive size, and
// once the default engine is serving: the limits are assembling input like
// everything else the frozen request pipeline reads.
func SetMaxBodySize(size ByteSize) {
	defaultEngine.assertAssembling("SetMaxBodySize")
	if size <= 0 {
		defaultEngine.failStartup("gbl: max body size must be greater than zero")
	}
	maxBodySize.Store(int64(size))
}

// SetMaxFileSize sets the multipart in-memory threshold for file content in
// aggregate: file content up to size stays in memory, and excess spills to
// temporary files. It does not limit the total upload size; see SetMaxBodySize
// for that. Panics on a non-positive size, and once the default engine is serving.
func SetMaxFileSize(size ByteSize) {
	defaultEngine.assertAssembling("SetMaxFileSize")
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
// total body. Panics on a non-positive size, and once the default engine is
// serving.
func SetMaxUploadFileSize(size ByteSize) {
	defaultEngine.assertAssembling("SetMaxUploadFileSize")
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
// query string. Returning nil avoids the empty map that URL query parsing allocates
// an empty RawQuery; form lookups and mergeFormBodyWins already treat nil as "no
// query values".
func queryValues(r *http.Request) url.Values {
	if r.URL.RawQuery == empty {
		return nil
	}
	return r.URL.Query()
}

// queryValuesFor parses only the exact keys a handler can bind. It preserves
// percent decoding, repeated values and the standard treatment of malformed
// pairs while avoiding a map entry for every unrelated query key.
func queryValuesFor(r *http.Request, keys []string) url.Values {
	if len(keys) == 0 || r.URL.RawQuery == empty {
		return nil
	}
	// The standard parser is faster for a single pair, so use it unless the
	// query contains multiple pairs.
	if !strings.Contains(r.URL.RawQuery, "&") {
		return queryValues(r)
	}
	var values url.Values
	for part := range strings.SplitSeq(r.URL.RawQuery, "&") {
		if part == empty {
			continue
		}
		rawKey, rawValue, _ := strings.Cut(part, "=")
		if strings.Contains(rawKey, ";") {
			continue
		}
		key, err := url.QueryUnescape(rawKey)
		if err != nil || !slices.Contains(keys, key) {
			continue
		}
		value, err := url.QueryUnescape(rawValue)
		if err != nil {
			continue
		}
		if values == nil {
			values = make(url.Values)
		}
		values[key] = append(values[key], value)
	}
	return values
}

// parseRequestParameters collects handler-bindable inputs for the request:
// form values, uploaded files, the JSON body buffer and whether a JSON body
// with actual content is present. queryKeys limits query parsing where the full
// query does not need to be exposed through Request.Form. The body buffer is
// pooled; the caller must release it with releaseBodyBuffer once binding is done,
// since the decoded parameters are independent copies. The returned flag already
// folds in the non-empty check, so a whitespace-only JSON body fails here and a
// body that passed is never scanned again downstream.
func parseRequestParameters(w http.ResponseWriter, r *http.Request,
	method httpMethod, queryKeys []string) (url.Values, filesMap, *bytes.Buffer, bool, error) {
	// GBL deliberately applies a strict parameter-source policy by HTTP method.
	// GET, HEAD, DELETE and OPTIONS may read only URL path parameters (matched by
	// the router) and URL query parameters. Their request bodies are never parsed
	// or read, regardless of Content-Type.
	//
	// Only POST, PUT and PATCH may carry a body GBL reads; every other method
	// exposes query parameters and leaves the body untouched.
	if !method.isBodyMethod() {
		// Only path and query parameters bind for these methods; the body is never
		// read. A single query pair uses the standard parser directly, while a
		// larger query can restrict parsing to bindable names.
		var form url.Values
		if len(queryKeys) > 0 {
			if strings.Contains(r.URL.RawQuery, "&") {
				form = queryValuesFor(r, queryKeys)
			} else {
				form = queryValues(r)
			}
		}
		return form, nil, nil, false, nil
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
	// A known body within the limit cannot exceed it, so avoid allocating a
	// MaxBytesReader. Unknown lengths and oversized known bodies still need the
	// wrapper to preserve the 413 path.
	if r.Body != nil && (r.ContentLength < 0 || r.ContentLength > limit) {
		r.Body = http.MaxBytesReader(w, r.Body, limit)
	}

	contentTypeHeader := r.Header.Get(contentType)
	if contentTypeHeader != empty {
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
				return nil, nil, nil, false, fmt.Errorf("invalid Content-Type: %w", err)
			}
		}
	}

	jsonMedia := isJSONMediaType(mediaType)
	switch {
	case jsonMedia:
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
				return nil, nil, nil, false, requestBodyTooLargeError
			}
			return nil, nil, nil, false, err
		}
		if bodyBuf != nil && !hasNonWhitespace(bodyBuf.Bytes()) {
			releaseBodyBuffer(bodyBuf)
			return nil, nil, nil, false, errors.New("JSON body is empty")
		}

	case mediaType == multipartFormData:
		memory := maxFileMemory.Load()
		if memory <= 0 {
			memory = defaultMaxFileMemory
		}
		if err := r.ParseMultipartForm(memory); err != nil {
			if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
				return nil, nil, nil, false, requestBodyTooLargeError
			}
			return nil, nil, nil, false, err
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
					return nil, nil, nil, false, requestBodyTooLargeError
				}
			}
		}

	case mediaType == xWwwFormUrlencoded || mediaType == empty:
		// The body is decoded directly rather than through r.ParseForm because
		// ParseForm would parse the URL query a second time: queryValues below
		// owns that single parse. When installed above, MaxBytesReader still caps
		// the read and surfaces its error identically.
		var bodyValues url.Values
		if r.Body != nil {
			buf := bodyBufferPool.Get().(*bytes.Buffer)
			buf.Reset()
			if r.ContentLength > int64(buf.Cap()) && r.ContentLength <= limit {
				buf.Grow(int(r.ContentLength))
			}
			if _, readErr := buf.ReadFrom(r.Body); readErr != nil {
				releaseBodyBuffer(buf)
				if _, ok := errors.AsType[*http.MaxBytesError](readErr); ok {
					return nil, nil, nil, false, requestBodyTooLargeError
				}
				return nil, nil, nil, false, readErr
			}
			parsed, parseErr := url.ParseQuery(string(buf.Bytes()))
			releaseBodyBuffer(buf)
			if parseErr != nil {
				return nil, nil, nil, false, parseErr
			}
			bodyValues = parsed
		}
		// Body fields win over query on shared keys; query-only keys survive.
		form = mergeFormBodyWins(queryValues(r), bodyValues)
		// Keep the fields r.ParseForm would have filled, so application code
		// reading them directly sees the same state as before.
		r.PostForm = bodyValues
		r.Form = form

	default:
		// Unsupported body media types are not interpreted as parameters.
		// Query parameters are still valid for POST and PUT.
		if len(queryKeys) > 0 {
			if strings.Contains(r.URL.RawQuery, "&") {
				form = queryValuesFor(r, queryKeys)
			} else {
				form = queryValues(r)
			}
		}
	}

	return form, files, bodyBuf, jsonMedia && bodyBuf != nil, nil
}
