// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestMiddlewareEdgeCompressPoolReuse(t *testing.T) {
	body := bytes.Repeat([]byte("pooled gzip writer\n"), 100)
	e := mwTestNewEngine(t, func() string { return string(body) }, Compress(BestSpeed))
	var writers []*gzip.Writer
	coverageSeamReplace(t, &gzipWriteAndClose, func(w io.WriteCloser, data []byte) (error, error) {
		writer, ok := w.(*gzip.Writer)
		if !ok {
			t.Fatalf("gzip sink = %T, want *gzip.Writer", w)
		}
		writers = append(writers, writer)
		return writeGzipAndClose(w, data)
	})

	for request := 0; request < 3; request++ {
		rec := mwTestGzipResponse(e, "gzip")
		mwTestRequireGzipBody(t, rec, body)
		if got := rec.Header().Values(vary); len(got) != 1 || got[0] != acceptEncoding {
			t.Fatalf("request %d Vary = %q, want exactly [Accept-Encoding]", request, got)
		}
	}
	if len(writers) != 3 {
		t.Fatalf("gzip seam calls = %d, want 3", len(writers))
	}
	reused := false
	for i, writer := range writers {
		for _, previous := range writers[:i] {
			if writer == previous {
				reused = true
			}
		}
	}
	if !reused {
		t.Fatalf("gzip writer identities = %p, %p, %p, want at least one reuse",
			writers[0], writers[1], writers[2])
	}
}

func TestMiddlewareEdgeCompressWriterFailures(t *testing.T) {
	writeFailure := errors.New("injected gzip write failure")
	closeFailure := errors.New("injected gzip close failure")
	tests := []struct {
		name     string
		writeErr error
		closeErr error
	}{
		{name: "write failure", writeErr: writeFailure},
		{name: "close failure", closeErr: closeFailure},
		{name: "both failures", writeErr: writeFailure, closeErr: closeFailure},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := bytes.Repeat([]byte("compressible failure payload"), 100)
			calls := 0
			coverageSeamReplace(t, &gzipWriteAndClose, func(w io.WriteCloser, data []byte) (error, error) {
				if _, ok := w.(*gzip.Writer); !ok {
					t.Fatalf("gzip sink = %T, want *gzip.Writer", w)
				}
				calls++
				if !bytes.Equal(data, body) {
					t.Fatalf("gzip input differs: got %d bytes, want %d", len(data), len(body))
				}
				return tt.writeErr, tt.closeErr
			})
			e := mwTestNewEngine(t, func() string { return string(body) }, Compress(DefaultCompression))
			recorder := mwTestGzipResponse(e, encodingGzip)
			if calls != 1 {
				t.Fatalf("gzip seam calls = %d, want 1", calls)
			}
			mwTestRequirePlainBody(t, recorder, body)
			if got := recorder.Header().Values(vary); len(got) != 1 || got[0] != acceptEncoding {
				t.Fatalf("Vary = %q, want exactly [Accept-Encoding]", got)
			}
		})
	}
}

func TestMiddlewareEdgeCORSCredentialsWildcardRejected(t *testing.T) {
	for _, origin := range []string{"*", " * ", "\t*\r\n"} {
		t.Run("validation "+origin, func(t *testing.T) {
			config := &CorsConfig{
				AllowedOrigins:   []string{origin},
				AllowCredentials: true,
			}
			err := config.validate()
			if err == nil || !strings.Contains(err.Error(), "AllowCredentials") {
				t.Fatalf("validate() error = %v, want credentials/wildcard rejection", err)
			}
		})
	}

	e := newEngine()
	t.Cleanup(e.cancelStreams)
	capture := &logTestCaptureLogger{}
	e.setLogger(capture)
	recovered := logTestPanicValue(func() {
		e.useCors(&CorsConfig{AllowedOrigins: []string{"*"}, AllowCredentials: true})
	})
	err, ok := recovered.(error)
	if !ok || !strings.Contains(err.Error(), "AllowCredentials") {
		t.Fatalf("useCors panic = %#v, want credentials/wildcard error", recovered)
	}
	if e.cors != nil {
		t.Fatal("rejected CORS configuration was installed")
	}
	if len(capture.records) != 1 || capture.records[0].msg != "startup aborted" {
		t.Fatalf("startup records = %#v, want one startup abort", capture.records)
	}

	// Bypass validation only to exercise matchOrigin's defensive credentials
	// guard. Public configuration cannot construct this unsafe combination.
	handler := newCorsHandler(e, &CorsConfig{
		AllowedOrigins:   []string{"*"},
		AllowCredentials: true,
	})
	if origin, allowed := handler.matchOrigin("https://any.example"); allowed || origin != empty {
		t.Fatalf("defensive wildcard match = (%q, %t), want rejected", origin, allowed)
	}
}

func TestMiddlewareEdgeCORSCaseAndTrim(t *testing.T) {
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
			origins[0] = "HTTPS://APP.EXAMPLE.TEST"
			methods[0], methods[1] = " get ", " pOsT "
			headers[0], headers[1] = "Content-Type", "X-TRACE-ID"
			config := &CorsConfig{
				AllowedOrigins: origins,
				AllowedMethods: methods,
				AllowedHeaders: headers,
				MaxAge:         2500 * time.Millisecond,
			}
			handler := mwTestCORSHandler(t, config)
			rec := mwTestCORSRequest(handler, http.MethodOptions, "  https://app.example.test  ", map[string]string{
				accessControlRequestMethod:  "post",
				accessControlRequestHeaders: " x-trace-id , , CONTENT-TYPE , ",
			})
			if rec.Code != http.StatusNoContent {
				t.Fatalf("preflight status = %d, want 204; body: %q", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get(accessControlAllowOrigin); got != "https://app.example.test" {
				t.Errorf("Allow-Origin = %q, want the trimmed request origin", got)
			}
			wantMethods := append([]string{"GET", "POST"}, methods[2:]...)
			if got := rec.Header().Get(accessControlAllowMethods); got != strings.Join(wantMethods, ", ") {
				t.Errorf("Allow-Methods = %q, want %q", got, strings.Join(wantMethods, ", "))
			}
			if got := rec.Header().Get(accessControlAllowHeaders); got != strings.Join(headers, ", ") {
				t.Errorf("Allow-Headers = %q, want %q", got, strings.Join(headers, ", "))
			}
			if got := rec.Header().Get(accessControlMaxAge); got != "2" {
				t.Errorf("Max-Age = %q, want 2", got)
			}
			mwTestRequireVary(t, rec.Header(), headerOrigin, accessControlRequestMethod, accessControlRequestHeaders)
		})
	}
}

func TestMiddlewareEdgeCORSNilConfigAndBlankOrigin(t *testing.T) {
	t.Run("nil public config", func(t *testing.T) {
		previous := defaultEngine
		e := newEngine()
		defaultEngine = e
		t.Cleanup(func() {
			e.cancelStreams()
			defaultEngine = previous
		})
		SetCorsConfig(nil)
		if e.cors != nil {
			t.Fatal("SetCorsConfig(nil) installed a CORS policy")
		}
	})

	t.Run("nil config leaves engine unwrapped", func(t *testing.T) {
		e := newEngine()
		t.Cleanup(e.cancelStreams)
		e.register(methodGet, "/test", func() string { return "ok" })
		e.useCors(nil)
		e.generateRoute()
		handler := e.buildHandler()
		if handler != e {
			t.Fatalf("buildHandler() = %T, want the unwrapped engine", handler)
		}
		rec := mwTestCORSRequest(handler, http.MethodGet, "https://any.example", nil)
		if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
			t.Fatalf("response = %d %q, want 200 ok", rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get(accessControlAllowOrigin); got != empty {
			t.Errorf("Allow-Origin = %q with CORS disabled", got)
		}
	})

	t.Run("whitespace-only origin is non-cross-origin", func(t *testing.T) {
		handler := mwTestCORSHandler(t, mwTestCORSConfig())
		rec := mwTestCORSRequest(handler, http.MethodGet, " \t ", nil)
		if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
			t.Fatalf("response = %d %q, want 200 ok", rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get(accessControlAllowOrigin); got != empty {
			t.Errorf("Allow-Origin = %q for blank origin", got)
		}
		mwTestRequireVary(t, rec.Header(), headerOrigin)
	})
}

func TestMiddlewareEdgeCORSMaxAgeBoundaries(t *testing.T) {
	tests := []struct {
		name   string
		maxAge time.Duration
		want   string
	}{
		{name: "zero", maxAge: 0, want: empty},
		{name: "negative", maxAge: -time.Second, want: empty},
		{name: "positive subsecond truncates to zero", maxAge: 999 * time.Millisecond, want: "0"},
		{name: "one second", maxAge: time.Second, want: "1"},
		{name: "fractional seconds truncate", maxAge: 1999 * time.Millisecond, want: "1"},
		{name: "large duration", maxAge: 2 * time.Hour, want: "7200"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := mwTestCORSHandler(t, &CorsConfig{
				AllowedOrigins: []string{"https://app.example.test"},
				AllowedMethods: []string{http.MethodGet},
				MaxAge:         tt.maxAge,
			})
			rec := mwTestCORSRequest(handler, http.MethodOptions, "https://app.example.test", map[string]string{
				accessControlRequestMethod: http.MethodGet,
			})
			if rec.Code != http.StatusNoContent {
				t.Fatalf("preflight status = %d, want 204", rec.Code)
			}
			if got := rec.Header().Get(accessControlMaxAge); got != tt.want {
				t.Fatalf("Max-Age = %q, want %q", got, tt.want)
			}
		})
	}
}
