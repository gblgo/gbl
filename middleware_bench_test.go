// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// middlewareBenchContains keeps benchmark lookup results observable.
var middlewareBenchContains bool

// BenchmarkCORSContains times scan and map lookups separately against the same
// allow-list, target, and last-element hit position. Sizes straddle the production
// scan threshold; even size 8 is mapped here for comparison. Each iteration
// performs exactly one lookup.
func BenchmarkCORSContains(b *testing.B) {
	for _, size := range []int{8, 9, 64} {
		values := make([]string, size)
		for i := range values {
			values[i] = fmt.Sprintf("X-ALLOWED-%02d", i)
		}
		allowedMap := make(map[string]struct{}, len(values))
		for _, value := range values {
			allowedMap[strings.ToLower(value)] = struct{}{}
		}
		targets := []struct {
			name  string
			value string
			want  bool
		}{
			{name: "hit", value: values[size-1], want: true},
			{name: "miss", value: "X-MISSING", want: false},
		}
		for _, target := range targets {
			b.Run(fmt.Sprintf("size=%d/%s/scan", size, target.name), func(b *testing.B) {
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					middlewareBenchContains = containsFold(values, target.value)
				}
				b.StopTimer()
				if middlewareBenchContains != target.want {
					b.Fatalf("containsFold(%q) = %v, want %v", target.value,
						middlewareBenchContains, target.want)
				}
			})
			b.Run(fmt.Sprintf("size=%d/%s/map", size, target.name), func(b *testing.B) {
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					middlewareBenchContains = containsInMap(allowedMap, target.value)
				}
				b.StopTimer()
				if middlewareBenchContains != target.want {
					b.Fatalf("containsInMap(%q) = %v, want %v", target.value,
						middlewareBenchContains, target.want)
				}
			})
		}
	}
}

// BenchmarkCompressGzip measures complete compressed responses at each level.
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
