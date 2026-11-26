package gbl

import (
	"io"
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

func BenchmarkIntegrationStaticHTTPFlow(b *testing.B) {
	e := integrationBenchEngine(b)
	server := httptest.NewServer(e.buildHandler())
	b.Cleanup(server.Close)
	client := server.Client()
	b.Cleanup(client.CloseIdleConnections)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		response, err := client.Get(server.URL + "/static")
		if err != nil {
			b.Fatalf("GET static: %v", err)
		}
		if _, err := io.Copy(io.Discard, response.Body); err != nil {
			_ = response.Body.Close()
			b.Fatalf("read static response: %v", err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK {
			b.Fatalf("static status = %d", response.StatusCode)
		}
	}
}

func BenchmarkIntegrationJSONHTTPFlow(b *testing.B) {
	e := integrationBenchEngine(b)
	server := httptest.NewServer(e.buildHandler())
	b.Cleanup(server.Close)
	client := server.Client()
	b.Cleanup(client.CloseIdleConnections)
	payload := `{"name":"alice"}`
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		request, err := http.NewRequest(http.MethodPost, server.URL+"/json", strings.NewReader(payload))
		if err != nil {
			b.Fatalf("build JSON request: %v", err)
		}
		request.Header.Set(contentType, applicationJson)
		response, err := client.Do(request)
		if err != nil {
			b.Fatalf("POST JSON: %v", err)
		}
		if _, err := io.Copy(io.Discard, response.Body); err != nil {
			_ = response.Body.Close()
			b.Fatalf("read JSON response: %v", err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusOK {
			b.Fatalf("JSON status = %d", response.StatusCode)
		}
	}
}
