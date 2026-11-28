// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func mwTestNewEngine(t *testing.T, handler any, middleware ...HandlerFunc) *engine {
	t.Helper()
	e := newEngine()
	t.Cleanup(e.cancelStreams)
	e.use(middleware...)
	e.register(methodGet, "/test", handler)
	e.generateRoute()
	return e
}

func mwTestGzipResponse(e http.Handler, encodings ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "http://example.test/test", nil)
	for _, encoding := range encodings {
		req.Header.Add(acceptEncoding, encoding)
	}
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

func mwTestRequireGzipBody(t *testing.T, rec *httptest.ResponseRecorder, want []byte) {
	t.Helper()
	if got := rec.Header().Get(contentEncoding); got != encodingGzip {
		t.Fatalf("Content-Encoding = %q, want %q", got, encodingGzip)
	}
	zr, err := gzip.NewReader(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("open gzip body: %v", err)
	}
	got, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read gzip body: %v", err)
	}
	if err := zr.Close(); err != nil {
		t.Fatalf("close gzip body: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("decompressed body differs: got %d bytes, want %d", len(got), len(want))
	}
}

func mwTestRequirePlainBody(t *testing.T, rec *httptest.ResponseRecorder, want []byte) {
	t.Helper()
	if got := rec.Header().Get(contentEncoding); got != empty {
		t.Fatalf("Content-Encoding = %q, want no encoding", got)
	}
	if got := rec.Body.Bytes(); !bytes.Equal(got, want) {
		t.Fatalf("body differs: got %q, want %q", got, want)
	}
}

func mwTestHeaderHasToken(header http.Header, name, token string) bool {
	for _, value := range header.Values(name) {
		for _, part := range strings.Split(value, comma) {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

func mwTestRequireVary(t *testing.T, header http.Header, tokens ...string) {
	t.Helper()
	for _, token := range tokens {
		if !mwTestHeaderHasToken(header, vary, token) {
			t.Errorf("Vary %q does not contain %q; all values: %q", header.Values(vary), token, header.Values(vary))
		}
	}
}

// mwTestCompressResponse drives the post-handler middleware contract directly.
// stagedType and wireType are separate so the test can distinguish the
// serialization result's MIME type from the header actually sent to the client.
func mwTestCompressResponse(level int, stagedType, wireType string, body []byte, encoding string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	rw := &pooledResponseWriter{w: rec}
	req := httptest.NewRequest(http.MethodGet, "http://example.test/test", nil)
	req.Header.Set(acceptEncoding, encoding)
	c := &Context{
		Request:     req,
		writer:      rw,
		bytes:       body,
		contentType: stagedType,
		handled:     true,
	}
	if wireType != empty {
		rw.Header().Set(contentType, wireType)
	}
	Compress(level)(c)
	if bodyAllowedForStatus(rw.status()) {
		rw.writeHeader()
		_, _ = rw.Write(c.bytes)
	}
	if c.responseBuf != nil {
		releaseResponseBuffer(c.responseBuf)
		c.responseBuf = nil
	}
	return rec
}

func TestAcceptsGzip(t *testing.T) {
	tests := []struct {
		name   string
		header string
		want   bool
	}{
		{name: "absent", header: empty, want: false},
		{name: "gzip", header: "gzip", want: true},
		{name: "legacy x-gzip", header: "x-gzip", want: true},
		{name: "case and whitespace", header: " br ;q=0.2, GZip ; q=0.7 ", want: true},
		{name: "quality zero", header: "gzip;q=0", want: false},
		{name: "fractional zero", header: "gzip;q=0.000", want: false},
		{name: "quality parameter case", header: "gzip; Q = 0 ; version=1", want: false},
		{name: "positive quality", header: "gzip;q=0.001", want: true},
		{name: "unrelated coding", header: "br, deflate", want: false},
		{name: "wildcard", header: "*", want: true},
		{name: "weighted wildcard", header: "*;q=0.4", want: true},
		{name: "wildcard refused", header: "*;q=0", want: false},
		{name: "explicit refusal beats wildcard", header: "*;q=1, gzip;q=0", want: false},
		{name: "legacy refusal beats wildcard", header: "x-gzip;q=0, *;q=1", want: false},
		{name: "explicit acceptance beats wildcard refusal", header: "*;q=0, gzip;q=0.5", want: true},
		{name: "explicit refusal with other codings", header: "gzip;q=0, br;q=1", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := acceptsGzip(tt.header); got != tt.want {
				t.Fatalf("acceptsGzip(%q) = %v, want %v", tt.header, got, tt.want)
			}
		})
	}
}

func TestCompressQualityLevels(t *testing.T) {
	body := []byte(strings.Repeat("compressible payload\n", 80))
	valid := []struct {
		name  string
		level int
	}{
		{name: "store only", level: NoCompression},
		{name: "best speed", level: BestSpeed},
		{name: "huffman only", level: HuffmanOnly},
		{name: "default", level: DefaultCompression},
		{name: "best compression", level: BestCompression},
	}
	for _, tt := range valid {
		t.Run(tt.name, func(t *testing.T) {
			e := mwTestNewEngine(t, func() string { return string(body) }, Compress(tt.level))
			rec := mwTestGzipResponse(e, "gzip")
			mwTestRequireGzipBody(t, rec, body)
			mwTestRequireVary(t, rec.Header(), acceptEncoding)
			if got := rec.Header().Get("Content-Length"); got != empty {
				t.Errorf("Content-Length = %q, want it removed after compression", got)
			}
		})
	}

	for _, level := range []int{HuffmanOnly - 1, BestCompression + 1} {
		t.Run(fmt.Sprintf("invalid level %d", level), func(t *testing.T) {
			e := mwTestNewEngine(t, func() string { return string(body) }, Compress(level))
			rec := mwTestGzipResponse(e, "gzip")
			mwTestRequirePlainBody(t, rec, body)
		})
	}
}

func TestCompressNoCompressionKeepsGzipForLargerOutput(t *testing.T) {
	body := make([]byte, minCompressSize+128)
	var state uint32 = 1
	for i := range body {
		state = state*1664525 + 1013904223
		body[i] = byte(state >> 24)
	}
	e := mwTestNewEngine(t, func() string { return string(body) }, Compress(NoCompression))
	rec := mwTestGzipResponse(e, "gzip")
	mwTestRequireGzipBody(t, rec, body)
}

func TestCompressThreshold(t *testing.T) {
	for _, size := range []int{minCompressSize - 1, minCompressSize, minCompressSize + 1} {
		t.Run(fmt.Sprintf("body size %d", size), func(t *testing.T) {
			body := bytes.Repeat([]byte("x"), size)
			e := mwTestNewEngine(t, func() string { return string(body) }, Compress(DefaultCompression))
			rec := mwTestGzipResponse(e, "gzip")
			mwTestRequireVary(t, rec.Header(), acceptEncoding)
			if size < minCompressSize {
				mwTestRequirePlainBody(t, rec, body)
				return
			}
			mwTestRequireGzipBody(t, rec, body)
		})
	}
}

func TestCompressSkipsLargerOutput(t *testing.T) {
	body := make([]byte, 4096)
	state := uint32(1)
	for i := range body {
		state = state*1664525 + 1013904223
		body[i] = byte(state >> 24)
	}
	e := mwTestNewEngine(t, func() string { return string(body) }, Compress(DefaultCompression))
	rec := mwTestGzipResponse(e, "gzip")
	mwTestRequirePlainBody(t, rec, body)
	mwTestRequireVary(t, rec.Header(), acceptEncoding)
}

func TestCompressWeakensStrongETag(t *testing.T) {
	body := []byte(strings.Repeat("etag payload\n", 100))
	e := mwTestNewEngine(t, func(w http.ResponseWriter) string {
		w.Header().Set("Etag", `"strong"`)
		return string(body)
	}, Compress(DefaultCompression))
	rec := mwTestGzipResponse(e, "gzip")
	mwTestRequireGzipBody(t, rec, body)
	if got := rec.Header().Get("Etag"); got != `W/"strong"` {
		t.Fatalf("ETag = %q, want weak gzip variant", got)
	}
}

func TestCompressMIME(t *testing.T) {
	body := bytes.Repeat([]byte("mime payload"), 100)
	tests := []struct {
		mime string
		want bool
	}{
		{mime: empty, want: false},
		{mime: applicationJsonCharset, want: true},
		{mime: " Application/JSON ; charset=UTF-8", want: true},
		{mime: "application/xml", want: true},
		{mime: "text/html", want: true},
		{mime: textPlainCharset, want: true},
		{mime: "text/xml", want: true},
		{mime: "text/css", want: true},
		{mime: "text/javascript", want: true},
		{mime: "application/javascript", want: true},
		{mime: "application/x-javascript", want: true},
		{mime: "application/xhtml+xml", want: true},
		{mime: "application/rss+xml", want: true},
		{mime: "application/atom+xml", want: true},
		{mime: "application/svg+xml", want: true},
		{mime: "image/svg+xml", want: true},
		{mime: "text/custom; charset=utf-8", want: true},
		{mime: "image/png", want: false},
		{mime: "image/jpeg", want: false},
		{mime: "video/mp4", want: false},
		{mime: "audio/mpeg", want: false},
		{mime: "application/pdf", want: false},
		{mime: "application/zip", want: false},
		{mime: "application/octet-stream", want: false},
	}
	for _, tt := range tests {
		name := tt.mime
		if name == empty {
			name = "empty"
		}
		t.Run(name, func(t *testing.T) {
			rec := mwTestCompressResponse(DefaultCompression, tt.mime, tt.mime, body, "gzip")
			mwTestRequireVary(t, rec.Header(), acceptEncoding)
			if tt.want {
				mwTestRequireGzipBody(t, rec, body)
			} else {
				mwTestRequirePlainBody(t, rec, body)
			}
		})
	}
}

func TestCompressUsesActualResponseMIME(t *testing.T) {
	body := []byte(strings.Repeat("actual mime\n", 100))
	t.Run("binary wire header prevents compression", func(t *testing.T) {
		setBinary := func(c *Context) {
			c.Next()
			c.writer.Header().Set(contentType, "image/png")
		}
		e := mwTestNewEngine(t, func() string { return string(body) }, Compress(DefaultCompression), setBinary)
		rec := mwTestGzipResponse(e, "gzip")
		if got := rec.Header().Get(contentType); got != "image/png" {
			t.Fatalf("Content-Type = %q, want image/png", got)
		}
		mwTestRequirePlainBody(t, rec, body)
	})

	t.Run("text wire header permits compression", func(t *testing.T) {
		rec := mwTestCompressResponse(DefaultCompression, "image/png", "text/plain; charset=utf-8", body, "gzip")
		mwTestRequireGzipBody(t, rec, body)
	})
}

func TestCompressVary(t *testing.T) {
	body := []byte(strings.Repeat("vary", 400))
	t.Run("present for small response", func(t *testing.T) {
		e := mwTestNewEngine(t, func() string { return "small" }, Compress(DefaultCompression))
		rec := mwTestGzipResponse(e, "gzip")
		mwTestRequireVary(t, rec.Header(), acceptEncoding)
	})

	t.Run("present when gzip refused", func(t *testing.T) {
		e := mwTestNewEngine(t, func() string { return string(body) }, Compress(DefaultCompression))
		rec := mwTestGzipResponse(e, "gzip;q=0")
		mwTestRequireVary(t, rec.Header(), acceptEncoding)
		mwTestRequirePlainBody(t, rec, body)
	})

	t.Run("preserves existing values", func(t *testing.T) {
		addVary := func(c *Context) {
			c.Next()
			c.writer.Header().Add(vary, headerOrigin)
		}
		e := mwTestNewEngine(t, func() string { return string(body) }, Compress(DefaultCompression), addVary)
		rec := mwTestGzipResponse(e, "gzip")
		mwTestRequireVary(t, rec.Header(), acceptEncoding, headerOrigin)
	})

	t.Run("handler cannot overwrite it", func(t *testing.T) {
		e := mwTestNewEngine(t, func(w http.ResponseWriter) string {
			w.Header().Set(vary, headerOrigin)
			return string(body)
		}, Compress(DefaultCompression))
		rec := mwTestGzipResponse(e, "gzip")
		mwTestRequireVary(t, rec.Header(), acceptEncoding, headerOrigin)
	})
}

func TestCompressStackedMiddleware(t *testing.T) {
	body := []byte(strings.Repeat("stacked gzip\n", 100))
	e := mwTestNewEngine(t, func() string { return string(body) },
		Compress(DefaultCompression), Compress(BestSpeed))
	rec := mwTestGzipResponse(e, "gzip")
	mwTestRequireGzipBody(t, rec, body)
	if got := rec.Header().Values(contentEncoding); len(got) != 1 || got[0] != encodingGzip {
		t.Fatalf("Content-Encoding values = %q, want exactly [gzip]", got)
	}
	mwTestRequireVary(t, rec.Header(), acceptEncoding)
}

func TestCompressSkipsIneligibleResponses(t *testing.T) {
	body := []byte(strings.Repeat("ineligible", 200))
	for _, status := range []int{http.StatusNoContent, http.StatusResetContent, http.StatusNotModified} {
		t.Run(fmt.Sprintf("status %d", status), func(t *testing.T) {
			e := mwTestNewEngine(t, func(w http.ResponseWriter) string {
				w.WriteHeader(status)
				return string(body)
			}, Compress(DefaultCompression))
			rec := mwTestGzipResponse(e, "gzip")
			if rec.Code != status {
				t.Fatalf("status = %d, want %d", rec.Code, status)
			}
			if got := rec.Header().Get(contentEncoding); got != empty {
				t.Errorf("Content-Encoding = %q, want no encoding", got)
			}
			if rec.Body.Len() != 0 {
				t.Errorf("body has %d bytes, want none", rec.Body.Len())
			}
			mwTestRequireVary(t, rec.Header(), acceptEncoding)
		})
	}

	t.Run("already encoded", func(t *testing.T) {
		e := mwTestNewEngine(t, func(w http.ResponseWriter) string {
			w.Header().Set(contentEncoding, "br")
			return string(body)
		}, Compress(DefaultCompression))
		rec := mwTestGzipResponse(e, "gzip")
		if got := rec.Body.Bytes(); !bytes.Equal(got, body) {
			t.Fatalf("body differs: got %d bytes, want %d", len(got), len(body))
		}
		if got := rec.Header().Get(contentEncoding); got != "br" {
			t.Errorf("Content-Encoding = %q, want br", got)
		}
	})

	t.Run("direct write", func(t *testing.T) {
		e := mwTestNewEngine(t, func(w http.ResponseWriter) {
			_, _ = w.Write(body)
		}, Compress(DefaultCompression))
		rec := mwTestGzipResponse(e, "gzip")
		mwTestRequirePlainBody(t, rec, body)
	})

	t.Run("flushed response", func(t *testing.T) {
		e := mwTestNewEngine(t, func(w http.ResponseWriter) string {
			w.(http.Flusher).Flush()
			return string(body)
		}, Compress(DefaultCompression))
		rec := mwTestGzipResponse(e, "gzip")
		mwTestRequirePlainBody(t, rec, body)
	})

	t.Run("aborted handler", func(t *testing.T) {
		e := mwTestNewEngine(t, func(c *Context) { c.Abort() }, Compress(DefaultCompression))
		rec := mwTestGzipResponse(e, "gzip")
		if got := rec.Header().Get(contentEncoding); got != empty {
			t.Errorf("Content-Encoding = %q, want no encoding", got)
		}
		if rec.Body.Len() != 0 {
			t.Errorf("body has %d bytes, want none", rec.Body.Len())
		}
		mwTestRequireVary(t, rec.Header(), acceptEncoding)
	})
}

func TestCompressMultiLineAcceptEncoding(t *testing.T) {
	body := []byte(strings.Repeat("multi-line encoding\n", 80))
	tests := []struct {
		name      string
		encodings []string
		wantGzip  bool
	}{
		{name: "gzip on second line", encodings: []string{"br", "gzip"}, wantGzip: true},
		{name: "wildcard on second line", encodings: []string{"deflate", "*;q=0.5"}, wantGzip: true},
		{name: "refusal on second line", encodings: []string{"*;q=1", "gzip;q=0"}, wantGzip: false},
		{name: "explicit acceptance on first line", encodings: []string{"gzip;q=1", "*;q=0"}, wantGzip: true},
		{name: "explicit refusal on first line", encodings: []string{"gzip;q=0", "*;q=1"}, wantGzip: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := mwTestNewEngine(t, func() string { return string(body) }, Compress(DefaultCompression))
			rec := mwTestGzipResponse(e, tt.encodings...)
			mwTestRequireVary(t, rec.Header(), acceptEncoding)
			if tt.wantGzip {
				mwTestRequireGzipBody(t, rec, body)
			} else {
				mwTestRequirePlainBody(t, rec, body)
			}
		})
	}
}

func mwTestCORSConfig() *CorsConfig {
	return &CorsConfig{
		AllowedOrigins:   []string{"https://app.example.test", "https://admin.example.test"},
		AllowedMethods:   []string{"GET", "POST"},
		AllowedHeaders:   []string{"Content-Type", "X-Trace-Id"},
		ExposedHeaders:   []string{"X-Response-Id", "X-Rate-Limit"},
		AllowCredentials: true,
		MaxAge:           90 * time.Second,
	}
}

func mwTestCORSHandler(t *testing.T, config *CorsConfig) http.Handler {
	t.Helper()
	e := newEngine()
	t.Cleanup(e.cancelStreams)
	e.register(methodGet, "/test", func() string { return "ok" })
	e.register(methodPost, "/test", func() string { return "ok" })
	e.useCors(config)
	e.generateRoute()
	return e.buildHandler()
}

func mwTestCORSRequest(handler http.Handler, method, origin string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://example.test/test", nil)
	if origin != empty {
		req.Header.Set(headerOrigin, origin)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestCORSDeepCopy(t *testing.T) {
	config := &CorsConfig{
		AllowedOrigins:   []string{"https://one.example", "https://two.example"},
		AllowedMethods:   []string{" get ", "post"},
		AllowedHeaders:   []string{"Content-Type", "X-One"},
		ExposedHeaders:   []string{"X-Result", "X-Count"},
		AllowCredentials: false,
		MaxAge:           time.Minute,
	}
	e := newEngine()
	t.Cleanup(e.cancelStreams)
	e.register(methodGet, "/test", func() string { return "ok" })
	e.register(methodPost, "/test", func() string { return "ok" })
	e.useCors(config)
	owned := e.cors
	if owned == config {
		t.Fatal("engine retained the caller's CorsConfig pointer")
	}
	wantMethods := []string{"GET", "POST"}
	if !reflect.DeepEqual(owned.AllowedMethods, wantMethods) {
		t.Fatalf("installed methods = %q, want normalized %q", owned.AllowedMethods, wantMethods)
	}

	config.AllowedOrigins[0] = "https://attacker.example"
	config.AllowedMethods[0] = "DELETE"
	config.AllowedHeaders[0] = "X-Attacker"
	config.ExposedHeaders[0] = "X-Attacker"
	config.AllowCredentials = true
	config.MaxAge = 24 * time.Hour

	if got, want := owned.AllowedOrigins, []string{"https://one.example", "https://two.example"}; !reflect.DeepEqual(got, want) {
		t.Errorf("installed origins changed to %q, want %q", got, want)
	}
	if !reflect.DeepEqual(owned.AllowedMethods, wantMethods) {
		t.Errorf("installed methods changed to %q, want %q", owned.AllowedMethods, wantMethods)
	}
	if got, want := owned.AllowedHeaders, []string{"Content-Type", "X-One"}; !reflect.DeepEqual(got, want) {
		t.Errorf("installed headers changed to %q, want %q", got, want)
	}
	if got, want := owned.ExposedHeaders, []string{"X-Result", "X-Count"}; !reflect.DeepEqual(got, want) {
		t.Errorf("installed exposed headers changed to %q, want %q", got, want)
	}
	if owned.AllowCredentials {
		t.Error("installed AllowCredentials changed to true")
	}
	if owned.MaxAge != time.Minute {
		t.Errorf("installed MaxAge = %v, want 1m", owned.MaxAge)
	}

	e.generateRoute()
	handler := e.buildHandler()
	actual := mwTestCORSRequest(handler, http.MethodGet, "https://one.example", nil)
	if got := actual.Header().Get(accessControlAllowOrigin); got != "https://one.example" {
		t.Errorf("Allow-Origin = %q, want original allowed origin", got)
	}
	if got := actual.Header().Get(accessControlExposeHeaders); got != "X-Result, X-Count" {
		t.Errorf("Expose-Headers = %q, want original list", got)
	}
	if got := actual.Header().Get(accessControlAllowCredentials); got != empty {
		t.Errorf("Allow-Credentials = %q, want absent", got)
	}
	preflight := mwTestCORSRequest(handler, http.MethodOptions, "https://one.example", map[string]string{
		accessControlRequestMethod:  "POST",
		accessControlRequestHeaders: "content-type, x-one",
	})
	if preflight.Code != http.StatusNoContent {
		t.Errorf("preflight status = %d, want 204", preflight.Code)
	}
	if got := preflight.Header().Get(accessControlMaxAge); got != "60" {
		t.Errorf("Max-Age = %q, want 60", got)
	}
	attacker := mwTestCORSRequest(handler, http.MethodOptions, "https://attacker.example", map[string]string{
		accessControlRequestMethod: "POST",
	})
	if attacker.Code != http.StatusForbidden {
		t.Errorf("mutated origin status = %d, want 403", attacker.Code)
	}
}

func TestCORSActualRequests(t *testing.T) {
	t.Run("allowed credentialed origin", func(t *testing.T) {
		handler := mwTestCORSHandler(t, mwTestCORSConfig())
		origin := "https://APP.example.test"
		rec := mwTestCORSRequest(handler, http.MethodGet, origin, nil)
		if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
			t.Fatalf("response = %d %q, want 200 ok", rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get(accessControlAllowOrigin); got != origin {
			t.Errorf("Allow-Origin = %q, want echoed %q", got, origin)
		}
		if got := rec.Header().Get(accessControlAllowCredentials); got != headerTrue {
			t.Errorf("Allow-Credentials = %q, want true", got)
		}
		if got := rec.Header().Get(accessControlExposeHeaders); got != "X-Response-Id, X-Rate-Limit" {
			t.Errorf("Expose-Headers = %q", got)
		}
		mwTestRequireVary(t, rec.Header(), headerOrigin)
	})

	t.Run("disallowed simple request passes through", func(t *testing.T) {
		handler := mwTestCORSHandler(t, mwTestCORSConfig())
		rec := mwTestCORSRequest(handler, http.MethodGet, "https://other.example", nil)
		if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
			t.Fatalf("response = %d %q, want 200 ok", rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get(accessControlAllowOrigin); got != empty {
			t.Errorf("Allow-Origin = %q, want absent", got)
		}
		if got := rec.Header().Get(accessControlAllowCredentials); got != empty {
			t.Errorf("Allow-Credentials = %q, want absent", got)
		}
		mwTestRequireVary(t, rec.Header(), headerOrigin)
	})

	t.Run("simple request method is enforced", func(t *testing.T) {
		config := mwTestCORSConfig()
		config.AllowedMethods = []string{http.MethodGet}
		handler := mwTestCORSHandler(t, config)
		rec := mwTestCORSRequest(handler, http.MethodPost, "https://APP.example.test", nil)
		if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
			t.Fatalf("response = %d %q, want 200 ok", rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get(accessControlAllowOrigin); got != empty {
			t.Errorf("Allow-Origin = %q, want absent for disallowed method", got)
		}
	})

	t.Run("same-origin request has no allow headers", func(t *testing.T) {
		handler := mwTestCORSHandler(t, mwTestCORSConfig())
		rec := mwTestCORSRequest(handler, http.MethodGet, empty, nil)
		if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
			t.Fatalf("response = %d %q, want 200 ok", rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get(accessControlAllowOrigin); got != empty {
			t.Errorf("Allow-Origin = %q, want absent", got)
		}
		mwTestRequireVary(t, rec.Header(), headerOrigin)
	})

	t.Run("wildcard without credentials", func(t *testing.T) {
		config := mwTestCORSConfig()
		config.AllowedOrigins = []string{asterisk}
		config.AllowCredentials = false
		handler := mwTestCORSHandler(t, config)
		rec := mwTestCORSRequest(handler, http.MethodGet, "https://any.example", nil)
		if got := rec.Header().Get(accessControlAllowOrigin); got != asterisk {
			t.Errorf("Allow-Origin = %q, want *", got)
		}
		if got := rec.Header().Get(accessControlAllowCredentials); got != empty {
			t.Errorf("Allow-Credentials = %q, want absent", got)
		}
		if mwTestHeaderHasToken(rec.Header(), vary, headerOrigin) {
			t.Errorf("wildcard response unnecessarily varies on Origin: %q", rec.Header().Values(vary))
		}
	})
}

func TestCORSResponseWriterUnwrap(t *testing.T) {
	sink := httptest.NewRecorder()
	writer := &corsResponseWriter{ResponseWriter: sink}
	if writer.Unwrap() != sink {
		t.Fatal("corsResponseWriter.Unwrap did not return the underlying writer")
	}
}

func TestCORSVarySurvivesHandlerOverride(t *testing.T) {
	for _, mode := range []string{"write", "flush"} {
		t.Run(mode, func(t *testing.T) {
			e := newEngine()
			t.Cleanup(e.cancelStreams)
			e.use(func(c *Context) {
				c.writer.Header().Set(vary, acceptEncoding)
				if mode == "flush" {
					_ = http.NewResponseController(c.writer).Flush()
				}
				c.Next()
			})
			e.register(methodGet, "/test", func() string { return "ok" })
			e.useCors(mwTestCORSConfig())
			e.generateRoute()

			rec := mwTestCORSRequest(e.buildHandler(), http.MethodGet, "https://app.example.test", nil)
			mwTestRequireVary(t, rec.Header(), headerOrigin, acceptEncoding)
		})
	}
}

func TestCORSPreflightAllowLists(t *testing.T) {
	for _, size := range []int{2, corsLinearScanLimit + 1} {
		t.Run(fmt.Sprintf("list size %d", size), func(t *testing.T) {
			origins := make([]string, size)
			methods := make([]string, size)
			headers := make([]string, size)
			for i := range size {
				origins[i] = fmt.Sprintf("https://origin-%d.example", i)
				methods[i] = fmt.Sprintf("METHOD-%d", i)
				headers[i] = fmt.Sprintf("X-Header-%d", i)
			}
			methods[0], methods[1] = "GET", "POST"
			headers[0], headers[1] = "Content-Type", "X-Trace-Id"
			config := &CorsConfig{
				AllowedOrigins: origins,
				AllowedMethods: methods,
				AllowedHeaders: headers,
				MaxAge:         1500 * time.Millisecond,
			}
			handler := mwTestCORSHandler(t, config)
			origin := origins[size-1]
			method := strings.ToLower(methods[size-1])
			header := strings.ToLower(headers[size-1])
			rec := mwTestCORSRequest(handler, http.MethodOptions, origin, map[string]string{
				accessControlRequestMethod:  method,
				accessControlRequestHeaders: " " + header + " , " + headers[0] + " ",
			})
			if rec.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want 204; body: %q", rec.Code, rec.Body.String())
			}
			if rec.Body.Len() != 0 {
				t.Errorf("preflight body = %q, want empty", rec.Body.String())
			}
			if got := rec.Header().Get(accessControlAllowOrigin); got != origin {
				t.Errorf("Allow-Origin = %q, want %q", got, origin)
			}
			if got, want := rec.Header().Get(accessControlAllowMethods), strings.Join(methods, ", "); got != want {
				t.Errorf("Allow-Methods = %q, want %q", got, want)
			}
			if got, want := rec.Header().Get(accessControlAllowHeaders), strings.Join(headers, ", "); got != want {
				t.Errorf("Allow-Headers = %q, want %q", got, want)
			}
			if got := rec.Header().Get(accessControlMaxAge); got != "1" {
				t.Errorf("Max-Age = %q, want truncated seconds 1", got)
			}
			mwTestRequireVary(t, rec.Header(), headerOrigin, accessControlRequestMethod, accessControlRequestHeaders)
		})
	}
}

func TestCORSPreflightRejections(t *testing.T) {
	tests := []struct {
		name       string
		origin     string
		method     string
		headers    string
		wantStatus int
		wantBody   string
		wantOrigin string
	}{
		{
			name: "origin denied", origin: "https://other.example", method: "POST",
			wantStatus: http.StatusForbidden, wantBody: corsOriginNotAllowed + "\n",
		},
		{
			name: "method denied", origin: "https://app.example.test", method: "DELETE",
			wantStatus: http.StatusForbidden, wantBody: corsMethodNotAllowed + "\n", wantOrigin: "https://app.example.test",
		},
		{
			name: "header denied", origin: "https://app.example.test", method: "POST", headers: "Content-Type, X-Forbidden",
			wantStatus: http.StatusForbidden, wantBody: corsHeaderNotAllowed + "\n", wantOrigin: "https://app.example.test",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := mwTestCORSHandler(t, mwTestCORSConfig())
			rec := mwTestCORSRequest(handler, http.MethodOptions, tt.origin, map[string]string{
				accessControlRequestMethod:  tt.method,
				accessControlRequestHeaders: tt.headers,
			})
			if rec.Code != tt.wantStatus || rec.Body.String() != tt.wantBody {
				t.Fatalf("response = %d %q, want %d %q", rec.Code, rec.Body.String(), tt.wantStatus, tt.wantBody)
			}
			if got := rec.Header().Get(accessControlAllowOrigin); got != tt.wantOrigin {
				t.Errorf("Allow-Origin = %q, want %q", got, tt.wantOrigin)
			}
			if got := rec.Header().Get(accessControlAllowMethods); got != empty {
				t.Errorf("Allow-Methods = %q on rejection, want absent", got)
			}
			if got := rec.Header().Get(accessControlAllowHeaders); got != empty {
				t.Errorf("Allow-Headers = %q on rejection, want absent", got)
			}
			mwTestRequireVary(t, rec.Header(), headerOrigin, accessControlRequestMethod, accessControlRequestHeaders)
		})
	}

	t.Run("too many requested headers", func(t *testing.T) {
		allowed := make([]string, corsMaxPreflightHeaders)
		requested := make([]string, corsMaxPreflightHeaders+1)
		for i := range allowed {
			allowed[i] = fmt.Sprintf("X-Header-%d", i)
		}
		for i := range requested {
			requested[i] = fmt.Sprintf("X-Header-%d", i)
		}
		config := mwTestCORSConfig()
		config.AllowedHeaders = allowed
		handler := mwTestCORSHandler(t, config)
		rec := mwTestCORSRequest(handler, http.MethodOptions, "https://app.example.test", map[string]string{
			accessControlRequestMethod:  "POST",
			accessControlRequestHeaders: strings.Join(requested, ","),
		})
		if rec.Code != http.StatusForbidden || rec.Body.String() != corsHeaderNotAllowed+"\n" {
			t.Fatalf("response = %d %q, want 403 header rejection", rec.Code, rec.Body.String())
		}
		mwTestRequireVary(t, rec.Header(), headerOrigin, accessControlRequestMethod, accessControlRequestHeaders)
	})
}

func TestContainsFold(t *testing.T) {
	values := []string{"GET", "Post", "X-Trace-ID"}
	tests := []struct {
		target string
		want   bool
	}{
		{target: "get", want: true},
		{target: "POST", want: true},
		{target: "x-trace-id", want: true},
		{target: "GETTING", want: false},
		{target: " GET", want: false},
		{target: empty, want: false},
	}
	for _, tt := range tests {
		if got := containsFold(values, tt.target); got != tt.want {
			t.Errorf("containsFold(%q) = %v, want %v", tt.target, got, tt.want)
		}
	}
	if containsFold(nil, "GET") {
		t.Error("containsFold(nil, GET) = true, want false")
	}
}

func TestContainsInMap(t *testing.T) {
	long := strings.Repeat("A", 300)
	values := map[string]struct{}{
		"get": {}, "post": {}, "x-trace-id": {}, "äbc": {}, strings.ToLower(long): {},
	}
	tests := []struct {
		target string
		want   bool
	}{
		{target: "GET", want: true},
		{target: "Post", want: true},
		{target: "X-TRACE-ID", want: true},
		{target: "ÄBC", want: true},
		{target: long, want: true},
		{target: "GETTING", want: false},
		{target: " GET", want: false},
		{target: empty, want: false},
	}
	for _, tt := range tests {
		if got := containsInMap(values, tt.target); got != tt.want {
			t.Errorf("containsInMap(%q) = %v, want %v", tt.target, got, tt.want)
		}
	}
	if containsInMap(nil, "GET") {
		t.Error("containsInMap(nil, GET) = true, want false")
	}
}
