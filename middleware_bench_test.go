// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func BenchmarkContainsFold(b *testing.B) {
	values := []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS", "TRACE"}
	for _, target := range []string{"post", "CONNECT"} {
		b.Run(target, func(b *testing.B) {
			want := target == "post"
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if got := containsFold(values, target); got != want {
					b.Fatalf("containsFold(%q) = %v, want %v", target, got, want)
				}
			}
		})
	}
}

func BenchmarkContainsInMap(b *testing.B) {
	values := make(map[string]struct{}, corsLinearScanLimit+1)
	for _, method := range []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS", "TRACE", "CONNECT"} {
		values[strings.ToLower(method)] = struct{}{}
	}
	for _, target := range []string{"post", "PROPFIND"} {
		b.Run(target, func(b *testing.B) {
			_, want := values[strings.ToLower(target)]
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if got := containsInMap(values, target); got != want {
					b.Fatalf("containsInMap(%q) = %v, want %v", target, got, want)
				}
			}
		})
	}
}

func BenchmarkCompressGzip(b *testing.B) {
	body := strings.Repeat("compressible benchmark payload\n", 80)
	levels := []struct {
		name  string
		level int
	}{
		{name: "best_speed", level: BestSpeed},
		{name: "default", level: DefaultCompression},
		{name: "best_compression", level: BestCompression},
	}
	for _, tt := range levels {
		b.Run(tt.name, func(b *testing.B) {
			e := newEngine()
			b.Cleanup(e.cancelStreams)
			e.use(Compress(tt.level))
			e.register(methodGet, "/test", func() string { return body })
			e.generateRoute()
			req := httptest.NewRequest(http.MethodGet, "http://example.test/test", nil)
			req.Header.Set(acceptEncoding, encodingGzip)
			rec := httptest.NewRecorder()
			b.ReportAllocs()
			b.SetBytes(int64(len(body)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				rec = httptest.NewRecorder()
				e.ServeHTTP(rec, req)
			}
			b.StopTimer()
			if got := rec.Header().Get(contentEncoding); got != encodingGzip {
				b.Fatalf("Content-Encoding = %q, want gzip", got)
			}
			if rec.Body.Len() == 0 {
				b.Fatal("compressed response body is empty")
			}
			b.ReportMetric(float64(rec.Body.Len()), "compressed-bytes/op")
		})
	}
}
