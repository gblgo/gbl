// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const requestStreamingTimeout = 2 * time.Second

type requestStreamingChunkBody struct {
	chunks     []string
	reads      int
	closeCalls int
}

func (b *requestStreamingChunkBody) Read(p []byte) (int, error) {
	b.reads++
	if len(b.chunks) == 0 {
		return 0, io.EOF
	}
	n := copy(p, b.chunks[0])
	if n == len(b.chunks[0]) {
		b.chunks = b.chunks[1:]
	} else {
		b.chunks[0] = b.chunks[0][n:]
	}
	return n, nil
}

func (b *requestStreamingChunkBody) Close() error {
	b.closeCalls++
	return nil
}

func TestPrepareRequestBodyNormalizesBodyMethods(t *testing.T) {
	requestTestLimits(t, 4, 8, 128)
	for _, method := range []string{"post", "put", "patch", "PoSt", "pUt", "pAtCh"} {
		t.Run(method, func(t *testing.T) {
			body := &requestStreamingChunkBody{chunks: []string{"data"}}
			r := httptest.NewRequest(method, "/", body)
			r.ContentLength = 5
			err := prepareRequestBody(httptest.NewRecorder(), r)
			if !errors.Is(err, requestBodyTooLargeError) {
				t.Fatalf("prepareRequestBody error = %v, want requestBodyTooLargeError", err)
			}
			if body.reads != 0 {
				t.Fatalf("body reads = %d, want 0 before rejection", body.reads)
			}
		})
	}
}

func TestRawUnknownBodyStreamsFirstChunkAndBoundsReads(t *testing.T) {
	requestTestLimits(t, 8, 8, 128)
	body := &requestStreamingChunkBody{chunks: []string{"first", "second"}}
	var (
		called      bool
		readsBefore int
		firstN      int
		firstErr    error
		firstData   string
		total       int
		readErr     error
	)
	e := requestTestEngine(t, http.MethodPost, "/raw", func(r *http.Request) string {
		called = true
		readsBefore = body.reads
		buf := make([]byte, len("first"))
		firstN, firstErr = r.Body.Read(buf)
		firstData = string(buf[:firstN])
		total = firstN
		for {
			n, err := r.Body.Read(make([]byte, 1))
			total += n
			if err != nil {
				readErr = err
				break
			}
		}
		return "streamed"
	}, []string{"r"})
	r := httptest.NewRequest(http.MethodPost, "/raw", body)
	r.Header.Set(contentType, "text/plain")
	r.ContentLength = -1
	r.TransferEncoding = []string{"chunked"}
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, r)

	if recorder.Code != http.StatusOK || recorder.Body.String() != "streamed" {
		t.Fatalf("raw response = %d %q, want 200 streamed", recorder.Code, recorder.Body.String())
	}
	if !called || readsBefore != 0 {
		t.Fatalf("handler called=%v, body reads before handler=%d, want true and 0", called, readsBefore)
	}
	if firstN != len("first") || firstErr != nil || firstData != "first" {
		t.Fatalf("first read = (%d, %v, %q), want (5, nil, first)", firstN, firstErr, firstData)
	}
	var maxBytesErr *http.MaxBytesError
	if !errors.As(readErr, &maxBytesErr) {
		t.Fatalf("limiting read error = %v, want MaxBytesError", readErr)
	}
	if total != 8 {
		t.Fatalf("accepted bytes = %d, want 8", total)
	}
}

func TestRawKnownOversizeBodyIsRejectedBeforeHandler(t *testing.T) {
	requestTestLimits(t, 8, 8, 128)
	for _, method := range []string{"post", "put", "patch"} {
		t.Run(method, func(t *testing.T) {
			called := false
			e := requestTestEngine(t, method, "/raw", func(*http.Request) string {
				called = true
				return "unexpected"
			}, nil)
			r := httptest.NewRequest(method, "/raw", strings.NewReader("123456789"))
			recorder := httptest.NewRecorder()
			e.ServeHTTP(recorder, r)
			if recorder.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("raw response = %d %q, want 413", recorder.Code, recorder.Body.String())
			}
			if called {
				t.Fatal("handler ran with a known oversized body")
			}
		})
	}
}

func TestRawChunkedBodyIsNotPreRead(t *testing.T) {
	requestTestLimits(t, 8, 8, 128)
	type firstReadResult struct {
		n    int
		err  error
		data string
	}
	type finalReadResult struct {
		total int
		err   error
	}
	type responseResult struct {
		status int
		body   string
		err    error
	}
	firstRead := make(chan firstReadResult, 1)
	finalRead := make(chan finalReadResult, 1)
	e := requestTestEngine(t, http.MethodPost, "/raw", func(r *http.Request) string {
		buf := make([]byte, len("first"))
		n, err := r.Body.Read(buf)
		firstRead <- firstReadResult{n: n, err: err, data: string(buf[:n])}
		total := n
		for {
			n, err = r.Body.Read(make([]byte, 1))
			total += n
			if err != nil {
				finalRead <- finalReadResult{total: total, err: err}
				break
			}
		}
		return "streamed"
	}, []string{"r"})
	server := httptest.NewServer(e)
	defer server.Close()

	reader, writer := io.Pipe()
	request, err := http.NewRequest(http.MethodPost, server.URL+"/raw", reader)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set(contentType, "application/octet-stream")
	request.ContentLength = -1
	client := &http.Client{Timeout: 5 * time.Second}
	responseDone := make(chan responseResult, 1)
	go func() {
		response, err := client.Do(request)
		if err != nil {
			responseDone <- responseResult{err: err}
			return
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		responseDone <- responseResult{status: response.StatusCode, body: string(body), err: err}
	}()

	writeDone := make(chan error, 1)
	go func() {
		_, err := io.WriteString(writer, "first")
		writeDone <- err
	}()
	var first firstReadResult
	select {
	case first = <-firstRead:
	case <-time.After(requestStreamingTimeout):
		_ = writer.CloseWithError(errors.New("first chunk was not read"))
		t.Fatal("handler did not read the first chunk before the body completed")
	}
	if first.n != len("first") || first.err != nil || first.data != "first" {
		t.Fatalf("first chunk read = (%d, %v, %q), want (5, nil, first)", first.n, first.err, first.data)
	}
	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatalf("write first chunk: %v", err)
		}
	case <-time.After(requestStreamingTimeout):
		t.Fatal("writing the first chunk timed out")
	}

	go func() {
		_, err := io.WriteString(writer, "second")
		writeDone <- err
	}()
	var final finalReadResult
	select {
	case final = <-finalRead:
	case <-time.After(requestStreamingTimeout):
		_ = writer.CloseWithError(errors.New("body limit was not enforced"))
		t.Fatal("handler did not observe the body limit")
	}
	var maxBytesErr *http.MaxBytesError
	if !errors.As(final.err, &maxBytesErr) || final.total != 8 {
		t.Fatalf("final read = (%d, %v), want 8 bytes and MaxBytesError", final.total, final.err)
	}
	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatalf("write second chunk: %v", err)
		}
	case <-time.After(requestStreamingTimeout):
		t.Fatal("writing the second chunk timed out")
	}
	_ = writer.Close()

	var response responseResult
	select {
	case response = <-responseDone:
	case <-time.After(requestStreamingTimeout):
		t.Fatal("raw chunked response timed out")
	}
	if response.err != nil || response.status != http.StatusOK || response.body != "streamed" {
		t.Fatalf("raw chunked response = (%d, %q, %v), want (200, streamed, nil)",
			response.status, response.body, response.err)
	}
}

func TestUnsupportedMediaLimitAndReplay(t *testing.T) {
	requestTestLimits(t, 4, 8, 128)
	t.Run("oversize is rejected before handler", func(t *testing.T) {
		called := false
		e := requestTestEngine(t, http.MethodPost, "/query", func(name string) string {
			called = true
			return name
		}, []string{"name"})
		r := httptest.NewRequest(http.MethodPost, "/query?name=ok", strings.NewReader("12345"))
		r.Header.Set(contentType, "text/plain")
		r.ContentLength = -1
		recorder := httptest.NewRecorder()
		e.ServeHTTP(recorder, r)
		if recorder.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("unsupported-media response = %d %q, want 413", recorder.Code, recorder.Body.String())
		}
		if called {
			t.Fatal("handler ran with an oversized unsupported-media body")
		}
	})

	t.Run("parsing without query keys still enforces limit", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/query", strings.NewReader("12345"))
		r.Header.Set(contentType, "text/plain")
		r.ContentLength = -1
		_, _, _, _, err := requestTestParse(t, r, nil)
		if !errors.Is(err, requestBodyTooLargeError) {
			t.Fatalf("unsupported-media error = %v, want requestBodyTooLargeError", err)
		}
	})

	t.Run("bounded body is replayed", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/query?name=ok", strings.NewReader("data"))
		r.Header.Set(contentType, "text/plain")
		r.ContentLength = -1
		form, _, _, _, err := requestTestParse(t, r, []string{"name"})
		if err != nil {
			t.Fatalf("parse bounded unsupported-media body: %v", err)
		}
		if form.Get("name") != "ok" {
			t.Fatalf("query form = %#v, want name=ok", form)
		}
		replay, ok := r.Body.(*replayRequestBody)
		if !ok || r.ContentLength != 4 {
			t.Fatalf("prepared body = %T, length=%d, want four-byte replay", r.Body, r.ContentLength)
		}
		data, err := io.ReadAll(replay)
		if err != nil || string(data) != "data" {
			t.Fatalf("replayed body = (%q, %v), want (data, nil)", data, err)
		}
	})

	t.Run("read failure remains internal", func(t *testing.T) {
		readFailure := errors.New("unsupported body read failed")
		body := &rrEdgeReadCloser{steps: []rrEdgeReadStep{{err: readFailure}}}
		r := httptest.NewRequest(http.MethodPost, "/query?name=ok", body)
		r.Header.Set(contentType, "text/plain")
		r.ContentLength = -1
		_, _, _, _, err := requestTestParse(t, r, []string{"name"})
		var internal *requestInternalError
		if !errors.As(err, &internal) || !errors.Is(err, readFailure) {
			t.Fatalf("unsupported-media error = %v, want wrapped internal read failure", err)
		}
	})
}

func TestURLEncodedBodyGrowsPooledBuffer(t *testing.T) {
	const initialCapacity = 64
	var pooled *bytes.Buffer
	originalNew := bodyBufferPool.New
	bodyBufferPool = sync.Pool{New: func() any {
		pooled = bytes.NewBuffer(make([]byte, 0, initialCapacity))
		return pooled
	}}
	t.Cleanup(func() { bodyBufferPool = sync.Pool{New: originalNew} })

	requestTestLimits(t, 4096, 8, 128)
	value := strings.Repeat("x", 512)
	payload := "name=" + value
	r := httptest.NewRequest(http.MethodPost, "/form", strings.NewReader(payload))
	r.Header.Set(contentType, xWwwFormUrlencoded)
	if r.ContentLength != int64(len(payload)) || len(payload) <= initialCapacity {
		t.Fatalf("fixture length/capacity = %d/%d, want a known length greater than %d",
			r.ContentLength, initialCapacity, initialCapacity)
	}
	form, _, _, _, err := requestTestParse(t, r, []string{"name"})
	if err != nil {
		t.Fatalf("parse urlencoded body: %v", err)
	}
	if got := form.Get("name"); got != value {
		t.Fatalf("form name = %q, want the %d-byte payload value", got, len(value))
	}
	if pooled == nil {
		t.Fatal("urlencoded parsing did not acquire the fixture buffer")
	}
	if pooled.Cap() < len(payload) {
		t.Fatalf("grown buffer capacity = %d, want at least %d", pooled.Cap(), len(payload))
	}
	if pooled.Len() != 0 {
		t.Fatalf("released buffer length = %d, want 0", pooled.Len())
	}
}

func TestReplayUnsupportedRequestBodyNil(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.Body = nil
	if err := replayUnsupportedRequestBody(r); err != nil {
		t.Fatalf("nil unsupported body: %v", err)
	}
}

func TestParsedMediaUnknownBodyLimitReturns413(t *testing.T) {
	requestTestLimits(t, 32, 8, 128)
	for _, mediaType := range []string{applicationJson, xWwwFormUrlencoded, multipartFormData} {
		t.Run(mediaType, func(t *testing.T) {
			var r *http.Request
			if mediaType == multipartFormData {
				r, _ = requestTestNewMultipart(t, "/body", nil,
					requestTestUpload{field: "upload", filename: "a.txt", body: []byte(strings.Repeat("x", 33))})
			} else {
				r = httptest.NewRequest(http.MethodPost, "/body", strings.NewReader(strings.Repeat("x", 33)))
				r.Header.Set(contentType, mediaType)
			}
			r.ContentLength = -1
			called := false
			e := requestTestEngine(t, http.MethodPost, "/body", func(name string) string {
				called = true
				return name
			}, []string{"name"})
			recorder := httptest.NewRecorder()
			e.ServeHTTP(recorder, r)
			if recorder.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("parsed-media response = %d %q, want 413", recorder.Code, recorder.Body.String())
			}
			if called {
				t.Fatal("handler ran with an oversized parsed-media body")
			}
		})
	}
}
