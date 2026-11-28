// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"bytes"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
)

// This file holds request parsing orchestration across body, form,
// multipart, and JSON inputs.

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

	originalBody := r.Body
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

		body := r.Body
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

// cleanupParsedMultipart removes all files left by a complete or partial
// multipart parse. ParseMultipartForm performs its own cleanup on read errors;
// calling this again also covers errors discovered after a successful parse.
func cleanupParsedMultipart(r *http.Request) {
	if r.MultipartForm != nil {
		_ = r.MultipartForm.RemoveAll()
	}
}
