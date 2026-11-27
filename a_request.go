// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"unsafe"
)

// This file holds request parsing: turning the inbound request into bindable
// form values, uploaded files and a JSON body buffer, plus the size limits that
// guard it. The binders that consume these inputs live in a_binding.go.

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

// bodyBufferPool recycles the buffer used to read JSON and URL-encoded request
// bodies, so a small-body request reuses one backing array instead of allocating
// a fresh read buffer every time. sync.Pool drops its contents at GC, so an
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

// requestClientError marks malformed client input whose message may be returned
// with HTTP 400. requestInternalError marks a body-read failure; its details are
// for server logs only and must be answered with a generic HTTP 500.
type requestClientError struct {
	// err is the client-safe parse failure.
	err error
}

// Error returns the client-safe parse message.
func (e *requestClientError) Error() string { return e.err.Error() }

// Unwrap exposes the underlying parse error for errors.Is and errors.As.
func (e *requestClientError) Unwrap() error { return e.err }

// requestInternalError marks a server-side body read or storage failure.
type requestInternalError struct {
	// err is retained for server logs and hidden from the client.
	err error
}

// Error returns the internal failure detail for server-side logging.
func (e *requestInternalError) Error() string { return e.err.Error() }

// Unwrap exposes the underlying failure for errors.Is and errors.As.
func (e *requestInternalError) Unwrap() error { return e.err }

// requestErrorResponse is the handler boundary for parse failures. It preserves
// the established 413 and client-safe 400 messages while ensuring internal I/O
// details never reach the client.
func requestErrorResponse(err error) (status int, body string) {
	if errors.Is(err, requestBodyTooLargeError) {
		return http.StatusRequestEntityTooLarge, http.StatusText(http.StatusRequestEntityTooLarge)
	}
	if clientError, ok := errors.AsType[*requestClientError](err); ok {
		return http.StatusBadRequest, clientError.Error()
	}
	return http.StatusInternalServerError, http.StatusText(http.StatusInternalServerError)
}

// classifyBodyReadError separates client truncation and size violations from
// internal body I/O failures across every request parser.
func classifyBodyReadError(err error) error {
	if errors.Is(err, requestBodyTooLargeError) {
		return requestBodyTooLargeError
	}
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		return requestBodyTooLargeError
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return &requestClientError{err: err}
	}
	return &requestInternalError{err: err}
}

// classifyMultipartParseError keeps temporary-file and disk failures server-side.
func classifyMultipartParseError(err error) error {
	if _, ok := errors.AsType[*os.PathError](err); ok {
		return &requestInternalError{err: err}
	}
	return &requestClientError{err: err}
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

// requestReadTracker remembers a body-read error even when a parser such as
// multipart.ReadForm wraps it with format-specific context.
type requestReadTracker struct {
	// ReadCloser is the request body being observed.
	io.ReadCloser
	// err retains the first non-EOF read failure.
	err error
}

// requestReadTrackerPool reuses the multipart parser's internal body wrapper.
var requestReadTrackerPool = sync.Pool{
	New: func() any { return &requestReadTracker{} },
}

// acquireRequestReadTracker binds a fresh body reader to a pooled tracker.
func acquireRequestReadTracker(body io.ReadCloser) *requestReadTracker {
	tracker := requestReadTrackerPool.Get().(*requestReadTracker)
	tracker.ReadCloser = body
	tracker.err = nil
	return tracker
}

// releaseRequestReadTracker clears body references before pooling.
func releaseRequestReadTracker(tracker *requestReadTracker) {
	if tracker == nil {
		return
	}
	tracker.ReadCloser = nil
	tracker.err = nil
	requestReadTrackerPool.Put(tracker)
}

// Read records body failures that a multipart parser may wrap.
func (r *requestReadTracker) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if err != nil && err != io.EOF {
		r.err = err
	}
	return n, err
}

// preparedRequestBody marks a body already wrapped with the framework limit.
// The wrapper makes boundary preparation idempotent while still allowing a
// middleware-replaced body to be limited again during parameter parsing.
type preparedRequestBody struct {
	io.ReadCloser
}

// prepareRequestBody applies the total body cap before media-type parsing. A
// known oversized length is rejected without reading; net/http already bounds a
// server body to its declared length. Unknown lengths are wrapped without being
// consumed, so raw handlers can stream them and observe MaxBytesError only when
// a read crosses the limit.
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
	if r.Body == nil {
		return nil
	}
	if _, ok := r.Body.(*preparedRequestBody); ok {
		return nil
	}

	unknownLength := r.ContentLength < 0 ||
		(r.ContentLength == 0 && r.Body != http.NoBody)
	if !unknownLength {
		return nil
	}

	r.Body = &preparedRequestBody{ReadCloser: http.MaxBytesReader(w, r.Body, limit)}
	return nil
}

// mergeSelectedFormInPlace adds query-only fields to the selected body map.
// Request.Form and Request.PostForm intentionally share this request-local view.
func mergeSelectedFormInPlace(query, body url.Values) url.Values {
	for key, values := range query {
		if _, exists := body[key]; !exists {
			body[key] = values
		}
	}
	return body
}

// mergeFormBodyWins merges multipart query and body values without mutating the
// standard library's PostForm map. The returned map and slices are independent.
func mergeFormBodyWins(query, body url.Values) url.Values {
	merged := make(url.Values, len(query)+len(body))
	for key, values := range query {
		merged[key] = slices.Clone(values)
	}
	for key, values := range body {
		merged[key] = slices.Clone(values)
	}
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
	return queryValuesForSet(r, newRequestKeySet(keys))
}

// queryValuesForSet is the request-path variant. Route handlers pass the lookup
// built by bindPlan.prepareKeys, avoiding both a key slice and a linear scan of
// that slice for every query pair.
func queryValuesForSet(r *http.Request, keys requestKeySet) url.Values {
	if len(keys) == 0 || r.URL.RawQuery == empty {
		return nil
	}
	var values url.Values
	for part := range strings.SplitSeq(r.URL.RawQuery, "&") {
		if part == empty {
			continue
		}
		rawKey, rawValue, _ := strings.Cut(part, "=")
		if strings.Contains(part, ";") {
			continue
		}
		key, err := url.QueryUnescape(rawKey)
		if err != nil {
			continue
		}
		if _, ok := keys[key]; !ok {
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

// maxPooledFormKeys bounds retained map capacity for request-scoped form values.
const maxPooledFormKeys = 64

// maxPooledFormValues bounds each pooled field slice.
const maxPooledFormValues = 256

// maxPooledFormBytes bounds body buffers eligible for pooling and marks larger forms unpooled.
const maxPooledFormBytes = 64 << 10

// pooledFormValues owns one pooled field slice.
type pooledFormValues struct {
	values []string
}

// formValueSlicePool reuses field slices without pooling the exposed url.Values map.
var formValueSlicePool = sync.Pool{
	New: func() any { return &pooledFormValues{} },
}

// pooledForm owns request-scoped Form/PostForm maps and pooled field slices.
// It remains valid until Context.recycle releases it after middleware and handlers return.
type pooledForm struct {
	values       url.Values
	objects      map[string]*pooledFormValues
	sets         []*pooledFormValues
	body         []byte
	bodyRetained bool // selected strings alias body, so release drops instead of clearing it.
	large        bool
}

// formPool reuses ordinary form maps and field-slice bookkeeping.
var formPool = sync.Pool{
	New: func() any {
		return &pooledForm{
			values:  make(url.Values),
			objects: make(map[string]*pooledFormValues),
		}
	},
}

// acquirePooledForm clears a pooled form and records whether its capacity is bounded.
func acquirePooledForm(keyCount int) *pooledForm {
	form := formPool.Get().(*pooledForm)
	clear(form.values)
	clear(form.objects)
	clear(form.sets)
	form.sets = form.sets[:0]
	form.body = form.body[:0]
	form.bodyRetained = false
	form.large = keyCount > maxPooledFormKeys
	return form
}

// urlValues returns the request-scoped binding view.
func (form *pooledForm) urlValues() url.Values {
	if form == nil {
		return nil
	}
	return form.values
}

// appendValue stores one value in a pooled field slice.
func (form *pooledForm) appendValue(key, value string) {
	set := form.objects[key]
	if set == nil {
		set = formValueSlicePool.Get().(*pooledFormValues)
		set.values = set.values[:0]
		form.objects[key] = set
		form.sets = append(form.sets, set)
	}
	set.values = append(set.values, value)
	form.values[key] = set.values
}

// parseQuery selects request query values into the same pool used for form bodies.
func (form *pooledForm) parseQuery(r *http.Request, keys requestKeySet) {
	if len(keys) == 0 || r.URL.RawQuery == empty {
		return
	}
	for part := range strings.SplitSeq(r.URL.RawQuery, "&") {
		if part == empty {
			continue
		}
		rawKey, rawValue, _ := strings.Cut(part, "=")
		if strings.Contains(part, ";") {
			continue
		}
		key, err := url.QueryUnescape(rawKey)
		if err != nil {
			continue
		}
		if _, ok := keys[key]; !ok {
			continue
		}
		value, err := url.QueryUnescape(rawValue)
		if err != nil {
			continue
		}
		form.appendValue(key, value)
	}
}

// mergeQuery adds query-only fields while preserving body precedence.
func (form *pooledForm) mergeQuery(r *http.Request, keys requestKeySet) {
	if len(keys) == 0 || r.URL.RawQuery == empty {
		return
	}
	for part := range strings.SplitSeq(r.URL.RawQuery, "&") {
		if part == empty {
			continue
		}
		rawKey, rawValue, _ := strings.Cut(part, "=")
		if strings.Contains(part, ";") {
			continue
		}
		key, err := url.QueryUnescape(rawKey)
		if err != nil {
			continue
		}
		if _, ok := keys[key]; !ok {
			continue
		}
		if _, exists := form.objects[key]; exists {
			continue
		}
		value, err := url.QueryUnescape(rawValue)
		if err != nil {
			continue
		}
		form.appendValue(key, value)
	}
}

// releasePooledForm clears request references and pools only bounded storage.
func releasePooledForm(form *pooledForm) {
	if form == nil {
		return
	}
	for _, set := range form.sets {
		clear(set.values)
		if !form.large && cap(set.values) <= maxPooledFormValues {
			set.values = set.values[:0]
			formValueSlicePool.Put(set)
		}
	}
	clear(form.values)
	clear(form.objects)
	clear(form.sets)
	if form.bodyRetained {
		form.body = nil
		form.bodyRetained = false
	} else {
		clear(form.body)
	}
	if form.large || cap(form.sets) > maxPooledFormKeys || cap(form.body) > maxPooledFormBytes {
		return
	}
	form.sets = form.sets[:0]
	form.body = form.body[:0]
	formPool.Put(form)
}

// parse decodes selected urlencoded fields in one pass using pooled maps and field slices.
func (form *pooledForm) parse(data []byte, keys requestKeySet) error {
	if len(keys) == 0 || len(data) == 0 {
		return nil
	}
	if len(data) > maxPooledFormBytes {
		form.large = true
	}
	if cap(form.body) < len(data) {
		form.body = make([]byte, len(data))
	} else {
		form.body = form.body[:len(data)]
	}
	copy(form.body, data)
	text := unsafe.String(unsafe.SliceData(form.body), len(form.body))
	for part := range strings.SplitSeq(text, "&") {
		if part == empty {
			continue
		}
		rawKey, rawValue, _ := strings.Cut(part, "=")
		if strings.Contains(part, ";") {
			return errors.New("invalid semicolon separator in query")
		}
		key, err := url.QueryUnescape(rawKey)
		if err != nil {
			return err
		}
		if _, ok := keys[key]; !ok {
			continue
		}
		if !strings.ContainsAny(rawKey, "%+") {
			form.bodyRetained = true
		}
		value, err := url.QueryUnescape(rawValue)
		if err != nil {
			return err
		}
		if !strings.ContainsAny(rawValue, "%+") {
			form.bodyRetained = true
		}
		form.appendValue(key, value)
	}
	return nil
}

// formValuesForSet is the direct-call variant; callers must release the returned form.
func formValuesForSet(data []byte, keys requestKeySet) (*pooledForm, error) {
	form := acquirePooledForm(len(keys))
	if err := form.parse(data, keys); err != nil {
		releasePooledForm(form)
		return nil, err
	}
	return form, nil
}

// requestKeySource accepts the startup-prepared set used by route handlers and
// a plain slice used by direct parser callers.
func requestKeySource(source any) (int, requestKeySet, bool) {
	switch keys := source.(type) {
	case requestKeySet:
		return len(keys), keys, true
	case []string:
		if keys == nil {
			return 0, nil, false
		}
		return len(keys), newRequestKeySet(keys), true
	default:
		return 0, nil, false
	}
}

// parseRequestParameters collects handler-bindable inputs for the request:
// form values, uploaded files, the JSON body buffer and whether a JSON body
// with actual content is present. queryKeys may be the startup-prepared set from
// a bind plan or a plain slice for direct callers. Route handlers parse only
// planned form keys, and Request.Form/PostForm expose that selected namespace.
// The body buffer is pooled; the caller must release it once binding is done.
// The returned flag already folds
// in the non-empty check, so a whitespace-only JSON body fails here and a body
// that passes is never scanned again downstream.
func parseRequestParameters(w http.ResponseWriter, r *http.Request,
	method httpMethod, queryKeys any) (*pooledForm, filesMap, *bytes.Buffer, bool, error) {
	keyCount, keySet, selective := requestKeySource(queryKeys)

	// GBL deliberately applies a strict parameter-source policy by HTTP method.
	// GET, HEAD, DELETE and OPTIONS may read only URL path parameters (matched by
	// the router) and URL query parameters. Their request bodies are never prepared,
	// parsed or read, regardless of Content-Type.
	//
	// Only POST, PUT and PATCH may carry a body GBL reads; every other method
	// exposes query parameters and leaves the body untouched.
	if !method.isBodyMethod() {
		if keyCount == 0 {
			return nil, nil, nil, false, nil
		}
		form := acquirePooledForm(keyCount)
		form.parseQuery(r, keySet)
		if len(form.values) == 0 {
			releasePooledForm(form)
			return nil, nil, nil, false, nil
		}
		return form, nil, nil, false, nil
	}

	if err := prepareRequestBody(w, r); err != nil {
		return nil, nil, nil, false, err
	}

	var (
		form      *pooledForm
		files     filesMap
		bodyBuf   *bytes.Buffer
		mediaType string
	)

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
				return nil, nil, nil, false, &requestClientError{
					err: fmt.Errorf("invalid Content-Type: %w", err),
				}
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
			return nil, nil, nil, false, classifyBodyReadError(err)
		}
		if bodyBuf != nil && !hasNonWhitespace(bodyBuf.Bytes()) {
			releaseBodyBuffer(bodyBuf)
			return nil, nil, nil, false, &requestClientError{err: errors.New("JSON body is empty")}
		}

	case mediaType == multipartFormData:
		maxUpload := maxUploadFileSize.Load()
		if maxUpload <= 0 {
			maxUpload = defaultMaxUploadFileSize
		}
		memory := maxFileMemory.Load()
		if memory <= 0 {
			memory = defaultMaxFileMemory
		}
		// A file that will exceed the per-file cap must not first spill a much
		// larger amount to disk merely because the aggregate threshold is higher.
		memory = min(memory, maxUpload)

		originalBody := r.Body
		body := originalBody
		if body == nil {
			body = http.NoBody
		}
		tracker := acquireRequestReadTracker(body)
		r.Body = tracker
		defer func() {
			r.Body = originalBody
			releaseRequestReadTracker(tracker)
		}()
		if err := r.ParseMultipartForm(memory); err != nil {
			cleanupParsedMultipart(r)
			if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
				return nil, nil, nil, false, requestBodyTooLargeError
			}
			if tracker.err != nil {
				return nil, nil, nil, false, classifyBodyReadError(tracker.err)
			}
			return nil, nil, nil, false, classifyMultipartParseError(err)
		}
		if tracker.err != nil {
			cleanupParsedMultipart(r)
			return nil, nil, nil, false, classifyBodyReadError(tracker.err)
		}
		if r.PostForm == nil {
			r.PostForm = make(url.Values)
		}
		// ReadForm combines query and body values with query first. Binding uses
		// body-wins precedence, so publish that same merged view on Request.Form.
		merged := mergeFormBodyWins(queryValues(r), r.PostForm)
		r.Form = merged
		form = &pooledForm{values: merged, large: true}
		if r.MultipartForm != nil {
			files = r.MultipartForm.File
		}
		// Per-file hard cap: any uploaded part larger than the configured
		// threshold aborts the request before the handler runs. All parsed
		// temporary files are removed if any file exceeds the cap.
		for _, headers := range files {
			for _, hdr := range headers {
				if hdr != nil && hdr.Size > maxUpload {
					cleanupParsedMultipart(r)
					return nil, nil, nil, false, requestBodyTooLargeError
				}
			}
		}

	case mediaType == xWwwFormUrlencoded || mediaType == empty:
		// The body is decoded directly rather than through r.ParseForm because
		// ParseForm would parse the URL query a second time: queryValues below
		// owns that single parse. prepareRequestBody rejects known oversized
		// lengths early and bounds unknown-length streams with MaxBytesReader.
		if r.Body != nil {
			buf := bodyBufferPool.Get().(*bytes.Buffer)
			buf.Reset()
			if r.ContentLength > int64(buf.Cap()) {
				buf.Grow(int(r.ContentLength))
			}
			if _, readErr := buf.ReadFrom(r.Body); readErr != nil {
				releaseBodyBuffer(buf)
				return nil, nil, nil, false, classifyBodyReadError(readErr)
			}
			var parseErr error
			if selective {
				form, parseErr = formValuesForSet(buf.Bytes(), keySet)
			} else {
				values, queryErr := url.ParseQuery(string(buf.Bytes()))
				parseErr = queryErr
				if values != nil {
					form = &pooledForm{values: values, large: true}
				}
			}
			releaseBodyBuffer(buf)
			if parseErr != nil {
				return nil, nil, nil, false, &requestClientError{err: parseErr}
			}
		}
		if form == nil {
			if selective {
				form = acquirePooledForm(len(keySet))
			} else {
				form = &pooledForm{values: make(url.Values), large: true}
			}
		}
		// Body fields win over query on shared keys; planned keys survive selection.
		if selective {
			form.mergeQuery(r, keySet)
		} else {
			mergeSelectedFormInPlace(queryValues(r), form.values)
		}
		r.PostForm = form.values
		r.Form = form.values

	default:
		// Unsupported body media types are not interpreted as parameters.
		// Query parameters are still valid for POST, PUT and PATCH. This parsing
		// path buffers the already bounded body so an oversized stream is rejected
		// before the handler and replays it otherwise.
		if err := replayUnsupportedRequestBody(r); err != nil {
			return nil, nil, nil, false, err
		}
		if keyCount > 0 {
			form = acquirePooledForm(keyCount)
			form.parseQuery(r, keySet)
			if len(form.values) == 0 {
				releasePooledForm(form)
				form = nil
			}
		}
	}

	return form, files, bodyBuf, jsonMedia && bodyBuf != nil, nil
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

// cleanupParsedMultipart removes all files left by a complete or partial
// multipart parse. ParseMultipartForm performs its own cleanup on read errors;
// calling this again also covers errors discovered after a successful parse.
func cleanupParsedMultipart(r *http.Request) {
	if r.MultipartForm != nil {
		_ = r.MultipartForm.RemoveAll()
	}
}
