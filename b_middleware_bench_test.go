// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"fmt"
	"net/http"
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

// BenchmarkFrameworkAcceptEncodingValues measures repeated-header scanning without joining.
func BenchmarkFrameworkAcceptEncodingValues(b *testing.B) {
	values := []string{"br", "gzip;q=0.8, deflate;q=0.5"}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		middlewareBenchContains = acceptsGzipValues(values)
	}
	b.StopTimer()
	if !middlewareBenchContains {
		b.Fatal("gzip header values were rejected")
	}
}

// BenchmarkFrameworkCompressHeaders measures framework negotiation and Vary handling only.
func BenchmarkFrameworkCompressHeaders(b *testing.B) {
	header := make(http.Header, 8)
	header.Set(contentType, textPlainCharset)
	accept := "gzip, deflate;q=0.5"
	var accepted, compressible bool
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		header.Del(vary)
		ensureVaryAcceptEncoding(header)
		accepted = acceptsGzip(accept)
		compressible = isCompressible(header.Get(contentType))
	}
	b.StopTimer()
	if !accepted || !compressible || header.Get(vary) != acceptEncoding {
		b.Fatalf("unexpected compression headers: accept=%t compressible=%t vary=%q",
			accepted, compressible, header.Get(vary))
	}
}
