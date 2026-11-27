package gbl

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func integrationBenchEngine(b *testing.B) *engine {
	b.Helper()
	e := newEngine()
	b.Cleanup(e.cancelStreams)
	e.use(func(c *Context) {
		c.Set("request-id", "benchmark")
		c.Next()
	})
	e.use(Compress(DefaultCompression))
	e.register(methodGet, "/static", func() string { return "static-response" })
	e.register(methodPost, "/json", func(user integrationTestUser) integrationTestResponse {
		return integrationTestResponse{ID: 1, Name: user.Name, RequestID: "benchmark"}
	})
	e.generateRoute()
	return e
}

// frameworkBenchBody resets benchmark input without allocating a reader per request.
type frameworkBenchBody struct {
	reader *strings.Reader
}

// Read implements io.Reader for the reusable request body.
func (b *frameworkBenchBody) Read(p []byte) (int, error) { return b.reader.Read(p) }

// Close implements io.Closer without releasing the reusable reader.
func (b *frameworkBenchBody) Close() error { return nil }

// frameworkBenchWriter excludes net/http transport and response parsing from measurements.
type frameworkBenchWriter struct {
	header http.Header
	status int
	size   int
}

// Header returns the reusable response header map.
func (w *frameworkBenchWriter) Header() http.Header { return w.header }

// WriteHeader records the first final status.
func (w *frameworkBenchWriter) WriteHeader(status int) {
	if w.status == 0 && status >= 200 {
		w.status = status
	}
}

// Write counts response bytes without allocating a transport buffer.
func (w *frameworkBenchWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	w.size += len(p)
	return len(p), nil
}

// benchmarkFrameworkJSONPipeline measures gbl handling with reusable input and output.
func benchmarkFrameworkJSONPipeline(b *testing.B, e *engine) {
	payload := `{"name":"alice"}`
	body := &frameworkBenchBody{reader: strings.NewReader(payload)}
	request := httptest.NewRequest(http.MethodPost, "/json", body)
	request.ContentLength = int64(len(payload))
	request.Header.Set(contentType, applicationJson)
	writer := &frameworkBenchWriter{header: make(http.Header, 8)}

	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		body.reader.Reset(payload)
		clear(writer.header)
		writer.status = 0
		writer.size = 0
		e.ServeHTTP(writer, request)
		if writer.status != http.StatusOK || writer.size == 0 {
			b.Fatalf("framework response = %d, %d bytes", writer.status, writer.size)
		}
	}
}

// BenchmarkFrameworkJSONPipeline separates core dispatch from optional middleware costs.
func BenchmarkFrameworkJSONPipeline(b *testing.B) {
	b.Run("core", func(b *testing.B) {
		e := newEngine()
		b.Cleanup(e.cancelStreams)
		e.register(methodPost, "/json", func(user integrationTestUser) integrationTestResponse {
			return integrationTestResponse{ID: 1, Name: user.Name, RequestID: "benchmark"}
		})
		e.generateRoute()
		benchmarkFrameworkJSONPipeline(b, e)
	})
	b.Run("with-compress", func(b *testing.B) {
		benchmarkFrameworkJSONPipeline(b, integrationBenchEngine(b))
	})
}
