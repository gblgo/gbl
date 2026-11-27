// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const routeTestLifecycleTimeout = 5 * time.Second

type routeTestStartedServer struct {
	engine     *engine
	address    string
	done       chan struct{}
	panicValue any
}

type routeTestHTTPResult struct {
	status int
	body   string
	err    error
}

type routeTestRunResult struct {
	index      int
	panicValue any
}

// routeTestHoldListener reserves a loopback listener without releasing it before
// Run serves on it, so another process cannot claim the port in the meantime.
func routeTestHoldListener(t *testing.T) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve test listener: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return listener
}

// routeTestInjectListener makes Run serve on an already-held listener. The
// returned channel closes when Run reaches the netListen seam.
func routeTestInjectListener(t *testing.T, listener net.Listener) <-chan struct{} {
	t.Helper()
	called := make(chan struct{})
	original := netListen
	netListen = func(network, address string) (net.Listener, error) {
		close(called)
		if network != "tcp" || address != ":"+routeTestPort(listener.Addr().String()) {
			return nil, fmt.Errorf("unexpected Listen(%q, %q) for held listener", network, address)
		}
		return listener, nil
	}
	t.Cleanup(func() { netListen = original })
	return called
}

func routeTestPort(address string) string {
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		panic(err)
	}
	return port
}

func routeTestAddressReady(address string) bool {
	conn, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func routeTestWaitDone(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(routeTestLifecycleTimeout):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func routeTestHTTPClient(t *testing.T) *http.Client {
	t.Helper()
	transport := &http.Transport{DisableKeepAlives: true}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{Transport: transport, Timeout: 3 * time.Second}
}

func routeTestStartServer(t *testing.T, e *engine, listener net.Listener) *routeTestStartedServer {
	t.Helper()
	address := listener.Addr().String()
	e.cfg.Port = routeTestPort(address)
	routeTestInjectListener(t, listener)
	server := &routeTestStartedServer{engine: e, address: address, done: make(chan struct{})}
	go func() {
		defer close(server.done)
		server.panicValue = routeTestCapturePanic(e.run)
	}()

	t.Cleanup(func() {
		deadline := time.NewTimer(routeTestLifecycleTimeout)
		defer deadline.Stop()
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			_ = e.shutdown(ctx)
			cancel()
			e.startMu.Lock()
			live := e.server
			e.startMu.Unlock()
			if live != nil {
				_ = live.Close()
			}
			_ = listener.Close()
			e.hijacks.closeAll()

			select {
			case <-server.done:
				return
			case <-ticker.C:
			case <-deadline.C:
				t.Errorf("Run goroutine did not exit during cleanup")
				return
			}
		}
	})

	deadline := time.NewTimer(routeTestLifecycleTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-server.done:
			t.Fatalf("Run exited before serving: %v", server.panicValue)
		case <-deadline.C:
			t.Fatalf("server at %s did not start", address)
		case <-ticker.C:
			e.startMu.Lock()
			live := e.server
			e.startMu.Unlock()
			if live != nil && routeTestAddressReady(address) {
				return server
			}
		}
	}
}

func routeTestWriteTLSKeyPair(t *testing.T) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate TLS key: %v", err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create TLS certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal TLS key: %v", err)
	}
	dir := t.TempDir()
	certPath := dir + string(os.PathSeparator) + "cert.pem"
	keyPath := dir + string(os.PathSeparator) + "key.pem"
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write TLS certificate: %v", err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatalf("write TLS key: %v", err)
	}
	return certPath, keyPath
}

func TestLifecycleShutdownBeforeRun(t *testing.T) {
	e := routeTestNewEngine(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := e.shutdown(ctx); !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("Shutdown before Run = %v, want http.ErrServerClosed", err)
	}
	if e.shutdownDone.Load() {
		t.Fatal("Shutdown before Run latched shutdownDone")
	}
	e.startMu.Lock()
	server := e.server
	e.startMu.Unlock()
	if server != nil {
		t.Fatal("Shutdown before Run published a server")
	}
}

func routeTestRequireFailedStartupState(t *testing.T, e *engine) {
	t.Helper()
	if got := e.runState.Load(); got != runStateStopped {
		t.Errorf("failed Run state = %d, want runStateStopped", got)
	}
	select {
	case <-e.startDone:
	default:
		t.Fatal("failed Run did not release startup waiters")
	}
	e.startMu.Lock()
	server := e.server
	e.startMu.Unlock()
	if server != nil {
		_ = server.Close()
		t.Fatal("failed Run published a server")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := e.shutdown(ctx); !errors.Is(err, http.ErrServerClosed) {
		t.Errorf("Shutdown after failed Run = %v, want http.ErrServerClosed", err)
	}
	if e.shutdownDone.Load() {
		t.Fatal("Shutdown after failed Run latched shutdownDone")
	}
}

func TestLifecycleAssemblingRejectedAfterRunClaim(t *testing.T) {
	e := routeTestNewEngine(t)
	e.runState.Store(runStateStarting)
	panicValue := routeTestRequirePanic(t, func() { e.assertAssembling("Configure") })
	if panicValue != "gbl.Configure must be called before Run; the engine is read-only while serving" {
		t.Fatalf("assertAssembling panic = %#v", panicValue)
	}
}

func TestLifecycleShutdownDuringStartupHonorsContext(t *testing.T) {
	tests := []struct {
		name       string
		newContext func() (context.Context, context.CancelFunc)
		want       error
	}{
		{
			name: "canceled",
			newContext: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx, cancel
			},
			want: context.Canceled,
		},
		{
			name: "deadline exceeded",
			newContext: func() (context.Context, context.CancelFunc) {
				return context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			},
			want: context.DeadlineExceeded,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := routeTestNewEngine(t)
			// Model the interval after Run claims the engine but before it publishes
			// a server. The real listener/publication race is covered separately by
			// TestLifecycleShutdownDuringStartupIsNotLost.
			e.runState.Store(runStateStarting)
			ctx, cancel := tt.newContext()
			defer cancel()
			if err := e.shutdown(ctx); !errors.Is(err, tt.want) {
				t.Fatalf("Shutdown during startup = %v, want %v", err, tt.want)
			}
			if got := e.runState.Load(); got != runStateStarting {
				t.Errorf("canceled Shutdown changed Run state to %d", got)
			}
			select {
			case <-e.startDone:
				t.Fatal("canceled Shutdown closed the unpublished server channel")
			default:
			}
			if e.shutdownDone.Load() {
				t.Fatal("canceled Shutdown latched shutdownDone")
			}
		})
	}
}

func TestLifecycleRunOnlyOnce(t *testing.T) {
	e := routeTestNewEngine(t)
	listener := routeTestHoldListener(t)
	server := routeTestStartServer(t, e, listener)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := e.shutdown(ctx); err != nil {
		t.Fatalf("first Shutdown: %v", err)
	}
	routeTestWaitDone(t, server.done, "Run after Shutdown")
	if server.panicValue != nil {
		t.Fatalf("Run panicked during normal shutdown: %v", server.panicValue)
	}

	panicValue := routeTestRequirePanic(t, e.run)
	if !strings.Contains(fmt.Sprint(panicValue), "called twice") {
		t.Fatalf("second Run panic = %v, want single-use diagnostic", panicValue)
	}
}

func TestLifecycleConcurrentRunRejectsAllButOne(t *testing.T) {
	oldProcs := runtime.GOMAXPROCS(8)
	defer runtime.GOMAXPROCS(oldProcs)

	e := routeTestNewEngine(t)
	listener := routeTestHoldListener(t)
	routeTestInjectListener(t, listener)
	certPath, keyPath := routeTestWriteTLSKeyPair(t)
	e.configure(func(cfg *Config) {
		cfg.Port = routeTestPort(listener.Addr().String())
		cfg.CertFile = certPath
		cfg.KeyFile = keyPath
	})

	const callers = 32
	start := make(chan struct{})
	results := make(chan routeTestRunResult, callers)
	var workers sync.WaitGroup
	workers.Add(callers)
	allDone := make(chan struct{})
	go func() {
		workers.Wait()
		close(allDone)
	}()
	for i := range callers {
		go func() {
			defer workers.Done()
			<-start
			results <- routeTestRunResult{index: i, panicValue: routeTestCapturePanic(e.run)}
		}()
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = e.shutdown(ctx)
		cancel()
		e.startMu.Lock()
		server := e.server
		e.startMu.Unlock()
		if server != nil {
			_ = server.Close()
		}
		_ = listener.Close()
		e.hijacks.closeAll()
		routeTestWaitDone(t, allDone, "concurrent Run callers")
	})
	close(start)

	deadline := time.NewTimer(routeTestLifecycleTimeout)
	defer deadline.Stop()
	for range callers - 1 {
		select {
		case result := <-results:
			if result.panicValue == nil {
				t.Errorf("Run caller %d returned normally before Shutdown", result.index)
				continue
			}
			if message := fmt.Sprint(result.panicValue); !strings.Contains(message, "called twice") {
				t.Errorf("Run caller %d panic = %v, want single-use diagnostic", result.index, result.panicValue)
			}
		case <-deadline.C:
			t.Fatal("timed out waiting for rejected concurrent Run callers")
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := e.shutdown(ctx); err != nil {
		t.Errorf("Shutdown after concurrent Run calls: %v", err)
	}
	routeTestWaitDone(t, allDone, "all concurrent Run callers")
	select {
	case result := <-results:
		if result.panicValue != nil {
			t.Errorf("surviving Run caller %d panicked: %v", result.index, result.panicValue)
		}
	case <-time.After(routeTestLifecycleTimeout):
		t.Fatal("missing result from surviving Run caller")
	}
}

func TestLifecycleShutdownDuringStartupIsNotLost(t *testing.T) {
	e := routeTestNewEngine(t)
	listener := routeTestHoldListener(t)
	listenerInjected := routeTestInjectListener(t, listener)
	certPath, keyPath := routeTestWriteTLSKeyPair(t)
	e.configure(func(cfg *Config) {
		cfg.Port = routeTestPort(listener.Addr().String())
		cfg.CertFile = certPath
		cfg.KeyFile = keyPath
	})

	// Hold the publication lock so Run cannot make the server visible after the
	// netListen seam returns. Queue Shutdown first; once that seam is reached,
	// releasing the lock makes the startup-window lost-shutdown bug deterministic.
	e.startMu.Lock()
	var unlockOnce sync.Once
	unlock := func() { unlockOnce.Do(e.startMu.Unlock) }
	t.Cleanup(unlock)

	shutdownEntered := make(chan struct{})
	var shutdownEnterOnce sync.Once
	previousShutdownStart := shutdownStart
	shutdownStart = func() { shutdownEnterOnce.Do(func() { close(shutdownEntered) }) }
	t.Cleanup(func() { shutdownStart = previousShutdownStart })

	shutdownResult := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		shutdownResult <- e.shutdown(ctx)
	}()
	<-shutdownEntered
	select {
	case err := <-shutdownResult:
		t.Fatalf("Shutdown returned while server publication was blocked: %v", err)
	default:
	}

	runDone := make(chan struct{})
	var runPanic any
	go func() {
		defer close(runDone)
		runPanic = routeTestCapturePanic(e.run)
	}()
	t.Cleanup(func() {
		unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = e.shutdown(ctx)
		cancel()
		e.startMu.Lock()
		server := e.server
		e.startMu.Unlock()
		if server != nil {
			_ = server.Close()
		}
		_ = listener.Close()
		e.hijacks.closeAll()
		routeTestWaitDone(t, runDone, "startup-window Run goroutine")
	})

	routeTestWaitDone(t, listenerInjected, "Run listener injection")
	unlock()

	select {
	case err := <-shutdownResult:
		if err != nil {
			t.Errorf("Shutdown after listener creation = %v, want nil", err)
		}
	case <-time.After(routeTestLifecycleTimeout):
		t.Fatal("Shutdown did not finish after listener creation")
	}

	select {
	case <-runDone:
		if runPanic != nil {
			t.Errorf("Run panicked while honoring startup Shutdown: %v", runPanic)
		}
	case <-time.After(time.Second):
		t.Error("Run kept serving after Shutdown during the listener-to-server publication window")
	}
}

func TestLifecycleGracefulShutdownWaitsForActiveRequest(t *testing.T) {
	e := routeTestNewEngine(t)
	listener := routeTestHoldListener(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseHandler := func() { releaseOnce.Do(func() { close(release) }) }

	e.register(methodGet, "/slow", func(w http.ResponseWriter) {
		close(entered)
		<-release
		_, _ = io.WriteString(w, "finished")
	})
	server := routeTestStartServer(t, e, listener)
	t.Cleanup(releaseHandler)
	client := routeTestHTTPClient(t)
	requestResult := make(chan routeTestHTTPResult, 1)
	go func() {
		response, err := client.Get("http://" + listener.Addr().String() + "/slow")
		if err != nil {
			requestResult <- routeTestHTTPResult{err: err}
			return
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		requestResult <- routeTestHTTPResult{status: response.StatusCode, body: string(body), err: err}
	}()

	select {
	case <-entered:
	case <-time.After(routeTestLifecycleTimeout):
		t.Fatal("active request did not enter handler")
	}

	shutdownResult := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		shutdownResult <- e.shutdown(ctx)
	}()

	// cancelStreams runs only after Shutdown has acquired the server and begun
	// draining, so this is a deterministic indication that graceful shutdown is
	// now waiting for the active handler.
	select {
	case <-e.streamCtx.Done():
	case <-time.After(routeTestLifecycleTimeout):
		t.Fatal("Shutdown did not begin draining")
	}
	select {
	case err := <-shutdownResult:
		t.Fatalf("Shutdown returned before the active request finished: %v", err)
	default:
	}

	releaseHandler()
	select {
	case result := <-requestResult:
		if result.err != nil || result.status != http.StatusOK || result.body != "finished" {
			t.Fatalf("active request = (%d, %q, %v), want (200, finished, nil)", result.status, result.body, result.err)
		}
	case <-time.After(routeTestLifecycleTimeout):
		t.Fatal("active request did not finish")
	}
	select {
	case err := <-shutdownResult:
		if err != nil {
			t.Fatalf("graceful Shutdown: %v", err)
		}
	case <-time.After(routeTestLifecycleTimeout):
		t.Fatal("graceful Shutdown did not finish")
	}
	routeTestWaitDone(t, server.done, "Run after graceful Shutdown")
	if server.panicValue != nil {
		t.Fatalf("Run panicked during graceful Shutdown: %v", server.panicValue)
	}

	const shutdownCallers = 16
	results := make(chan error, shutdownCallers)
	start := make(chan struct{})
	for range shutdownCallers {
		go func() {
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			results <- e.shutdown(ctx)
		}()
	}
	close(start)
	for range shutdownCallers {
		select {
		case err := <-results:
			if err != nil {
				t.Errorf("repeated concurrent Shutdown: %v", err)
			}
		case <-time.After(routeTestLifecycleTimeout):
			t.Fatal("repeated concurrent Shutdown timed out")
		}
	}
}

func TestLifecycleShutdownTimeoutCanBeRetried(t *testing.T) {
	e := routeTestNewEngine(t)
	listener := routeTestHoldListener(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseHandler := func() { releaseOnce.Do(func() { close(release) }) }

	e.register(methodGet, "/slow", func(w http.ResponseWriter) {
		close(entered)
		<-release
		_, _ = io.WriteString(w, "finished")
	})
	server := routeTestStartServer(t, e, listener)
	t.Cleanup(releaseHandler)
	client := routeTestHTTPClient(t)
	requestResult := make(chan routeTestHTTPResult, 1)
	go func() {
		response, err := client.Get("http://" + listener.Addr().String() + "/slow")
		if err != nil {
			requestResult <- routeTestHTTPResult{err: err}
			return
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		requestResult <- routeTestHTTPResult{status: response.StatusCode, body: string(body), err: err}
	}()
	select {
	case <-entered:
	case <-time.After(routeTestLifecycleTimeout):
		t.Fatal("active request did not enter handler")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	err := e.shutdown(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown with active request = %v, want context deadline exceeded", err)
	}
	if e.shutdownDone.Load() {
		t.Fatal("failed Shutdown latched shutdownDone")
	}

	releaseHandler()
	select {
	case result := <-requestResult:
		if result.err != nil || result.status != http.StatusOK || result.body != "finished" {
			t.Fatalf("request after timed-out Shutdown = (%d, %q, %v)", result.status, result.body, result.err)
		}
	case <-time.After(routeTestLifecycleTimeout):
		t.Fatal("request did not finish after timed-out Shutdown")
	}

	ctx, cancel = context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := e.shutdown(ctx); err != nil {
		t.Fatalf("retry Shutdown: %v", err)
	}
	routeTestWaitDone(t, server.done, "Run after retried Shutdown")
	if server.panicValue != nil {
		t.Fatalf("Run panicked after retried Shutdown: %v", server.panicValue)
	}
}

func TestLifecycleShutdownLockWaitHonorsContext(t *testing.T) {
	e := routeTestNewEngine(t)
	e.runState.Store(runStateRunning)
	e.server = &http.Server{}
	e.shutdownMu <- struct{}{}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := e.shutdown(ctx)
	elapsed := time.Since(started)
	<-e.shutdownMu
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown while another drain holds the lock = %v, want context deadline exceeded", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("Shutdown ignored context while waiting for the lock: %s", elapsed)
	}
}

func TestLifecycleShutdownClosesHijackedConnections(t *testing.T) {
	e := routeTestNewEngine(t)
	listener := routeTestHoldListener(t)
	hijacked := make(chan net.Conn, 1)
	hijackErrors := make(chan error, 1)
	e.register(methodGet, "/hijack", func(w http.ResponseWriter) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			hijackErrors <- err
			return
		}
		hijacked <- conn
	})
	server := routeTestStartServer(t, e, listener)

	conn, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatalf("dial hijack route: %v", err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(routeTestLifecycleTimeout)); err != nil {
		t.Fatalf("set client deadline: %v", err)
	}
	if _, err := fmt.Fprintf(conn, "GET /hijack HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", listener.Addr().String()); err != nil {
		t.Fatalf("write hijack request: %v", err)
	}

	var owned net.Conn
	select {
	case owned = <-hijacked:
	case err := <-hijackErrors:
		t.Fatalf("Hijack: %v", err)
	case <-time.After(routeTestLifecycleTimeout):
		t.Fatal("handler did not hijack the connection")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := e.shutdown(ctx); err != nil {
		t.Fatalf("Shutdown with hijacked connection: %v", err)
	}
	routeTestWaitDone(t, server.done, "Run after hijacked-connection Shutdown")
	if server.panicValue != nil {
		t.Fatalf("Run panicked after hijacked-connection Shutdown: %v", server.panicValue)
	}

	buffer := make([]byte, 1)
	n, readErr := conn.Read(buffer)
	if n != 0 || readErr == nil {
		t.Fatalf("hijacked connection read after Shutdown = (%d, %v), want closed connection", n, readErr)
	}
	// The registry owns the connection and closes it during Shutdown. Closing
	// the wrapper again is harmless and keeps cleanup safe on assertion failures.
	_ = owned.Close()
}

func TestLifecycleTLSStartupErrorPanicsBeforePublish(t *testing.T) {
	e := routeTestNewEngine(t)
	dir := t.TempDir()
	certPath := dir + string(os.PathSeparator) + "invalid-cert.pem"
	keyPath := dir + string(os.PathSeparator) + "invalid-key.pem"
	if err := os.WriteFile(certPath, []byte("not a certificate"), 0o600); err != nil {
		t.Fatalf("write invalid certificate: %v", err)
	}
	if err := os.WriteFile(keyPath, []byte("not a key"), 0o600); err != nil {
		t.Fatalf("write invalid key: %v", err)
	}
	listener := routeTestHoldListener(t)
	routeTestInjectListener(t, listener)
	e.configure(func(cfg *Config) {
		cfg.Port = routeTestPort(listener.Addr().String())
		cfg.CertFile = certPath
		cfg.KeyFile = keyPath
	})
	if !e.cfg.useTLS {
		t.Fatal("paired TLS paths did not enable TLS")
	}

	panicValue := routeTestRequirePanic(t, e.run)
	if !strings.Contains(fmt.Sprint(panicValue), "PEM") && !strings.Contains(fmt.Sprint(panicValue), "certificate") {
		t.Fatalf("TLS startup panic = %v, want key-pair loading error", panicValue)
	}
	routeTestRequireFailedStartupState(t, e)
}

func TestLifecycleListenErrorPanicsWithoutServer(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T, *engine)
		want  string
	}{
		{
			name: "invalid port",
			setup: func(_ *testing.T, e *engine) {
				e.cfg.Port = "70000"
			},
			want: "invalid port",
		},
		{
			name: "address already in use",
			setup: func(t *testing.T, e *engine) {
				listener, err := net.Listen("tcp", ":0")
				if err != nil {
					t.Fatalf("reserve listening port: %v", err)
				}
				t.Cleanup(func() { _ = listener.Close() })
				e.cfg.Port = routeTestPort(listener.Addr().String())
			},
			want: "bind",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := routeTestNewEngine(t)
			tt.setup(t, e)
			panicValue := routeTestRequirePanic(t, e.run)
			if !strings.Contains(fmt.Sprint(panicValue), tt.want) {
				t.Fatalf("listen failure panic = %v, want substring %q", panicValue, tt.want)
			}
			routeTestRequireFailedStartupState(t, e)
		})
	}
}
