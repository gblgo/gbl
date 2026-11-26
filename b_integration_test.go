package gbl

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type integrationTestUser struct {
	Name string `json:"name"`
}

func (u *integrationTestUser) Validate() error {
	if u.Name == "" {
		return errors.New("name required")
	}
	return nil
}

type integrationTestResponse struct {
	ID        int    `json:"id"`
	Name      string `json:"name"`
	Payload   string `json:"payload"`
	RequestID string `json:"request_id"`
}

type integrationTestUploadResponse struct {
	Field    string `json:"field"`
	Filename string `json:"filename"`
	Content  string `json:"content"`
}

type integrationTestTrace struct {
	mu    sync.Mutex
	steps []string
}

func (t *integrationTestTrace) add(step string) {
	t.mu.Lock()
	t.steps = append(t.steps, step)
	t.mu.Unlock()
}

func (t *integrationTestTrace) snapshot() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.steps...)
}

type integrationTestLogger struct {
	mu    sync.Mutex
	lines []string
}

func (l *integrationTestLogger) Info(msg string, kv ...any) {
	l.append("INFO", msg, kv)
}

func (l *integrationTestLogger) Warn(msg string, kv ...any) {
	l.append("WARN", msg, kv)
}

func (l *integrationTestLogger) Error(msg string, kv ...any) {
	l.append("ERROR", msg, kv)
}

func (l *integrationTestLogger) append(level, msg string, kv []any) {
	l.mu.Lock()
	l.lines = append(l.lines, logLine(level, msg, kv))
	l.mu.Unlock()
}

func (l *integrationTestLogger) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.lines...)
}

func integrationTestServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

func integrationTestClient(t *testing.T, server *httptest.Server) *http.Client {
	t.Helper()
	client := server.Client()
	client.Timeout = 5 * time.Second
	t.Cleanup(client.CloseIdleConnections)
	return client
}

func integrationTestReadBody(t *testing.T, response *http.Response) []byte {
	t.Helper()
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return body
}

func TestIntegrationFullMiddlewareBindingResponseFlow(t *testing.T) {
	e := newEngine()
	t.Cleanup(e.cancelStreams)
	trace := &integrationTestTrace{}

	e.use(func(c *Context) {
		trace.add("global-before")
		c.Set("request-id", "req-integration")
		c.writer.Header().Set("X-Request-ID", "req-integration")
		c.Next()
		trace.add("global-after")
	})
	e.use(Compress(DefaultCompression))
	e.useCors(&CorsConfig{
		AllowedOrigins: []string{"https://example.com"},
		AllowedMethods: []string{http.MethodGet, http.MethodPost, http.MethodOptions},
		AllowedHeaders: []string{contentType, "X-Request-ID"},
	})

	group := e.group("/api")
	group.GroupUse(func(c *Context) {
		trace.add("group-before")
		c.Next()
		trace.add("group-after")
	})
	group.Post("/users/:id", func(id int, user integrationTestUser, c *Context) integrationTestResponse {
		trace.add("handler")
		return integrationTestResponse{
			ID:        id,
			Name:      user.Name,
			Payload:   strings.Repeat("payload", 300),
			RequestID: c.Value[string]("request-id"),
		}
	})
	e.generateRoute()

	server := integrationTestServer(t, e.buildHandler())
	client := integrationTestClient(t, server)
	request, err := http.NewRequest(http.MethodPost, server.URL+"/api/users/42",
		strings.NewReader(`{"user":{"name":"alice"}}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set(contentType, applicationJson)
	request.Header.Set(headerOrigin, "https://example.com")
	request.Header.Set(acceptEncoding, encodingGzip)

	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("POST integrated route: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %q", response.StatusCode,
			integrationTestReadBody(t, response))
	}
	if got := response.Header.Get(accessControlAllowOrigin); got != "https://example.com" {
		t.Errorf("Access-Control-Allow-Origin = %q", got)
	}
	if got := response.Header.Get("X-Request-ID"); got != "req-integration" {
		t.Errorf("X-Request-ID = %q", got)
	}
	if got := response.Header.Get(contentEncoding); got != encodingGzip {
		t.Errorf("Content-Encoding = %q, want gzip", got)
	}
	if got := strings.Join(response.Header.Values(vary), ","); !strings.Contains(got, acceptEncoding) {
		t.Errorf("Vary = %q, want Accept-Encoding", got)
	}

	reader, err := gzip.NewReader(response.Body)
	if err != nil {
		t.Fatalf("open gzip response: %v", err)
	}
	defer reader.Close()
	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read gzip response: %v", err)
	}
	var got integrationTestResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode integrated response: %v", err)
	}
	if got.ID != 42 || got.Name != "alice" || got.RequestID != "req-integration" ||
		len(got.Payload) != len("payload")*300 {
		t.Fatalf("integrated response = %#v", got)
	}

	wantTrace := []string{
		"global-before", "group-before", "handler", "group-after", "global-after",
	}
	if trace := trace.snapshot(); !reflectDeepEqualStrings(trace, wantTrace) {
		t.Errorf("middleware trace = %v, want %v", trace, wantTrace)
	}
}

func reflectDeepEqualStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestIntegrationErrorValidationPanicAndRoutingFlow(t *testing.T) {
	e := newEngine()
	t.Cleanup(e.cancelStreams)
	logger := &integrationTestLogger{}
	e.setLogger(logger)
	e.register(methodGet, "/resource", func() string { return "resource" })
	e.register(methodPost, "/validate", func(user integrationTestUser) string { return user.Name })
	e.register(methodGet, "/panic", func() string { panic("integration secret") })
	e.generateRoute()

	server := integrationTestServer(t, e)
	client := integrationTestClient(t, server)

	t.Run("method not allowed", func(t *testing.T) {
		request, err := http.NewRequest(http.MethodPut, server.URL+"/resource", nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		body := integrationTestReadBody(t, response)
		if response.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, body = %q", response.StatusCode, body)
		}
		allow := response.Header.Get(allow)
		for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
			if !strings.Contains(allow, method) {
				t.Errorf("Allow = %q, want %s", allow, method)
			}
		}
	})

	t.Run("automatic options", func(t *testing.T) {
		request, err := http.NewRequest(http.MethodOptions, server.URL+"/resource", nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		body := integrationTestReadBody(t, response)
		if response.StatusCode != http.StatusNoContent || len(body) != 0 {
			t.Fatalf("OPTIONS response = %d %q", response.StatusCode, body)
		}
		if allow := response.Header.Get(allow); !strings.Contains(allow, http.MethodOptions) {
			t.Errorf("Allow = %q, want OPTIONS", allow)
		}
	})

	t.Run("not found", func(t *testing.T) {
		response, err := client.Get(server.URL + "/missing")
		if err != nil {
			t.Fatal(err)
		}
		body := integrationTestReadBody(t, response)
		if response.StatusCode != http.StatusNotFound || string(body) != http.StatusText(http.StatusNotFound) {
			t.Fatalf("GET missing = %d %q", response.StatusCode, body)
		}
	})

	t.Run("validation failure", func(t *testing.T) {
		request, err := http.NewRequest(http.MethodPost, server.URL+"/validate",
			strings.NewReader(`{"name":""}`))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set(contentType, applicationJson)
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		body := integrationTestReadBody(t, response)
		if response.StatusCode != http.StatusBadRequest || string(body) != "name required" {
			t.Fatalf("validation response = %d %q", response.StatusCode, body)
		}
	})

	t.Run("panic is generic and logged", func(t *testing.T) {
		response, err := client.Get(server.URL + "/panic")
		if err != nil {
			t.Fatal(err)
		}
		body := integrationTestReadBody(t, response)
		if response.StatusCode != http.StatusInternalServerError ||
			string(body) != http.StatusText(http.StatusInternalServerError) {
			t.Fatalf("panic response = %d %q", response.StatusCode, body)
		}
		if strings.Contains(string(body), "integration secret") {
			t.Fatal("panic response leaked the internal panic value")
		}
		logs := strings.Join(logger.snapshot(), "\n")
		if !strings.Contains(logs, "recovered panic") || !strings.Contains(logs, "integration secret") {
			t.Errorf("panic log = %q", logs)
		}
	})
}

func TestIntegrationHeadFallbackFlow(t *testing.T) {
	e := newEngine()
	t.Cleanup(e.cancelStreams)
	e.register(methodGet, "/head", func() string { return "head-body" })
	e.generateRoute()
	server := integrationTestServer(t, e)
	client := integrationTestClient(t, server)

	request, err := http.NewRequest(http.MethodHead, server.URL+"/head", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body := integrationTestReadBody(t, response)
	if response.StatusCode != http.StatusOK || len(body) != 0 {
		t.Fatalf("HEAD response = %d %q", response.StatusCode, body)
	}
	if got := response.Header.Get(contentType); got != textPlainCharset {
		t.Errorf("HEAD Content-Type = %q, want %q", got, textPlainCharset)
	}
}

func TestIntegrationMultipartUploadFlow(t *testing.T) {
	e := newEngine()
	t.Cleanup(e.cancelStreams)
	e.register(methodPost, "/upload", func(name string, file *multipart.FileHeader) integrationTestUploadResponse {
		reader, err := file.Open()
		if err != nil {
			panic(err)
		}
		defer reader.Close()
		content, err := io.ReadAll(reader)
		if err != nil {
			panic(err)
		}
		return integrationTestUploadResponse{Field: name, Filename: file.Filename, Content: string(content)}
	})
	e.generateRoute()
	server := integrationTestServer(t, e)
	client := integrationTestClient(t, server)

	var payload bytes.Buffer
	writer := multipart.NewWriter(&payload)
	if err := writer.WriteField("name", "avatar"); err != nil {
		t.Fatal(err)
	}
	part, err := writer.CreateFormFile("file", "avatar.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("integration-upload")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	request, err := http.NewRequest(http.MethodPost, server.URL+"/upload", &payload)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set(contentType, writer.FormDataContentType())
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body := integrationTestReadBody(t, response)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("upload response = %d %q", response.StatusCode, body)
	}
	var got integrationTestUploadResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Field != "avatar" || got.Filename != "avatar.txt" || got.Content != "integration-upload" {
		t.Fatalf("upload result = %#v", got)
	}
}

func TestIntegrationSSEFlow(t *testing.T) {
	e := newEngine()
	t.Cleanup(e.cancelStreams)
	completed := make(chan struct{}, 1)
	failed := make(chan error, 1)
	e.register(methodGet, "/events", func(stream SSEWriter) {
		stream.OnComplete(func() { completed <- struct{}{} })
		stream.OnError(func(err error) { failed <- err })
		if err := stream.Send("one"); err != nil {
			return
		}
		_ = stream.Send("two")
	})
	e.generateRoute()
	server := integrationTestServer(t, e)
	client := integrationTestClient(t, server)

	response, err := client.Get(server.URL + "/events")
	if err != nil {
		t.Fatal(err)
	}
	body := integrationTestReadBody(t, response)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("SSE status = %d, body = %q", response.StatusCode, body)
	}
	if got := response.Header.Get(contentType); got != sseContentType {
		t.Errorf("SSE Content-Type = %q, want %q", got, sseContentType)
	}
	if got, want := string(body), "data: one\n\ndata: two\n\n"; got != want {
		t.Errorf("SSE body = %q, want %q", got, want)
	}
	select {
	case <-completed:
	case <-time.After(2 * time.Second):
		t.Fatal("SSE OnComplete callback did not run")
	}
	select {
	case err := <-failed:
		t.Fatalf("SSE OnError callback ran: %v", err)
	default:
	}
}

func TestIntegrationRawRequestBodyLimitFlow(t *testing.T) {
	oldLimit := maxBodySize.Load()
	maxBodySize.Store(32)
	t.Cleanup(func() { maxBodySize.Store(oldLimit) })

	e := newEngine()
	t.Cleanup(e.cancelStreams)
	e.register(methodPost, "/raw", func(request *http.Request) string {
		body, _ := io.ReadAll(request.Body)
		return fmt.Sprintf("read=%d", len(body))
	})
	e.generateRoute()
	server := integrationTestServer(t, e)
	client := integrationTestClient(t, server)

	request, err := http.NewRequest(http.MethodPost, server.URL+"/raw",
		strings.NewReader(strings.Repeat("x", 64)))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set(contentType, "application/octet-stream")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body := integrationTestReadBody(t, response)
	if response.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("raw oversize response = %d %q, want 413", response.StatusCode, body)
	}
}
