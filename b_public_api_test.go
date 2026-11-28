package gbl

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type publicTestLogger struct {
	mu    sync.Mutex
	lines []string
}

func (l *publicTestLogger) Info(msg string, kv ...any) {
	l.append("INFO", msg, kv)
}

func (l *publicTestLogger) Warn(msg string, kv ...any) {
	l.append("WARN", msg, kv)
}

func (l *publicTestLogger) Error(msg string, kv ...any) {
	l.append("ERROR", msg, kv)
}

func (l *publicTestLogger) append(level, msg string, kv []any) {
	l.mu.Lock()
	l.lines = append(l.lines, logLine(level, msg, kv))
	l.mu.Unlock()
}

func publicTestUseDefaultEngine(t *testing.T) *engine {
	t.Helper()
	previous := defaultEngine
	e := newEngine()
	defaultEngine = e
	t.Cleanup(func() {
		e.cancelStreams()
		defaultEngine = previous
	})
	return e
}

func TestPublicRegistrationConfigurationAndMiddleware(t *testing.T) {
	e := publicTestUseDefaultEngine(t)
	logger := &publicTestLogger{}
	oldBodyLimit := maxBodySize.Load()
	oldFileLimit := maxFileMemory.Load()
	oldUploadLimit := maxUploadFileSize.Load()
	t.Cleanup(func() {
		maxBodySize.Store(oldBodyLimit)
		maxFileMemory.Store(oldFileLimit)
		maxUploadFileSize.Store(oldUploadLimit)
	})

	SetLogger(logger)
	Configure(func(config *Config) {
		config.Port = "8080"
		config.ReadTimeout = 2 * time.Second
	})
	SetCorsConfig(&CorsConfig{
		AllowedOrigins: []string{"https://example.com"},
		AllowedMethods: []string{http.MethodGet},
	})
	SetMaxBodySize(1024)
	SetMaxFileSize(512)
	SetMaxUploadFileSize(256)
	Use(func(c *Context) {
		c.writer.Header().Set("X-Public-Middleware", "yes")
		c.Next()
	})
	Get("/public/:id", func(id int) string { return strconv.Itoa(id) })
	Group("/group").Get("/value", func() string { return "group" })
	RegisterPanicProcessor(func(any) (any, HttpStatus) {
		return "public panic handled", HttpStatus(http.StatusTeapot)
	})
	Get("/panic", func() { panic("public panic") })
	e.generateRoute()

	if e.cfg.Port != "8080" || e.cfg.ReadTimeout != 2*time.Second {
		t.Fatalf("public Configure did not update engine: %#v", e.cfg)
	}
	if maxBodySize.Load() != 1024 || maxFileMemory.Load() != 512 || maxUploadFileSize.Load() != 256 {
		t.Fatalf("public size setters did not update limits: %d %d %d",
			maxBodySize.Load(), maxFileMemory.Load(), maxUploadFileSize.Load())
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/public/42", nil)
	request.Header.Set(headerOrigin, "https://example.com")
	e.buildHandler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || recorder.Body.String() != "42" {
		t.Fatalf("public GET response = %d %q", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("X-Public-Middleware"); got != "yes" {
		t.Errorf("public middleware header = %q", got)
	}
	if got := recorder.Header().Get(accessControlAllowOrigin); got != "https://example.com" {
		t.Errorf("public CORS origin = %q", got)
	}

	recorder = httptest.NewRecorder()
	e.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/group/value", nil))
	if recorder.Code != http.StatusOK || recorder.Body.String() != "group" {
		t.Fatalf("public Group response = %d %q", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	e.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/panic", nil))
	if recorder.Code != http.StatusTeapot || recorder.Body.String() != "public panic handled" {
		t.Fatalf("public panic response = %d %q", recorder.Code, recorder.Body.String())
	}
	if loggerLines := len(logger.lines); loggerLines != 0 {
		t.Errorf("PanicProcessor path logged %d unexpected lines", loggerLines)
	}
}

func TestPublicRunAndShutdown(t *testing.T) {
	e := publicTestUseDefaultEngine(t)
	listener := routeTestHoldListener(t)
	routeTestInjectListener(t, listener)
	Configure(func(config *Config) { config.Port = routeTestPort(listener.Addr().String()) })
	Get("/public-run", func() string { return "public-run" })

	type runResult struct {
		panicValue any
	}
	runDone := make(chan runResult, 1)
	runExited := make(chan struct{})
	go func() {
		defer close(runExited)
		var result runResult
		defer func() {
			result.panicValue = recover()
			runDone <- result
		}()
		Run()
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = e.shutdown(ctx)
		e.startMu.Lock()
		server := e.server
		e.startMu.Unlock()
		if server != nil {
			_ = server.Close()
		}
		_ = listener.Close()
		e.hijacks.closeAll()
		routeTestWaitDone(t, runExited, "public Run cleanup")
	})

	client := routeTestHTTPClient(t)
	deadline := time.Now().Add(3 * time.Second)
	for {
		response, err := client.Get("http://" + listener.Addr().String() + "/public-run")
		if err == nil {
			body := integrationTestReadBody(t, response)
			if response.StatusCode != http.StatusOK || string(body) != "public-run" {
				t.Fatalf("public Run response = %d %q", response.StatusCode, body)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("public Run did not start: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := Shutdown(ctx); err != nil {
		t.Fatalf("public Shutdown: %v", err)
	}
	select {
	case result := <-runDone:
		if result.panicValue != nil {
			t.Fatalf("public Run panicked: %v", result.panicValue)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("public Run did not return after Shutdown")
	}
	if err := Shutdown(context.Background()); err != nil {
		t.Fatalf("repeated public Shutdown: %v", err)
	}
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Shutdown(canceledCtx); err != nil {
		t.Fatalf("Shutdown after completion with canceled context: %v", err)
	}
}

func TestPublicShutdownBeforeRun(t *testing.T) {
	publicTestUseDefaultEngine(t)
	if err := Shutdown(context.Background()); !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("Shutdown before Run = %v, want http.ErrServerClosed", err)
	}
}

func TestPublicHTTPVerbRegistrations(t *testing.T) {
	e := publicTestUseDefaultEngine(t)
	e.logger = routeTestDiscardLogger{}
	tests := []struct {
		name     string
		method   httpMethod
		path     string
		register func(string, any)
	}{
		{"HEAD", methodHead, "/public-head", Head},
		{"POST", methodPost, "/public-post", Post},
		{"PUT", methodPut, "/public-put", Put},
		{"PATCH", methodPatch, "/public-patch", Patch},
		{"DELETE", methodDelete, "/public-delete", Delete},
		{"OPTIONS", methodOptions, "/public-options", Options},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.register(tt.path, func() string { return tt.name })
		})
	}
	if len(e.routeMetaSlice) != len(tests) {
		t.Fatalf("queued public registrations = %d, want %d", len(e.routeMetaSlice), len(tests))
	}
	for i, tt := range tests {
		meta := e.routeMetaSlice[i]
		if meta.method != tt.method || meta.path != tt.path {
			t.Errorf("registration %d = (%d, %q), want (%d, %q)", i, meta.method, meta.path, tt.method, tt.path)
		}
	}
	e.generateRoute()
	for _, tt := range tests {
		recorder := routeTestServe(e, methodNames[tt.method], tt.path)
		if recorder.Code != http.StatusOK || recorder.Body.String() != tt.name {
			t.Errorf("%s response = (%d, %q), want (200, %q)", tt.name, recorder.Code, recorder.Body.String(), tt.name)
		}
	}
}

func TestPublicSizeSettersRejectNonPositiveValues(t *testing.T) {
	e := publicTestUseDefaultEngine(t)
	e.logger = routeTestDiscardLogger{}
	oldBodyLimit := maxBodySize.Load()
	oldFileLimit := maxFileMemory.Load()
	oldUploadLimit := maxUploadFileSize.Load()
	t.Cleanup(func() {
		maxBodySize.Store(oldBodyLimit)
		maxFileMemory.Store(oldFileLimit)
		maxUploadFileSize.Store(oldUploadLimit)
	})

	tests := []struct {
		name    string
		set     func(ByteSize)
		current func() int64
		message string
	}{
		{"body", SetMaxBodySize, func() int64 { return maxBodySize.Load() }, "max body size must be greater than zero"},
		{"file", SetMaxFileSize, func() int64 { return maxFileMemory.Load() }, "multipart memory threshold must be greater than zero"},
		{"upload", SetMaxUploadFileSize, func() int64 { return maxUploadFileSize.Load() }, "max upload file size must be greater than zero"},
	}
	for _, tt := range tests {
		for _, size := range []ByteSize{0, -1} {
			t.Run(tt.name+"/"+strconv.Itoa(int(size)), func(t *testing.T) {
				before := tt.current()
				panicValue := routeTestRequirePanic(t, func() { tt.set(size) })
				if !strings.Contains(fmt.Sprint(panicValue), tt.message) {
					t.Fatalf("setter panic = %v, want %q", panicValue, tt.message)
				}
				if after := tt.current(); after != before {
					t.Fatalf("rejected size %d changed limit from %d to %d", size, before, after)
				}
			})
		}
	}
}

func TestPublicNilConfigurationAndPanicProcessor(t *testing.T) {
	e := publicTestUseDefaultEngine(t)
	Configure(func(config *Config) {
		config.Port = "8123"
		config.ReadTimeout = 17 * time.Second
		config.CertFile = "cert.pem"
		config.KeyFile = "key.pem"
	})
	before := *e.cfg
	Configure(nil)
	if *e.cfg != before {
		t.Fatalf("Configure(nil) changed config from %#v to %#v", before, *e.cfg)
	}

	RegisterPanicProcessor(func(any) (any, HttpStatus) {
		return "temporary", HttpStatus(http.StatusTeapot)
	})
	RegisterPanicProcessor(nil)
	if e.panicProcessor != nil {
		t.Fatal("RegisterPanicProcessor(nil) did not clear the processor")
	}
}

func TestPublicAssemblingAPIsFreezeAfterRunClaim(t *testing.T) {
	e := publicTestUseDefaultEngine(t)
	e.logger = routeTestDiscardLogger{}
	oldBodyLimit := maxBodySize.Load()
	oldFileLimit := maxFileMemory.Load()
	oldUploadLimit := maxUploadFileSize.Load()
	t.Cleanup(func() {
		maxBodySize.Store(oldBodyLimit)
		maxFileMemory.Store(oldFileLimit)
		maxUploadFileSize.Store(oldUploadLimit)
	})

	Use(func(*Context) {})
	group := Group("/frozen")
	group.GroupUse(func(*Context) {})
	RegisterPanicProcessor(func(any) (any, HttpStatus) {
		return "original", HttpStatus(http.StatusTeapot)
	})
	Get("/frozen", func() string { return "ok" })
	e.generateRoute()
	e.runState.Store(runStateRunning)
	beforeConfig := e.cfg
	beforeMiddleware := len(e.middlewares)
	beforeGroupMiddleware := len(group.(*routeGroup).middlewares)

	tests := []struct {
		name string
		call func()
		want string
	}{
		{"Use", func() { Use(func(*Context) {}) }, "gbl.Use"},
		{"GroupUse", func() { group.GroupUse(func(*Context) {}) }, "gbl.GroupUse"},
		{"Configure nil", func() { Configure(nil) }, "gbl.Configure"},
		{"panic processor", func() {
			RegisterPanicProcessor(func(any) (any, HttpStatus) { return "replacement", HttpStatus(http.StatusOK) })
		}, "gbl.RegisterPanicProcessor"},
		{"nil panic processor", func() { RegisterPanicProcessor(nil) }, "gbl.RegisterPanicProcessor"},
		{"body limit", func() { SetMaxBodySize(2048) }, "gbl.SetMaxBodySize"},
		{"file limit", func() { SetMaxFileSize(1024) }, "gbl.SetMaxFileSize"},
		{"upload limit", func() { SetMaxUploadFileSize(512) }, "gbl.SetMaxUploadFileSize"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			panicValue := routeTestRequirePanic(t, tt.call)
			want := tt.want + " must be called before Run; the engine is read-only while serving"
			if panicValue != want {
				t.Fatalf("freeze panic = %#v, want %q", panicValue, want)
			}
		})
	}
	if e.cfg != beforeConfig {
		t.Fatal("rejected Configure replaced the frozen configuration")
	}
	if len(e.middlewares) != beforeMiddleware || len(group.(*routeGroup).middlewares) != beforeGroupMiddleware {
		t.Fatal("rejected middleware registration changed a frozen chain")
	}
	if maxBodySize.Load() != oldBodyLimit || maxFileMemory.Load() != oldFileLimit || maxUploadFileSize.Load() != oldUploadLimit {
		t.Fatal("rejected size setter changed a frozen limit")
	}
	body, status := e.panicProcessor("panic")
	if body != "original" || status != HttpStatus(http.StatusTeapot) {
		t.Fatalf("frozen panic processor = (%v, %d), want (original, 418)", body, status)
	}
}

func TestContextPathParamAndLookupString(t *testing.T) {
	values := map[string]string{"id": "42"}
	if got, ok := lookupString(values, "id"); !ok || got != "42" {
		t.Fatalf("lookupString = %q, %v", got, ok)
	}
	if got, ok := lookupString(values, "missing"); ok || got != "" {
		t.Fatalf("lookupString missing = %q, %v", got, ok)
	}
	c := &Context{params: values}
	if got := c.PathParam("id"); got != "42" {
		t.Fatalf("PathParam = %q, want 42", got)
	}
	if got := c.PathParam("missing"); got != "" {
		t.Fatalf("PathParam missing = %q, want empty", got)
	}
}
