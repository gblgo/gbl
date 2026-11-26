// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var logTestBenchmarkSink string

func BenchmarkLogLine(b *testing.B) {
	kv := []any{
		"method", "GET",
		"path", "/users/:id",
		"status", 200,
		"duration", 15 * time.Millisecond,
		"cached", false,
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		logTestBenchmarkSink = logLine("INFO", "request completed", kv)
	}
}

func BenchmarkAppendLogValue(b *testing.B) {
	values := []struct {
		name  string
		value any
	}{
		{name: "string", value: "/users/:id"},
		{name: "int", value: 200},
		{name: "duration", value: 15 * time.Millisecond},
		{name: "error", value: errors.New("request failed")},
		{name: "stringer", value: logTestStringer{text: "string value"}},
		{name: "nil", value: nil},
	}

	for _, tt := range values {
		b.Run(tt.name, func(b *testing.B) {
			var line strings.Builder
			line.Grow(128)
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				line.Reset()
				appendLogValue(&line, tt.value)
				logTestBenchmarkSink = line.String()
			}
		})
	}
}
