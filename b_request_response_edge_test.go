// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const rrEdgeTestTimeout = 2 * time.Second

// rrEdgeRun gives every operation using an injected reader or writer a cancellable
// context. The worker never calls testing.T: failures are returned to the test
// goroutine. Callbacks must keep injected I/O nonblocking or observe ctx; after a
// timeout rrEdgeRun cancels the context and bounds the wait for worker exit.
func rrEdgeRun(t *testing.T, fn func(context.Context) error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), rrEdgeTestTimeout)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		var err error
		defer func() {
			if recovered := recover(); recovered != nil {
				err = fmt.Errorf("panic: %v", recovered)
			}
			done <- err
		}()
		if ctxErr := ctx.Err(); ctxErr != nil {
			err = ctxErr
			return
		}
		err = fn(ctx)
		if ctxErr := ctx.Err(); ctxErr != nil {
			err = ctxErr
		}
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("request/response edge operation failed while canceling: %v", err)
			}
		case <-time.After(rrEdgeTestTimeout):
			t.Fatal("request/response edge worker did not exit after cancellation")
		}
		t.Fatalf("request/response edge operation timed out after %s", rrEdgeTestTimeout)
	}
}

type rrEdgeReadStep struct {
	data []byte
	err  error
}

// rrEdgeReadCloser is a deterministic, concurrency-safe fault-injecting body.
// A step may return data and an error together, which exercises parsers that
// buffer a complete final read before observing its error.
type rrEdgeReadCloser struct {
	mu         sync.Mutex
	steps      []rrEdgeReadStep
	index      int
	offset     int
	calls      int
	closeCalls int
	closeErr   error
}

func (r *rrEdgeReadCloser) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.index >= len(r.steps) {
		return 0, io.EOF
	}
	step := r.steps[r.index]
	n := copy(p, step.data[r.offset:])
	r.offset += n
	if r.offset == len(step.data) {
		r.index++
		r.offset = 0
	}
	return n, step.err
}

func (r *rrEdgeReadCloser) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closeCalls++
	return r.closeErr
}

func (r *rrEdgeReadCloser) snapshot() (calls, closeCalls int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls, r.closeCalls
}

type rrEdgeWriter struct {
	mu         sync.Mutex
	header     http.Header
	statuses   []int
	body       bytes.Buffer
	writeCalls int
	writeN     int
	writeErr   error
}

func rrEdgeNewWriter() *rrEdgeWriter {
	return &rrEdgeWriter{header: make(http.Header), writeN: -1}
}

func (w *rrEdgeWriter) Header() http.Header {
	return w.header
}

func (w *rrEdgeWriter) WriteHeader(code int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.statuses = append(w.statuses, code)
}

func (w *rrEdgeWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writeCalls++
	n := len(p)
	if w.writeErr != nil && w.writeN >= 0 && w.writeN < n {
		n = w.writeN
	}
	if n > 0 {
		_, _ = w.body.Write(p[:n])
	}
	return n, w.writeErr
}

func (w *rrEdgeWriter) snapshot() (http.Header, []int, string, int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.header.Clone(), append([]int(nil), w.statuses...), w.body.String(), w.writeCalls
}

type rrEdgeFlushWriter struct {
	*rrEdgeWriter
	mu            sync.Mutex
	flushCalls    int
	deadlineCalls int
	flushErr      error
	deadlineErr   error
}

func (w *rrEdgeFlushWriter) FlushError() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.flushCalls++
	return w.flushErr
}

func (w *rrEdgeFlushWriter) SetWriteDeadline(time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.deadlineCalls++
	return w.deadlineErr
}

func (w *rrEdgeFlushWriter) flushSnapshot() (flushes, deadlines int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.flushCalls, w.deadlineCalls
}

type rrEdgeHijackWriter struct {
	*rrEdgeWriter
	mu       sync.Mutex
	calls    int
	conn     net.Conn
	buffered *bufio.ReadWriter
	err      error
}

func (w *rrEdgeHijackWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls++
	return w.conn, w.buffered, w.err
}

type rrEdgeUnwrapWriter struct {
	http.ResponseWriter
}

func (w *rrEdgeUnwrapWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

type rrEdgeLogEntry struct {
	message string
	kv      []any
}

type rrEdgeLogger struct {
	mu      sync.Mutex
	entries []rrEdgeLogEntry
}

func (*rrEdgeLogger) Info(string, ...any) {}
func (*rrEdgeLogger) Warn(string, ...any) {}
func (l *rrEdgeLogger) Error(message string, kv ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, rrEdgeLogEntry{message: message, kv: append([]any(nil), kv...)})
}

func (l *rrEdgeLogger) snapshot() []rrEdgeLogEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	entries := make([]rrEdgeLogEntry, len(l.entries))
	copy(entries, l.entries)
	return entries
}

func rrEdgeLimits(t *testing.T, body, memory, upload int64) {
	t.Helper()
	oldBody, oldMemory, oldUpload := maxBodySize.Load(), maxFileMemory.Load(), maxUploadFileSize.Load()
	maxBodySize.Store(body)
	maxFileMemory.Store(memory)
	maxUploadFileSize.Store(upload)
	t.Cleanup(func() {
		maxBodySize.Store(oldBody)
		maxFileMemory.Store(oldMemory)
		maxUploadFileSize.Store(oldUpload)
	})
}

func rrEdgeParse(r *http.Request, keys any) (url.Values, filesMap, *bytes.Buffer, bool, error) {
	form, files, body, jsonMedia, err := parseRequestParameters(rrEdgeNewWriter(), r, methodPost, keys)
	if body != nil {
		releaseBodyBuffer(body)
	}
	cleanupParsedMultipart(r)
	return form.urlValues(), files, body, jsonMedia, err
}

func rrEdgeMultipart(t *testing.T, fields url.Values, upload []byte) ([]byte, string) {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for key, values := range fields {
		for _, value := range values {
			if err := writer.WriteField(key, value); err != nil {
				t.Fatalf("WriteField: %v", err)
			}
		}
	}
	if upload != nil {
		part, err := writer.CreateFormFile("upload", "edge.txt")
		if err != nil {
			t.Fatalf("CreateFormFile: %v", err)
		}
		if _, err := part.Write(upload); err != nil {
			t.Fatalf("write upload: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	return body.Bytes(), writer.FormDataContentType()
}

func rrEdgeAbort(t *testing.T, fn func()) responseAbort {
	t.Helper()
	var halt responseAbort
	rrEdgeRun(t, func(ctx context.Context) error {
		recovered := rrEdgeRecover(fn)
		if recovered == nil {
			return errors.New("writeRequestError returned without aborting")
		}
		var ok bool
		halt, ok = recovered.(responseAbort)
		if !ok {
			return fmt.Errorf("writeRequestError panicked with %T, want responseAbort", recovered)
		}
		return nil
	})
	return halt
}

func rrEdgeRecover(fn func()) (recovered any) {
	defer func() { recovered = recover() }()
	fn()
	return nil
}

func TestRequestResponseEdgeErrorsAndReplayBody(t *testing.T) {
	cause := errors.New("edge cause")
	client := &requestClientError{err: cause}
	internal := &requestInternalError{err: cause}
	if client.Error() != cause.Error() || client.Unwrap() != cause || !errors.Is(client, cause) {
		t.Fatalf("client error did not preserve its cause: %#v", client)
	}
	if internal.Error() != cause.Error() || internal.Unwrap() != cause || !errors.Is(internal, cause) {
		t.Fatalf("internal error did not preserve its cause: %#v", internal)
	}

	for _, tc := range []struct {
		name   string
		err    error
		status int
		body   string
	}{
		{name: "nil", status: http.StatusInternalServerError, body: "Internal Server Error"},
		{name: "generic", err: errors.New("secret"), status: http.StatusInternalServerError, body: "Internal Server Error"},
		{name: "internal", err: internal, status: http.StatusInternalServerError, body: "Internal Server Error"},
		{name: "client", err: client, status: http.StatusBadRequest, body: cause.Error()},
		{name: "wrapped client", err: fmt.Errorf("outer: %w", client), status: http.StatusBadRequest, body: cause.Error()},
		{name: "too large", err: requestBodyTooLargeError, status: http.StatusRequestEntityTooLarge, body: "Request Entity Too Large"},
		{name: "wrapped too large", err: fmt.Errorf("outer: %w", requestBodyTooLargeError), status: http.StatusRequestEntityTooLarge, body: "Request Entity Too Large"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body := requestErrorResponse(tc.err)
			if status != tc.status || body != tc.body {
				t.Fatalf("requestErrorResponse = (%d, %q), want (%d, %q)", status, body, tc.status, tc.body)
			}
		})
	}

	if err := (&replayRequestBody{Reader: bytes.NewReader(nil)}).Close(); err != nil {
		t.Fatalf("Close with nil closer: %v", err)
	}
	for _, tc := range []struct {
		name      string
		readErr   error
		wantTrack error
	}{
		{name: "EOF is not retained", readErr: io.EOF},
		{name: "non-EOF is retained", readErr: cause, wantTrack: cause},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &rrEdgeReadCloser{steps: []rrEdgeReadStep{{data: []byte("x"), err: tc.readErr}}}
			tracker := &requestReadTracker{ReadCloser: body}
			var n int
			var err error
			rrEdgeRun(t, func(ctx context.Context) error { n, err = tracker.Read(make([]byte, 1)); return nil })
			if n != 1 || !errors.Is(err, tc.readErr) || !errors.Is(tracker.err, tc.wantTrack) {
				t.Fatalf("tracked Read = (%d, %v, %v), want (1, %v, %v)", n, err, tracker.err, tc.readErr, tc.wantTrack)
			}
		})
	}
	for _, closeErr := range []error{nil, errors.New("close failed")} {
		closer := &rrEdgeReadCloser{closeErr: closeErr}
		body := &replayRequestBody{Reader: bytes.NewReader([]byte("replay")), closer: closer}
		var got error
		rrEdgeRun(t, func(ctx context.Context) error { got = body.Close(); return nil })
		_, calls := closer.snapshot()
		if calls != 1 || !errors.Is(got, closeErr) {
			t.Fatalf("Close = %v, calls=%d, want %v and one call", got, calls, closeErr)
		}
	}
}

func TestRequestResponseEdgePrepareRequestBody(t *testing.T) {
	rrEdgeLimits(t, 4, 8, 128)
	for _, tc := range []struct {
		name          string
		method        string
		contentLength int64
		steps         []rrEdgeReadStep
		nilBody       bool
		noBody        bool
		limit         int64
		wantErr       error
		wantData      string
		wantCalls     int
	}{
		{name: "non body method", method: http.MethodGet, contentLength: 99, steps: []rrEdgeReadStep{{data: []byte("data")}}, limit: 4},
		{name: "nil body", method: http.MethodPost, nilBody: true, limit: 4},
		{name: "known no body", method: http.MethodPost, noBody: true, limit: 4},
		{name: "known oversize", method: http.MethodPost, contentLength: 5, steps: []rrEdgeReadStep{{data: []byte("data")}}, limit: 4, wantErr: requestBodyTooLargeError},
		{name: "known exact", method: http.MethodPut, contentLength: 4, steps: []rrEdgeReadStep{{data: []byte("data")}}, limit: 4},
		{name: "unknown length", method: http.MethodPatch, contentLength: -1, steps: []rrEdgeReadStep{{data: []byte("data")}}, limit: 4, wantData: "data"},
		{name: "zero with body", method: http.MethodPost, steps: []rrEdgeReadStep{{data: []byte("data")}}, limit: 4, wantData: "data"},
		{name: "non positive limit falls back", method: http.MethodPost, contentLength: -1, steps: []rrEdgeReadStep{{data: []byte("ok")}}, limit: 0, wantData: "ok"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			maxBodySize.Store(tc.limit)
			body := &rrEdgeReadCloser{steps: tc.steps}
			r := httptest.NewRequest(tc.method, "/", nil)
			r.ContentLength = tc.contentLength
			switch {
			case tc.nilBody:
				r.Body = nil
			case tc.noBody:
				r.Body = http.NoBody
			default:
				r.Body = body
			}
			original := r.Body
			var err error
			rrEdgeRun(t, func(ctx context.Context) error {
				err = prepareRequestBody(rrEdgeNewWriter(), r)
				return nil
			})
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("prepareRequestBody error = %v, want %v", err, tc.wantErr)
				}
			} else if err != nil {
				t.Fatalf("prepareRequestBody: %v", err)
			}
			calls, _ := body.snapshot()
			if calls != tc.wantCalls {
				t.Fatalf("body reads = %d, want %d", calls, tc.wantCalls)
			}
			if tc.wantData != "" {
				if _, ok := r.Body.(*replayRequestBody); ok {
					t.Fatal("unknown-length raw body was replaced with a replay buffer")
				}
				if r.ContentLength != tc.contentLength {
					t.Fatalf("prepared content length = %d, want unchanged %d", r.ContentLength, tc.contentLength)
				}
				var data []byte
				rrEdgeRun(t, func(ctx context.Context) error {
					var readErr error
					data, readErr = io.ReadAll(r.Body)
					return readErr
				})
				if string(data) != tc.wantData {
					t.Fatalf("streamed data = %q, want %q", data, tc.wantData)
				}
				rrEdgeRun(t, func(ctx context.Context) error { return r.Body.Close() })
				_, closeCalls := body.snapshot()
				if closeCalls != 1 {
					t.Fatalf("original body close calls = %d, want 1", closeCalls)
				}
			} else if tc.wantErr == nil && r.Body != original {
				t.Fatal("body that did not need streaming protection was replaced")
			}
		})
	}
}

func TestRequestResponseEdgeQueryAndKeySources(t *testing.T) {
	keys := newRequestKeySet([]string{"name", "tag"})
	for _, tc := range []struct {
		name  string
		query string
		keys  requestKeySet
		want  url.Values
	}{
		{name: "empty query", query: "", keys: keys},
		{name: "no keys", query: "name=ignored", keys: nil},
		{name: "single pair", query: "name=one", keys: keys, want: url.Values{"name": {"one"}}},
		{name: "repeated and unrelated", query: "name=one&tag=a&tag=b&other=x", keys: keys, want: url.Values{"name": {"one"}, "tag": {"a", "b"}}},
		{name: "empty parts", query: "&&name=one&&", keys: keys, want: url.Values{"name": {"one"}}},
		{name: "semicolon rejected", query: "name=one;bad&tag=ok", keys: keys, want: url.Values{"tag": {"ok"}}},
		{name: "malformed key", query: "%zz=bad&name=ok", keys: keys, want: url.Values{"name": {"ok"}}},
		{name: "malformed value", query: "name=%zz&tag=ok", keys: keys, want: url.Values{"tag": {"ok"}}},
		{name: "unselected key", query: "other=x&name=ok", keys: keys, want: url.Values{"name": {"ok"}}},
		{name: "decoding", query: "name=a+b&name=c%20d", keys: keys, want: url.Values{"name": {"a b", "c d"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &http.Request{URL: &url.URL{RawQuery: tc.query}}
			if got := queryValuesForSet(r, tc.keys); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("queryValuesForSet = %#v, want %#v", got, tc.want)
			}
		})
	}

	count, set, selected := requestKeySource(keys)
	if count != 2 || len(set) != 2 || !selected {
		t.Fatalf("set source = (%d, %#v, %t), want two selected keys", count, set, selected)
	}
	count, set, selected = requestKeySource([]string{"name", "name", "tag"})
	if count != 3 || len(set) != 2 || !selected {
		t.Fatalf("slice source = (%d, %#v, %t), want count 3 and two unique keys", count, set, selected)
	}
	count, set, selected = requestKeySource("unsupported")
	if count != 0 || set != nil || selected {
		t.Fatalf("unsupported source = (%d, %#v, %t), want (0, nil, false)", count, set, selected)
	}

	r := httptest.NewRequest(http.MethodGet, "/?name=query&other=ignored", strings.NewReader("untouched"))
	form, files, body, jsonMedia, err := parseRequestParameters(rrEdgeNewWriter(), r, methodGet, keys)
	if err != nil || !reflect.DeepEqual(form.urlValues(), url.Values{"name": {"query"}}) ||
		files != nil || body != nil || jsonMedia {
		t.Fatalf("non-body parse = (%#v, %#v, %v, %v, %v)", form, files, body, jsonMedia, err)
	}

	r = httptest.NewRequest(http.MethodPost, "/?name=query&other=ignored", nil)
	r.Header.Set(contentType, "text/plain")
	values, _, _, _, err := rrEdgeParse(r, keys)
	if err != nil || !reflect.DeepEqual(values, url.Values{"name": {"query"}}) {
		t.Fatalf("unsupported-media query parse = (%#v, %v)", values, err)
	}

	r = httptest.NewRequest(http.MethodPost, "/?name=query", nil)
	r.Header.Set(contentType, "text/plain")
	values, _, _, _, err = rrEdgeParse(r, keys)
	if err != nil || !reflect.DeepEqual(values, url.Values{"name": {"query"}}) {
		t.Fatalf("unsupported-media single query parse = (%#v, %v)", values, err)
	}
}

func TestRequestResponseEdgeParseInputErrors(t *testing.T) {
	rrEdgeLimits(t, 128, 8, 128)
	readFailure := errors.New("parser read failed")
	maxBytes := &http.MaxBytesError{Limit: 128}
	for _, mediaType := range []string{applicationJson, xWwwFormUrlencoded} {
		for _, tc := range []struct {
			name         string
			readErr      error
			wantTooLarge bool
			wantClient   bool
		}{
			{name: "I/O error", readErr: readFailure},
			{name: "unexpected EOF", readErr: io.ErrUnexpectedEOF, wantClient: true},
			{name: "MaxBytes error", readErr: maxBytes, wantTooLarge: true},
		} {
			t.Run(mediaType+"/"+tc.name, func(t *testing.T) {
				body := &rrEdgeReadCloser{steps: []rrEdgeReadStep{{err: tc.readErr}}}
				r := httptest.NewRequest(http.MethodPost, "/", nil)
				r.Body = body
				r.ContentLength = 1
				r.Header.Set(contentType, mediaType)
				_, _, _, _, err := rrEdgeRunParse(t, r)
				if tc.wantTooLarge {
					if !errors.Is(err, requestBodyTooLargeError) {
						t.Fatalf("parse error = %v, want requestBodyTooLargeError", err)
					}
				} else if tc.wantClient {
					var client *requestClientError
					if !errors.As(err, &client) {
						t.Fatalf("parse error = %v, want client read failure", err)
					}
				} else {
					var internal *requestInternalError
					if !errors.As(err, &internal) || !errors.Is(err, readFailure) {
						t.Fatalf("parse error = %v, want internal read failure", err)
					}
				}
			})
		}
	}

	t.Run("nil JSON body", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		r.Body = nil
		r.Header.Set(contentType, applicationJson)
		_, _, body, jsonMedia, err := rrEdgeRunParse(t, r)
		if err != nil || body != nil || jsonMedia {
			t.Fatalf("nil JSON parse = (%v, %v, %v)", body, jsonMedia, err)
		}
	})

	t.Run("nil urlencoded body initializes PostForm", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		r.Body = nil
		form, _, _, _, err := rrEdgeRunParse(t, r)
		if err != nil || form == nil || r.PostForm == nil || r.Form == nil || len(r.PostForm) != 0 {
			t.Fatalf("nil form parse = (%#v, %#v, %#v, %v)", form, r.PostForm, r.Form, err)
		}
	})

	t.Run("unsupported media unexpected EOF is a client error", func(t *testing.T) {
		body := &rrEdgeReadCloser{steps: []rrEdgeReadStep{{err: io.ErrUnexpectedEOF}}}
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		r.Body = body
		r.ContentLength = -1
		r.Header.Set(contentType, "text/plain")
		_, _, _, _, err := rrEdgeRunParse(t, r)
		var client *requestClientError
		if !errors.As(err, &client) {
			t.Fatalf("parse error = %v, want client read failure", err)
		}
	})

	t.Run("malformed urlencoded body", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("name=%zz"))
		r.Header.Set(contentType, xWwwFormUrlencoded)
		_, _, _, _, err := rrEdgeRunParse(t, r)
		var client *requestClientError
		if !errors.As(err, &client) {
			t.Fatalf("parse error = %v, want requestClientError", err)
		}
	})
}

func TestRequestResponseEdgeSelectedFormParserBranches(t *testing.T) {
	keys := requestKeySet{"name": {}}
	form, err := formValuesForSet([]byte("name=alice&noise=ignored"), keys)
	if err != nil || !reflect.DeepEqual(form.urlValues(), url.Values{"name": {"alice"}}) {
		releasePooledForm(form)
		t.Fatalf("small selected form = (%#v, %v)", form, err)
	}
	releasePooledForm(form)
	prefix := "name=alice&&noise=ignored&"
	form, err = formValuesForSet([]byte(prefix+strings.Repeat("other=x&", 8)), keys)
	if err != nil || !reflect.DeepEqual(form.urlValues(), url.Values{"name": {"alice"}}) {
		releasePooledForm(form)
		t.Fatalf("large selected form with empty pair = (%#v, %v)", form, err)
	}
	releasePooledForm(form)
	tests := []struct {
		name string
		body string
	}{
		{name: "semicolon", body: prefix + "bad;key=x&" + strings.Repeat("other=x&", 8)},
		{name: "invalid key escape", body: prefix + "%zz=x&" + strings.Repeat("other=x&", 8)},
		{name: "invalid value escape", body: prefix + "name=%zz&" + strings.Repeat("other=x&", 8)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := formValuesForSet([]byte(tc.body), keys); err == nil {
				t.Fatal("malformed selected form succeeded")
			}
		})
	}
	form, err = formValuesForSet(nil, keys)
	if err != nil || form == nil || len(form.values) != 0 {
		t.Fatalf("empty selected form = (%#v, %v), want empty form", form, err)
	}
	releasePooledForm(form)

	large := "name=alice&" + strings.Repeat("noise=x&", maxPooledFormBytes/4)
	form, err = formValuesForSet([]byte(large), keys)
	if err != nil || form.urlValues().Get("name") != "alice" || !form.large {
		t.Fatalf("large selected form = (%#v, %v)", form, err)
	}
	releasePooledForm(form)
}

func TestPooledQuerySelectionBranches(t *testing.T) {
	keys := requestKeySet{"name": {}}
	r := &http.Request{URL: &url.URL{RawQuery: "&&name=alice&bad;key=x&%zz=x&name=%zz&other=x"}}
	form := acquirePooledForm(len(keys))
	form.parseQuery(r, keys)
	if !reflect.DeepEqual(form.values, url.Values{"name": {"alice"}}) {
		releasePooledForm(form)
		t.Fatalf("selected query = %#v, want only valid name", form.values)
	}
	releasePooledForm(form)

	form = acquirePooledForm(len(keys))
	form.parseQuery(&http.Request{URL: &url.URL{}}, keys)
	form.parseQuery(r, nil)
	if len(form.values) != 0 {
		releasePooledForm(form)
		t.Fatalf("empty query selection = %#v, want empty", form.values)
	}
	releasePooledForm(form)

	body, err := formValuesForSet([]byte("name=body"), keys)
	if err != nil {
		t.Fatalf("parse precedence body: %v", err)
	}
	query := "&&name=query&queryOnly=yes&bad;key=x&%zz=x&queryOnly=%zz&other=x"
	body.mergeQuery(&http.Request{URL: &url.URL{RawQuery: query}},
		requestKeySet{"name": {}, "queryOnly": {}})
	want := url.Values{"name": {"body"}, "queryOnly": {"yes"}}
	if !reflect.DeepEqual(body.values, want) {
		releasePooledForm(body)
		t.Fatalf("merged query = %#v, want %#v", body.values, want)
	}
	releasePooledForm(body)

	form = acquirePooledForm(len(keys))
	form.mergeQuery(&http.Request{URL: &url.URL{RawQuery: "name=%zz"}}, keys)
	if len(form.values) != 0 {
		releasePooledForm(form)
		t.Fatalf("invalid query value was retained: %#v", form.values)
	}
	releasePooledForm(form)
}

func TestMergeSelectedFormInPlacePrecedence(t *testing.T) {
	body := url.Values{"name": {"body"}}
	query := url.Values{"name": {"query"}, "queryOnly": {"yes"}}
	got := mergeSelectedFormInPlace(query, body)
	want := url.Values{"name": {"body"}, "queryOnly": {"yes"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merged selected form = %#v, want %#v", got, want)
	}
}

func rrEdgeRunParse(t *testing.T, r *http.Request) (url.Values, filesMap, *bytes.Buffer, bool, error) {
	t.Helper()
	var form url.Values
	var files filesMap
	var body *bytes.Buffer
	var jsonMedia bool
	var err error
	rrEdgeRun(t, func(ctx context.Context) error {
		*r = *r.WithContext(ctx)
		form, files, body, jsonMedia, err = rrEdgeParse(r, nil)
		return ctx.Err()
	})
	return form, files, body, jsonMedia, err
}

func TestRequestResponseEdgeMultipartBranches(t *testing.T) {
	rrEdgeLimits(t, 4096, 8, 128)
	payload, mediaType := rrEdgeMultipart(t, url.Values{"name": {"value"}}, []byte("upload"))
	readFailure := errors.New("multipart read failed")
	maxBytes := &http.MaxBytesError{Limit: 4096}
	for _, tc := range []struct {
		name         string
		steps        []rrEdgeReadStep
		nilBody      bool
		wantTooLarge bool
		wantClient   bool
	}{
		{name: "nil body", nilBody: true, wantClient: true},
		{name: "read error", steps: []rrEdgeReadStep{{err: readFailure}}},
		{name: "unexpected EOF is client error", steps: []rrEdgeReadStep{{err: io.ErrUnexpectedEOF}}, wantClient: true},
		{name: "MaxBytes read error", steps: []rrEdgeReadStep{{err: maxBytes}}, wantTooLarge: true},
		{name: "error after complete body", steps: []rrEdgeReadStep{{data: payload, err: readFailure}}},
		{name: "unexpected EOF after complete body", steps: []rrEdgeReadStep{{data: payload, err: io.ErrUnexpectedEOF}}, wantClient: true},
		{name: "MaxBytes after complete body", steps: []rrEdgeReadStep{{data: payload, err: maxBytes}}, wantTooLarge: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &rrEdgeReadCloser{steps: tc.steps}
			r := httptest.NewRequest(http.MethodPost, "/", nil)
			r.Header.Set(contentType, mediaType)
			if tc.nilBody {
				r.Body = nil
			} else {
				r.Body = body
				r.ContentLength = int64(len(payload))
			}
			_, _, _, _, err := rrEdgeRunParse(t, r)
			switch {
			case tc.wantTooLarge:
				if !errors.Is(err, requestBodyTooLargeError) {
					t.Fatalf("multipart error = %v, want requestBodyTooLargeError", err)
				}
			case tc.wantClient:
				var client *requestClientError
				if !errors.As(err, &client) {
					t.Fatalf("multipart error = %T %v, want requestClientError", err, err)
				}
			default:
				var internal *requestInternalError
				if !errors.As(err, &internal) || !errors.Is(err, readFailure) {
					t.Fatalf("multipart error = %T %v, want internal read failure", err, err)
				}
			}
		})
	}

	t.Run("tracked MaxBytes masked by malformed header", func(t *testing.T) {
		const boundary = "rrEdgeBoundary"
		malformed := []byte("--" + boundary + "\r\nnot-a-header\r\n\r\n")
		body := &rrEdgeReadCloser{steps: []rrEdgeReadStep{{data: malformed, err: maxBytes}}}
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		r.Body = body
		r.ContentLength = int64(len(malformed))
		r.Header.Set(contentType, multipartFormData+"; boundary="+boundary)
		_, _, _, _, err := rrEdgeRunParse(t, r)
		if !errors.Is(err, requestBodyTooLargeError) {
			t.Fatalf("masked MaxBytes error = %T %v, want requestBodyTooLargeError", err, err)
		}
	})

	t.Run("non positive multipart limits fall back", func(t *testing.T) {
		maxFileMemory.Store(0)
		maxUploadFileSize.Store(0)
		r := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(payload))
		r.Header.Set(contentType, mediaType)
		form, files, _, _, err := rrEdgeRunParse(t, r)
		if err != nil || form.Get("name") != "value" || len(files["upload"]) != 1 {
			t.Fatalf("multipart fallback parse = (%#v, %#v, %v, query=%q, content-type=%q)", form, files, err, r.URL.RawQuery, r.Header.Get(contentType))
		}
	})

	t.Run("preparsed multipart initializes nil PostForm", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		r.Header.Set(contentType, mediaType)
		r.Form = make(url.Values)
		r.MultipartForm = &multipart.Form{Value: make(url.Values)}
		form, _, _, _, err := rrEdgeRunParse(t, r)
		if err != nil || r.PostForm == nil || form == nil {
			t.Fatalf("preparsed multipart = (%#v, %#v, %v)", form, r.PostForm, err)
		}
	})
}

func TestRequestResponseEdgePoolAndCleanup(t *testing.T) {
	bodyBuffer := bytes.NewBuffer(make([]byte, 0, maxPooledBodyCapacity+1))
	bodyBuffer.WriteString("oversized body buffer")
	releaseBodyBuffer(bodyBuffer)
	if bodyBuffer.String() != "oversized body buffer" {
		t.Fatal("oversized body buffer was reset instead of discarded")
	}
	responseBuffer := bytes.NewBuffer(make([]byte, 0, maxPooledResponseCapacity+1))
	responseBuffer.WriteString("oversized response buffer")
	releaseResponseBuffer(responseBuffer)
	if responseBuffer.String() != "oversized response buffer" {
		t.Fatal("oversized response buffer was reset instead of discarded")
	}

	cleanupParsedMultipart(&http.Request{})
	cleanupParsedMultipart(&http.Request{MultipartForm: &multipart.Form{}})
	cleanupMultipart(nil)
	cleanupMultipart(&http.Request{MultipartForm: &multipart.Form{}})
}

func TestRequestResponseEdgeJSONDecodeAndScanners(t *testing.T) {
	fields, err := decodeJSONFields([]byte(`{"name":"first","name":"last","other":1}`), []string{"name"})
	if err != nil || string(fields["name"]) != `"last"` || len(fields) != 1 {
		t.Fatalf("decodeJSONFields = (%#v, %v), want last name", fields, err)
	}
	_, err = decodeJSONFields([]byte(`{`), []string{"name"})
	var syntaxErr *json.SyntaxError
	if err == nil || !errors.As(err, &syntaxErr) || !strings.Contains(err.Error(), "invalid JSON body") {
		t.Fatalf("invalid JSON error = %v, want wrapped syntax error", err)
	}
	fields, err = decodeJSONFieldsSet([]byte(`{}`), newRequestKeySet([]string{"name"}))
	if err != nil || fields == nil || len(fields) != 0 {
		t.Fatalf("empty object decode = (%#v, %v)", fields, err)
	}
	if _, err = decodeJSONFieldsSet([]byte(`null`), nil); !errors.Is(err, multipleRequireError) {
		t.Fatalf("null decode error = %v, want multipleRequireError", err)
	}
	if _, err = decodeJSONFieldsSet([]byte(`[]`), nil); err == nil {
		t.Fatal("array decode succeeded, want object error")
	}

	if got := skipJSONSpace(nil, 0); got != 0 {
		t.Fatalf("skipJSONSpace(nil) = %d, want 0", got)
	}
	if got := skipJSONSpace([]byte(" \t\r\n"), 2); got != 4 {
		t.Fatalf("skipJSONSpace(end) = %d, want 4", got)
	}
	for _, input := range []string{`{"a":1`, `[1,2`, `true`, `123`} {
		if got := skipJSONValueEnd([]byte(input), 0); got != len(input) {
			t.Fatalf("skipJSONValueEnd(%q) = %d, want %d", input, got, len(input))
		}
	}
	if got := scanJSONStringEnd([]byte(`"a\\\"b"`), 0); got != len(`"a\\\"b"`) {
		t.Fatalf("scanJSONStringEnd escaped string = %d, want %d", got, len(`"a\\\"b"`))
	}
	if got := jsonFirstNonSpace([]byte(" \n\t")); got != 0 {
		t.Fatalf("jsonFirstNonSpace(empty) = %d, want 0", got)
	}
	visits := 0
	stopped := visitJSONArray([]byte(`[1,2,3]`), func([]byte) bool {
		visits++
		return visits == 2
	})
	if !stopped || visits != 2 {
		t.Fatalf("visitJSONArray early stop = (%v, %d), want (true, 2)", stopped, visits)
	}
}

type rrEdgeJSONItem struct {
	Value int `json:"value"`
}

type rrEdgeJSONExact struct {
	Value int `json:"value"`
}

type rrEdgeJSONCustom struct {
	Seen map[string]json.RawMessage
}

func (v *rrEdgeJSONCustom) UnmarshalJSON(data []byte) error {
	return json.Unmarshal(data, &v.Seen)
}

type rrEdgeEndlessPointer *rrEdgeEndlessPointer

func TestRequestResponseEdgeFilterExactJSONValue(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		typ   reflect.Type
		want  string
	}{
		{name: "struct wrong case and duplicates", input: ` {"Value":99,"value":1,"value":2,"other":3} `, typ: reflect.TypeFor[rrEdgeJSONItem](), want: `{"value":1,"value":2}`},
		{name: "struct non object", input: `[{"value":1}]`, typ: reflect.TypeFor[rrEdgeJSONItem](), want: `[{"value":1}]`},
		{name: "map nested structs", input: `{"a":{"Value":9,"value":1},"b":{"value":2}}`, typ: reflect.TypeFor[map[string]rrEdgeJSONItem](), want: `{"a":{"value":1},"b":{"value":2}}`},
		{name: "map non object", input: `[]`, typ: reflect.TypeFor[map[string]rrEdgeJSONItem](), want: `[]`},
		{name: "slice nested structs", input: `[{"Value":9,"value":1},null,{"value":2}]`, typ: reflect.TypeFor[[]rrEdgeJSONItem](), want: `[{"value":1},null,{"value":2}]`},
		{name: "array nested structs", input: `[{"value":1},{"Value":9,"value":2}]`, typ: reflect.TypeFor[[2]rrEdgeJSONItem](), want: `[{"value":1},{"value":2}]`},
		{name: "slice non array", input: `{}`, typ: reflect.TypeFor[[]rrEdgeJSONItem](), want: `{}`},
		{name: "byte slice unchanged", input: `"YQ=="`, typ: reflect.TypeFor[[]byte](), want: `"YQ=="`},
		{name: "scalar unchanged", input: `42`, typ: reflect.TypeFor[int](), want: `42`},
		{name: "custom decoder unchanged", input: `{"Value":7}`, typ: reflect.TypeFor[rrEdgeJSONCustom](), want: `{"Value":7}`},
		{name: "pointer depth fallback", input: `{"value":1}`, typ: reflect.TypeFor[rrEdgeEndlessPointer](), want: `{"value":1}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(filterExactJSONValue([]byte(tc.input), tc.typ)); got != tc.want {
				t.Fatalf("filterExactJSONValue = %s, want %s", got, tc.want)
			}
		})
	}

	value, err := decodeJSONExactStruct(reflect.TypeFor[rrEdgeJSONExact](), []byte(`{"value":2,"Value":9}`))
	if err != nil || value.Interface().(rrEdgeJSONExact).Value != 2 {
		t.Fatalf("exact struct decode = (%v, %v), want value 2", value, err)
	}
	_, err = decodeJSONExactStruct(reflect.TypeFor[rrEdgeJSONExact](), []byte(`{"value":"bad","value":2}`))
	var typeErr *json.UnmarshalTypeError
	if !errors.As(err, &typeErr) {
		t.Fatalf("duplicate field decode error = %v, want earlier duplicate type error", err)
	}
	custom, err := decodeJSONExactStruct(reflect.TypeFor[rrEdgeJSONCustom](), []byte(`{"Value":7}`))
	if err != nil || custom.Interface().(rrEdgeJSONCustom).Seen["Value"] == nil {
		t.Fatalf("custom decoder = (%v, %v), want original member name", custom, err)
	}
	if _, err = decodeJSONExactStruct(nil, []byte(`{}`)); err == nil {
		t.Fatal("nil exact JSON target succeeded")
	}
	if _, err = decodeJSONExactStruct(reflect.TypeFor[rrEdgeJSONExact](), []byte(`{`)); err == nil {
		t.Fatal("invalid exact JSON succeeded")
	}
	if _, err = decodeJSON(nil, []byte(`{}`)); err == nil {
		t.Fatal("nil JSON target succeeded")
	}
	if _, err = decodeJSON(reflect.TypeFor[int](), []byte(`"bad"`)); err == nil {
		t.Fatal("invalid JSON value succeeded")
	}

	for _, tc := range []struct {
		input string
		typ   reflect.Type
		want  bool
	}{
		{input: `null`, typ: reflect.TypeFor[rrEdgeJSONItem]()},
		{input: `[]`, typ: reflect.TypeFor[map[string]rrEdgeJSONItem]()},
		{input: `{}`, typ: reflect.TypeFor[[]rrEdgeJSONItem]()},
		{input: `"YQ=="`, typ: reflect.TypeFor[[]byte]()},
		{input: `{"a":{"Value":1}}`, typ: reflect.TypeFor[map[string]rrEdgeJSONItem](), want: true},
		{input: `[{"Value":1}]`, typ: reflect.TypeFor[[]rrEdgeJSONItem](), want: true},
		{input: `{"Value":1}`, typ: reflect.TypeFor[rrEdgeEndlessPointer]()},
	} {
		if got := jsonNeedsExactFilter([]byte(tc.input), tc.typ); got != tc.want {
			t.Fatalf("jsonNeedsExactFilter(%s, %s) = %v, want %v", tc.input, tc.typ, got, tc.want)
		}
	}
}

type rrEdgeRecursiveForm struct {
	Next  *rrEdgeRecursiveForm `json:"next"`
	Value string               `json:"value"`
}

type rrEdgeCountForm struct {
	Count int `json:"count"`
}

func TestRequestResponseEdgeFormAllocationLimit(t *testing.T) {
	state := formBindState{allocations: maxFormPointerExpansion}
	_, err := bindStructFieldsState(reflect.TypeFor[rrEdgeCountForm](),
		structFieldsOf(reflect.TypeFor[rrEdgeCountForm]()), &nameSources{}, &state)
	if err == nil || !strings.Contains(err.Error(), "form pointer expansion") {
		t.Fatalf("allocation limit error = %v, want form pointer expansion rejection", err)
	}
	_, err = bindFormParameterDepth(reflect.TypeFor[*rrEdgeCountForm](), "count",
		&nameSources{form: url.Values{"count": {"1"}}}, 0, &state)
	if err == nil || !strings.Contains(err.Error(), "form pointer expansion") {
		t.Fatalf("pointer allocation limit error = %v, want form pointer expansion rejection", err)
	}
}

func TestInternalPoolReleaseBounds(t *testing.T) {
	releaseJSONFields(nil)
	fields := acquireJSONFields(maxPooledJSONFieldKeys + 1)
	releaseJSONFields(fields)

	releaseRequestReadTracker(nil)
	tracker := acquireRequestReadTracker(http.NoBody)
	releaseRequestReadTracker(tracker)

	buffer := make([]byte, 0, maxPooledPathBytes+1)
	pathPool := &buffer
	releasePathBytes(pathPool, buffer)
}

func TestRequestResponseEdgeBindFormParameterIntoBranches(t *testing.T) {
	t.Run("file header", func(t *testing.T) {
		header := &multipart.FileHeader{Filename: "a.txt"}
		dst := reflect.New(reflect.TypeFor[multipart.FileHeader]()).Elem()
		err := bindFormParameterInto(dst, "file", &nameSources{
			files: filesMap{"file": {header}},
		}, 0, &formBindState{})
		if err != nil || dst.Interface().(multipart.FileHeader).Filename != "a.txt" {
			t.Fatalf("bound file header = (%#v, %v)", dst.Interface(), err)
		}
	})

	t.Run("returning text pointer depth", func(t *testing.T) {
		_, err := bindTextValueDepth(reflect.TypeFor[*int](), []string{"1"}, maxTypePointerDepth)
		if err == nil || !strings.Contains(err.Error(), "recursive pointer type") {
			t.Fatalf("text pointer depth error = %v", err)
		}
	})

	t.Run("empty pointer setter", func(t *testing.T) {
		dst := reflect.New(reflect.TypeFor[*int]()).Elem()
		dst.Set(reflect.New(reflect.TypeFor[int]()))
		if err := setTextValue(dst, nil); err != nil || !dst.IsNil() {
			t.Fatalf("empty pointer setter = (%v, %v), want nil", dst, err)
		}
	})

	t.Run("pointer depth", func(t *testing.T) {
		dst := reflect.New(reflect.TypeFor[*int]()).Elem()
		err := bindFormParameterInto(dst, "value", &nameSources{}, maxTypePointerDepth,
			&formBindState{})
		if err == nil || !strings.Contains(err.Error(), "recursive pointer type") {
			t.Fatalf("pointer depth error = %v", err)
		}
	})

	t.Run("recursive cycle", func(t *testing.T) {
		typ := reflect.TypeFor[rrEdgeRecursiveForm]()
		state := &formBindState{}
		if err := state.push(typ); err != nil {
			t.Fatal(err)
		}
		dst := reflect.New(reflect.TypeFor[*rrEdgeRecursiveForm]()).Elem()
		if err := bindFormParameterInto(dst, "next", &nameSources{}, 0, state); err != nil ||
			!dst.IsNil() {
			t.Fatalf("recursive cycle = (%v, %v), want nil", dst, err)
		}
	})

	t.Run("returning binder recursive cycle", func(t *testing.T) {
		typ := reflect.TypeFor[rrEdgeRecursiveForm]()
		state := &formBindState{}
		if err := state.push(typ); err != nil {
			t.Fatal(err)
		}
		value, err := bindFormParameterDepth(reflect.TypeFor[*rrEdgeRecursiveForm](), "next",
			&nameSources{}, 0, state)
		if err != nil || !value.IsNil() {
			t.Fatalf("returning binder cycle = (%v, %v), want nil", value, err)
		}
	})

	t.Run("recursive leaf probe error", func(t *testing.T) {
		state := &formBindState{size: maxTypePointerDepth}
		dst := reflect.New(reflect.TypeFor[*rrEdgeRecursiveForm]()).Elem()
		err := bindFormParameterInto(dst, "next", &nameSources{}, 0, state)
		if err == nil || !strings.Contains(err.Error(), "struct nesting exceeds") {
			t.Fatalf("leaf probe error = %v", err)
		}
	})

	t.Run("recursive without leaf", func(t *testing.T) {
		dst := reflect.New(reflect.TypeFor[*rrEdgeRecursiveForm]()).Elem()
		if err := bindFormParameterInto(dst, "next", &nameSources{}, 0,
			&formBindState{}); err != nil || !dst.IsNil() {
			t.Fatalf("missing recursive leaf = (%v, %v), want nil", dst, err)
		}
	})

	t.Run("missing scalar pointer", func(t *testing.T) {
		dst := reflect.New(reflect.TypeFor[*int]()).Elem()
		if err := bindFormParameterInto(dst, "value", &nameSources{}, 0,
			&formBindState{}); err != nil || !dst.IsNil() {
			t.Fatalf("missing scalar pointer = (%v, %v), want nil", dst, err)
		}
	})

	t.Run("pointer allocation limit", func(t *testing.T) {
		state := &formBindState{allocations: maxFormPointerExpansion}
		dst := reflect.New(reflect.TypeFor[*rrEdgeCountForm]()).Elem()
		err := bindFormParameterInto(dst, "count", &nameSources{
			form: url.Values{"count": {"1"}},
		}, 0, state)
		if err == nil || !strings.Contains(err.Error(), "form pointer expansion") {
			t.Fatalf("pointer allocation error = %v", err)
		}
	})

	t.Run("array length", func(t *testing.T) {
		dst := reflect.New(reflect.TypeFor[[2]int]()).Elem()
		err := bindFormParameterInto(dst, "pair", &nameSources{
			form: url.Values{"pair": {"1"}},
		}, 0, &formBindState{})
		if err == nil || !strings.Contains(err.Error(), "array requires 2 values") {
			t.Fatalf("array length error = %v", err)
		}
	})

	t.Run("slice element", func(t *testing.T) {
		dst := reflect.New(reflect.TypeFor[[]bool]()).Elem()
		err := bindFormParameterInto(dst, "flags", &nameSources{
			form: url.Values{"flags": {"bad"}},
		}, 0, &formBindState{})
		if err == nil || !strings.Contains(err.Error(), "slice index 0") {
			t.Fatalf("slice element error = %v", err)
		}
	})
}

func TestRequestResponseEdgeFormBindingDepthAndRecursion(t *testing.T) {
	var state formBindState
	first := reflect.TypeFor[int]()
	if state.contains(first) {
		t.Fatal("empty form state contains a type")
	}
	for range maxTypePointerDepth {
		if err := state.push(first); err != nil {
			t.Fatalf("push within limit: %v", err)
		}
	}
	if !state.contains(first) {
		t.Fatal("form state does not contain the pushed type")
	}
	if err := state.push(first); err == nil || !strings.Contains(err.Error(), "struct nesting exceeds") {
		t.Fatalf("overflow push error = %v", err)
	}
	state.pop()
	if state.size != maxTypePointerDepth-1 {
		t.Fatalf("state size after pop = %d", state.size)
	}

	deep := reflect.TypeFor[int]()
	for range maxTypePointerDepth + 1 {
		deep = reflect.PointerTo(deep)
	}
	src := &nameSources{path: map[string]string{"value": "1"}}
	if _, err := bindFormParameter(deep, "value", src); err == nil || !strings.Contains(err.Error(), "recursive pointer type") {
		t.Fatalf("deep pointer bind error = %v", err)
	}

	recursive := reflect.TypeFor[rrEdgeRecursiveForm]()
	missing, err := bindFormParameter(reflect.TypeFor[*rrEdgeRecursiveForm](), "next", &nameSources{})
	if err != nil || !missing.IsNil() {
		t.Fatalf("missing recursive pointer = (%v, %v), want nil", missing, err)
	}
	src = &nameSources{form: url.Values{"next": {"present"}, "value": {"bound"}}}
	bound, err := bindFormParameter(recursive, "root", src)
	if err != nil {
		t.Fatalf("bind recursive form: %v", err)
	}
	got := bound.Interface().(rrEdgeRecursiveForm)
	if got.Next != nil || got.Value != "bound" {
		t.Fatalf("recursive form = %#v, want nil Next and bound Value", got)
	}

	var active formBindState
	if err := active.push(recursive); err != nil {
		t.Fatalf("push recursive type: %v", err)
	}
	zero, err := bindStructFieldsState(recursive, structFieldsOf(recursive), &nameSources{}, &active)
	if err != nil || !zero.IsZero() || active.size != 1 {
		t.Fatalf("active recursion guard = (%v, %v, size=%d)", zero, err, active.size)
	}
	active.pop()

	var full formBindState
	full.size = len(full.active)
	if _, err := bindStructFieldsState(reflect.TypeFor[rrEdgeCountForm](), structFieldsOf(reflect.TypeFor[rrEdgeCountForm]()), &nameSources{}, &full); err == nil {
		t.Fatal("over-nested struct binding succeeded")
	}
	if _, err := bindStruct(reflect.TypeFor[rrEdgeCountForm](), &nameSources{path: map[string]string{"count": "bad"}}); err == nil || !strings.Contains(err.Error(), `path field "count"`) {
		t.Fatalf("path field error = %v", err)
	}
	if _, err := bindStruct(reflect.TypeFor[rrEdgeCountForm](), &nameSources{form: url.Values{"count": {"bad"}}}); err == nil || !strings.Contains(err.Error(), `field "count"`) {
		t.Fatalf("form field error = %v", err)
	}
}

func TestRequestResponseEdgeParamNamesAndSourcePresence(t *testing.T) {
	if got := paramNameAt([]string{"first"}, 0); got != "first" {
		t.Fatalf("paramNameAt present = %q", got)
	}
	if got := paramNameAt([]string{"first"}, 1); got != "arg1" {
		t.Fatalf("paramNameAt fallback = %q, want arg1", got)
	}
	if got := paramNameAt(nil, 3); got != "arg3" {
		t.Fatalf("paramNameAt nil fallback = %q, want arg3", got)
	}
	for _, tc := range []struct {
		name string
		src  nameSources
		want bool
	}{
		{name: "path", src: nameSources{path: map[string]string{"key": ""}}, want: true},
		{name: "form", src: nameSources{form: url.Values{"key": nil}}, want: true},
		{name: "files", src: nameSources{files: filesMap{"key": nil}}, want: true},
		{name: "missing", src: nameSources{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.src.has("key"); got != tc.want {
				t.Fatalf("nameSources.has = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRequestResponseEdgeContextPendingAndSSE(t *testing.T) {
	t.Run("flush commits a staged response immediately", func(t *testing.T) {
		sink := &rrEdgeFlushWriter{rrEdgeWriter: rrEdgeNewWriter()}
		rw := &pooledResponseWriter{w: sink}
		rw.WriteHeader(http.StatusAccepted)
		rrEdgeRun(t, func(ctx context.Context) error { return rw.FlushError() })
		_, statuses, _, _ := sink.snapshot()
		flushes, _ := sink.flushSnapshot()
		if !rw.flushed() || !reflect.DeepEqual(statuses, []int{http.StatusAccepted}) || flushes != 1 {
			t.Fatalf("flushed response = (flushed=%v, statuses=%v, flushes=%d)", rw.flushed(), statuses, flushes)
		}
	})

	t.Run("HEAD SSE only commits headers", func(t *testing.T) {
		called := false
		e := requestTestEngine(t, http.MethodGet, "/events", func(SSEWriter) { called = true }, []string{"stream"})
		sink := rrEdgeNewWriter()
		r := httptest.NewRequest(http.MethodHead, "/events", nil)
		rrEdgeRun(t, func(ctx context.Context) error {
			e.ServeHTTP(sink, r.WithContext(ctx))
			return ctx.Err()
		})
		header, statuses, body, _ := sink.snapshot()
		if called || !reflect.DeepEqual(statuses, []int{http.StatusOK}) || body != "" {
			t.Fatalf("HEAD SSE response = (called=%v, statuses=%v, body=%q)", called, statuses, body)
		}
		if header.Get(contentType) != sseContentType || header.Get("Cache-Control") != sseCacheControl {
			t.Fatalf("HEAD SSE headers = %v", header)
		}
	})

	t.Run("SSE startup error after flush does not rewrite", func(t *testing.T) {
		flushErr := errors.New("injected SSE flush failure")
		logger := &rrEdgeLogger{}
		called := false
		e := requestTestEngine(t, http.MethodGet, "/events", func(SSEWriter) { called = true }, []string{"stream"})
		e.logger = logger
		sink := &rrEdgeFlushWriter{rrEdgeWriter: rrEdgeNewWriter(), flushErr: flushErr}
		r := httptest.NewRequest(http.MethodGet, "/events", nil)
		rrEdgeRun(t, func(ctx context.Context) error {
			e.ServeHTTP(sink, r.WithContext(ctx))
			return ctx.Err()
		})
		_, statuses, body, _ := sink.snapshot()
		flushes, deadlines := sink.flushSnapshot()
		if called || !reflect.DeepEqual(statuses, []int{http.StatusOK}) || body != "" || flushes != 1 || deadlines != 1 {
			t.Fatalf("failed SSE startup = (called=%v, statuses=%v, body=%q, flushes=%d, deadlines=%d)", called, statuses, body, flushes, deadlines)
		}
		entries := logger.snapshot()
		if len(entries) != 1 || entries[0].message != "SSE startup failed" || !errors.Is(entries[0].kv[1].(error), flushErr) {
			t.Fatalf("SSE startup logs = %#v", entries)
		}
	})
}

func TestRequestResponseEdgeWriteRequestError(t *testing.T) {
	oldLogger := defaultEngine.logger
	defer func() { defaultEngine.logger = oldLogger }()
	fallback := &rrEdgeLogger{}
	defaultEngine.logger = fallback
	client := &requestClientError{err: errors.New("bad input")}
	internal := &requestInternalError{err: errors.New("private I/O detail")}
	for _, tc := range []struct {
		name             string
		err              error
		loggerMode       string
		defaultNil       bool
		status           int
		body             string
		wantProvidedLogs int
		wantFallbackLogs int
	}{
		{name: "body sentinel", err: requestBodyTooLargeError, loggerMode: "provided", status: http.StatusRequestEntityTooLarge, body: "Request Entity Too Large"},
		{name: "MaxBytes", err: &http.MaxBytesError{Limit: 10}, loggerMode: "provided", status: http.StatusRequestEntityTooLarge, body: "Request Entity Too Large"},
		{name: "client", err: client, loggerMode: "provided", status: http.StatusBadRequest, body: "bad input"},
		{name: "provided internal logger", err: internal, loggerMode: "provided", status: http.StatusInternalServerError, body: "Internal Server Error", wantProvidedLogs: 1},
		{name: "nil logger falls back", err: internal, loggerMode: "nil", status: http.StatusInternalServerError, body: "Internal Server Error", wantFallbackLogs: 1},
		{name: "default internal logger", err: internal, status: http.StatusInternalServerError, body: "Internal Server Error", wantFallbackLogs: 1},
		{name: "nil default logger", err: internal, defaultNil: true, status: http.StatusInternalServerError, body: "Internal Server Error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provided := &rrEdgeLogger{}
			fallback := &rrEdgeLogger{}
			defaultEngine.logger = fallback
			if tc.defaultNil {
				defaultEngine.logger = nil
			}
			var loggers []Logger
			switch tc.loggerMode {
			case "provided":
				loggers = []Logger{provided}
			case "nil":
				loggers = []Logger{nil}
			}
			halt := rrEdgeAbort(t, func() { writeRequestError(tc.err, loggers...) })
			if halt.status != tc.status || halt.body != tc.body {
				t.Fatalf("writeRequestError abort = (%d, %q), want (%d, %q)", halt.status, halt.body, tc.status, tc.body)
			}
			if got := len(provided.snapshot()); got != tc.wantProvidedLogs {
				t.Fatalf("provided logger entries = %d, want %d", got, tc.wantProvidedLogs)
			}
			if got := len(fallback.snapshot()); got != tc.wantFallbackLogs {
				t.Fatalf("fallback logger entries = %d, want %d", got, tc.wantFallbackLogs)
			}
		})
	}
}

func TestRequestResponseEdgeResponseWriterCapabilities(t *testing.T) {
	t.Run("nil hijack connection is not tracked", func(t *testing.T) {
		registry := newHijackRegistry()
		conn, ok := registry.track(nil)
		if conn != nil || ok || len(registry.connections) != 0 {
			t.Fatalf("track(nil) = (%v, %v), registry size=%d", conn, ok, len(registry.connections))
		}
	})

	t.Run("flush preserves the staged status and returns the underlying error", func(t *testing.T) {
		flushErr := errors.New("flush failed")
		sink := &rrEdgeFlushWriter{rrEdgeWriter: rrEdgeNewWriter(), flushErr: flushErr}
		rw := &pooledResponseWriter{w: sink}
		rw.WriteHeader(http.StatusCreated)
		rrEdgeRun(t, func(ctx context.Context) error {
			if err := rw.FlushError(); !errors.Is(err, flushErr) {
				return fmt.Errorf("FlushError() = %v, want %v", err, flushErr)
			}
			return nil
		})
		_, statuses, _, _ := sink.snapshot()
		flushes, _ := sink.flushSnapshot()
		if !rw.flushed() || !reflect.DeepEqual(statuses, []int{http.StatusCreated}) || flushes != 1 {
			t.Fatalf("flushed state = (flushed=%v, statuses=%v, flushes=%d)", rw.flushed(), statuses, flushes)
		}
	})

	t.Run("unsupported flush and nil unwrap", func(t *testing.T) {
		sink := rrEdgeNewWriter()
		rw := &pooledResponseWriter{w: sink}
		var err error
		rrEdgeRun(t, func(ctx context.Context) error { err = rw.FlushError(); return nil })
		if !errors.Is(err, http.ErrNotSupported) || rw.flushed() {
			t.Fatalf("unsupported FlushError = %v, flushed=%v", err, rw.flushed())
		}
		if supportsFlush(nil) || supportsFlush(&rrEdgeUnwrapWriter{}) {
			t.Fatal("nil unwrap chain reported flush support")
		}
		stream := &rrEdgeFlushWriter{rrEdgeWriter: rrEdgeNewWriter()}
		if !supportsFlush(&rrEdgeUnwrapWriter{ResponseWriter: stream}) {
			t.Fatal("wrapped flush capability was not found")
		}
	})

	t.Run("hijack error success and repeat", func(t *testing.T) {
		unsupported := &pooledResponseWriter{w: rrEdgeNewWriter()}
		var conn net.Conn
		var buffered *bufio.ReadWriter
		var err error
		rrEdgeRun(t, func(ctx context.Context) error { conn, buffered, err = unsupported.Hijack(); return nil })
		if !errors.Is(err, http.ErrNotSupported) || conn != nil || buffered != nil || unsupported.hijacked() {
			t.Fatalf("unsupported Hijack = (%v, %v, %v, %v)", conn, buffered, err, unsupported.hijacked())
		}
		hijackErr := errors.New("hijack failed")
		failed := &pooledResponseWriter{w: &rrEdgeHijackWriter{rrEdgeWriter: rrEdgeNewWriter(), err: hijackErr}}
		rrEdgeRun(t, func(ctx context.Context) error { _, _, err = failed.Hijack(); return nil })
		if !errors.Is(err, hijackErr) || failed.hijacked() {
			t.Fatalf("failed Hijack = (%v, %v)", err, failed.hijacked())
		}
		succeeded := &pooledResponseWriter{w: &rrEdgeHijackWriter{rrEdgeWriter: rrEdgeNewWriter()}}
		rrEdgeRun(t, func(ctx context.Context) error { conn, buffered, err = succeeded.Hijack(); return err })
		if err != nil || conn != nil || buffered != nil || !succeeded.hijacked() {
			t.Fatalf("successful Hijack = (%v, %v, %v, %v)", conn, buffered, err, succeeded.hijacked())
		}
		rrEdgeRun(t, func(ctx context.Context) error { _, _, err = succeeded.Hijack(); return nil })
		if !errors.Is(err, http.ErrHijacked) {
			t.Fatalf("repeated Hijack error = %v", err)
		}
		rrEdgeRun(t, func(ctx context.Context) error { _, err = succeeded.Write([]byte("no")); return nil })
		if !errors.Is(err, http.ErrHijacked) {
			t.Fatalf("Write after Hijack error = %v", err)
		}
	})
}

type rrEdgePanicError struct{}

func (rrEdgePanicError) Error() string { panic("error method panic") }

func TestRequestResponseEdgeErrorWritebackPriorState(t *testing.T) {
	body, mediaType := errorWriteback(rrEdgePanicError{})
	if string(body) != string(typeRecognitionError) || mediaType != textPlainCharset {
		t.Fatalf("panicking error writeback = (%q, %q)", body, mediaType)
	}

	e := newEngine()
	defer e.cancelStreams()
	sink := rrEdgeNewWriter()
	rw := &pooledResponseWriter{w: sink}
	rw.WriteHeader(http.StatusAccepted)
	rw.Header().Set(contentType, "application/x-prior")
	rrEdgeRun(t, func(ctx context.Context) error {
		e.writeResponseAbort(rw, responseAbort{status: http.StatusForbidden, body: "must not replace"})
		return nil
	})
	header, statuses, responseBody, _ := sink.snapshot()
	if !reflect.DeepEqual(statuses, []int{http.StatusAccepted}) || responseBody != "" || header.Get(contentType) != "application/x-prior" {
		t.Fatalf("prior response changed = (statuses=%v, body=%q, header=%v)", statuses, responseBody, header)
	}

	for _, tc := range []struct {
		name    string
		prepare func(*pooledResponseWriter)
	}{
		{name: "flushed", prepare: func(rw *pooledResponseWriter) { rw.WriteHeader(http.StatusAccepted); rw.writeHeader() }},
		{name: "written", prepare: func(rw *pooledResponseWriter) { _, _ = rw.Write([]byte("committed")) }},
		{name: "hijacked", prepare: func(rw *pooledResponseWriter) { rw.hijackedStatus = true }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sink := rrEdgeNewWriter()
			rw := &pooledResponseWriter{w: sink}
			tc.prepare(rw)
			_, beforeStatuses, beforeBody, _ := sink.snapshot()
			rrEdgeRun(t, func(ctx context.Context) error {
				e.writeResponseAbort(rw, responseAbort{status: http.StatusForbidden, body: "ignored"})
				return nil
			})
			_, afterStatuses, afterBody, _ := sink.snapshot()
			if !reflect.DeepEqual(afterStatuses, beforeStatuses) || afterBody != beforeBody {
				t.Fatalf("committed response changed: before=(%v, %q), after=(%v, %q)", beforeStatuses, beforeBody, afterStatuses, afterBody)
			}
		})
	}
}
