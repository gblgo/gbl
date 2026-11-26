// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"bytes"
	"net/http"
	"reflect"
	"testing"
)

type responseTestBenchWriter struct {
	header http.Header
}

func responseTestNewBenchWriter() *responseTestBenchWriter {
	return &responseTestBenchWriter{header: make(http.Header)}
}

func (w *responseTestBenchWriter) Header() http.Header { return w.header }
func (w *responseTestBenchWriter) WriteHeader(int)     {}
func (w *responseTestBenchWriter) Write(p []byte) (int, error) {
	return len(p), nil
}
func (w *responseTestBenchWriter) Flush() {}

var (
	responseTestBenchMediaType string
	responseTestBenchErr       error
	responseTestBenchN         int
	responseTestBenchStatus    int
)

func BenchmarkResponseScalarSerialization(b *testing.B) {
	value := reflect.ValueOf("responseTest scalar payload")
	buf := new(bytes.Buffer)
	b.ReportAllocs()
	b.SetBytes(int64(value.Len()))
	b.ResetTimer()
	for range b.N {
		buf.Reset()
		responseTestBenchMediaType, responseTestBenchErr = convertToBuffer(buf, value)
	}
}

func BenchmarkResponseJSONSerialization(b *testing.B) {
	value := reflect.ValueOf(responseTestPayload{Name: "responseTest", Count: 42})
	buf := new(bytes.Buffer)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		buf.Reset()
		responseTestBenchMediaType, responseTestBenchErr = convertToBuffer(buf, value)
	}
}

func BenchmarkResponseWriterStatus(b *testing.B) {
	rw := &pooledResponseWriter{w: responseTestNewBenchWriter(), headedStatus: true, statusCode: http.StatusCreated}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		responseTestBenchStatus = rw.status()
	}
}

func BenchmarkResponseWriterWriteHeader(b *testing.B) {
	sink := responseTestNewBenchWriter()
	rw := &pooledResponseWriter{}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		*rw = pooledResponseWriter{w: sink}
		rw.WriteHeader(http.StatusCreated)
	}
}

func BenchmarkResponseWriterWrite(b *testing.B) {
	sink := responseTestNewBenchWriter()
	rw := &pooledResponseWriter{}
	payload := []byte("responseTest writer payload")
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for range b.N {
		*rw = pooledResponseWriter{w: sink}
		responseTestBenchN, responseTestBenchErr = rw.Write(payload)
	}
}

func BenchmarkResponseWriterFlush(b *testing.B) {
	sink := responseTestNewBenchWriter()
	rw := &pooledResponseWriter{}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		*rw = pooledResponseWriter{w: sink, headedStatus: true, statusCode: http.StatusCreated}
		rw.Flush()
	}
}

func BenchmarkResponseWriterReplaceStatus(b *testing.B) {
	sink := responseTestNewBenchWriter()
	rw := &pooledResponseWriter{}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		*rw = pooledResponseWriter{w: sink, headedStatus: true, statusCode: http.StatusCreated}
		rw.replaceStatus(http.StatusInternalServerError)
	}
}
