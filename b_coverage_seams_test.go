// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

// Tests that replace package-level seams must remain serial. Every replacement
// below registers a t.Cleanup restoration before the test continues.

import (
	"context"
	"debug/dwarf"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"runtime/debug"
	"strings"
	"syscall"
	"testing"
	"time"
)

const coverageSeamTimeout = 3 * time.Second

func coverageSeamReplace[T any](t *testing.T, seam *T, replacement T) {
	t.Helper()
	original := *seam
	*seam = replacement
	t.Cleanup(func() { *seam = original })
}

func coverageSeamWait[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(coverageSeamTimeout):
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}

func TestCoverageSeamBannerReadBuildInfoFailure(t *testing.T) {
	coverageSeamReplace(t, &readBuildInfo, func() (*debug.BuildInfo, bool) {
		return nil, false
	})
	if got := bannerVersion(); got != versionDevel {
		t.Fatalf("bannerVersion() = %q, want %q", got, versionDevel)
	}
}

type coverageSeamGzipWriter struct {
	writeErr error
	closeErr error
	calls    []string
	body     []byte
}

func (w *coverageSeamGzipWriter) Write(p []byte) (int, error) {
	w.calls = append(w.calls, "write")
	w.body = append(w.body, p...)
	return len(p), w.writeErr
}

func (w *coverageSeamGzipWriter) Close() error {
	w.calls = append(w.calls, "close")
	return w.closeErr
}

func TestCoverageSeamWriteGzipAndClose(t *testing.T) {
	writeFailure := errors.New("gzip write failed")
	closeFailure := errors.New("gzip close failed")
	tests := []struct {
		name      string
		writeErr  error
		closeErr  error
		wantWrite error
		wantClose error
	}{
		{name: "success"},
		{name: "write failure", writeErr: writeFailure, wantWrite: writeFailure},
		{name: "close failure", closeErr: closeFailure, wantClose: closeFailure},
		{name: "both failures", writeErr: writeFailure, closeErr: closeFailure, wantWrite: writeFailure, wantClose: closeFailure},
	}
	body := []byte("gzip payload")
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			writer := &coverageSeamGzipWriter{writeErr: tt.writeErr, closeErr: tt.closeErr}
			writeErr, closeErr := writeGzipAndClose(writer, body)
			if !errors.Is(writeErr, tt.wantWrite) {
				t.Errorf("Write error = %v, want %v", writeErr, tt.wantWrite)
			}
			if !errors.Is(closeErr, tt.wantClose) {
				t.Errorf("Close error = %v, want %v", closeErr, tt.wantClose)
			}
			if got := strings.Join(writer.calls, ","); got != "write,close" {
				t.Errorf("gzip calls = %q, want write followed by close", got)
			}
			if string(writer.body) != string(body) {
				t.Errorf("gzip input = %q, want %q", writer.body, body)
			}
		})
	}
}

func TestCoverageSeamRunClosedListener(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("create closed-listener fixture: %v", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("close listener fixture: %v", err)
	}
	coverageSeamReplace(t, &netListen, func(string, string) (net.Listener, error) {
		return listener, nil
	})

	registered := make(chan chan<- os.Signal, 1)
	notified := make(chan []os.Signal, 1)
	stopped := make(chan chan<- os.Signal, 1)
	coverageSeamReplace(t, &signalNotify, func(stop chan<- os.Signal, sig ...os.Signal) {
		registered <- stop
		notified <- append([]os.Signal(nil), sig...)
	})
	coverageSeamReplace(t, &signalStop, func(stop chan<- os.Signal) { stopped <- stop })

	e := routeTestNewEngine(t)
	e.cfg.Port = routeTestPort(address)
	panicValue := routeTestRequirePanic(t, e.run)
	serveErr, ok := panicValue.(error)
	if !ok || !errors.Is(serveErr, net.ErrClosed) {
		t.Fatalf("Run panic = %#v, want a closed-listener error", panicValue)
	}
	if !errors.Is(e.streamCtx.Err(), context.Canceled) {
		t.Fatal("abnormal Serve exit did not cancel stream context")
	}
	e.hijacks.mu.Lock()
	hijacksClosed := e.hijacks.closed
	e.hijacks.mu.Unlock()
	if !hijacksClosed {
		t.Fatal("abnormal Serve exit did not close the hijack registry")
	}
	if got := e.runState.Load(); got != runStateStopped {
		t.Errorf("Run state = %d, want runStateStopped", got)
	}
	select {
	case <-e.startDone:
	default:
		t.Fatal("Run did not release startup waiters")
	}
	e.startMu.Lock()
	server := e.server
	e.startMu.Unlock()
	if server == nil {
		t.Fatal("Run did not publish the server before Serve")
	}
	_ = server.Close()

	gotSignals := coverageSeamWait(t, notified, "signal notification")
	wantSignals := []os.Signal{syscall.SIGINT, syscall.SIGTERM}
	if len(gotSignals) != len(wantSignals) {
		t.Fatalf("signal.Notify signals = %v, want %v", gotSignals, wantSignals)
	}
	for i, want := range wantSignals {
		if gotSignals[i] != want {
			t.Errorf("signal.Notify signal %d = %v, want %v", i, gotSignals[i], want)
		}
	}
	wantStop := coverageSeamWait(t, registered, "registered signal channel")
	if got := coverageSeamWait(t, stopped, "signal cleanup"); got != wantStop {
		t.Fatal("signal.Stop received a different channel")
	}
}

func TestCoverageSeamRunSignalShutdownTimeoutForcesClose(t *testing.T) {
	coverageSeamReplace(t, &shutdownGracePeriod, 20*time.Millisecond)
	registered := make(chan chan<- os.Signal, 1)
	notified := make(chan []os.Signal, 1)
	stopped := make(chan chan<- os.Signal, 1)
	coverageSeamReplace(t, &signalNotify, func(stop chan<- os.Signal, sig ...os.Signal) {
		registered <- stop
		notified <- append([]os.Signal(nil), sig...)
	})
	coverageSeamReplace(t, &signalStop, func(stop chan<- os.Signal) { stopped <- stop })

	// Hold the loopback listener through Run so the port cannot be reused.
	listener := routeTestHoldListener(t)
	routeTestInjectListener(t, listener)

	e := routeTestNewEngine(t)
	e.cfg.Port = routeTestPort(listener.Addr().String())
	logger := &logTestCaptureLogger{}
	e.setLogger(logger)
	requestEntered := make(chan struct{})
	requestCanceled := make(chan struct{})
	e.register(methodGet, "/slow", func(r *http.Request) {
		close(requestEntered)
		<-r.Context().Done()
		close(requestCanceled)
	})

	owned, peer := net.Pipe()
	t.Cleanup(func() {
		_ = owned.Close()
		_ = peer.Close()
	})
	if _, ok := e.hijacks.track(owned); !ok {
		t.Fatal("failed to track hijacked connection")
	}
	runDone := make(chan struct{})
	var runPanic any
	go func() {
		defer close(runDone)
		runPanic = routeTestCapturePanic(e.run)
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), coverageSeamTimeout)
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
		coverageSeamWait(t, runDone, "Run cleanup")
	})

	coverageSeamWait(t, e.startDone, "server publication")
	e.startMu.Lock()
	server := e.server
	e.startMu.Unlock()
	if server == nil {
		coverageSeamWait(t, runDone, "failed Run exit")
		t.Fatalf("Run exited before serving: %v", runPanic)
	}
	gotSignals := coverageSeamWait(t, notified, "signal notification")
	wantSignals := []os.Signal{syscall.SIGINT, syscall.SIGTERM}
	if len(gotSignals) != len(wantSignals) || gotSignals[0] != wantSignals[0] || gotSignals[1] != wantSignals[1] {
		t.Fatalf("signal.Notify signals = %v, want %v", gotSignals, wantSignals)
	}
	signals := coverageSeamWait(t, registered, "registered signal channel")

	client := routeTestHTTPClient(t)
	client.Timeout = 0
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	defer cancelRequest()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, "http://"+listener.Addr().String()+"/slow", nil)
	if err != nil {
		t.Fatalf("create slow request: %v", err)
	}
	requestResult := make(chan error, 1)
	go func() {
		response, err := client.Do(request)
		if response != nil {
			_ = response.Body.Close()
		}
		requestResult <- err
	}()
	coverageSeamWait(t, requestEntered, "active request")
	if err := requestCtx.Err(); err != nil {
		t.Fatalf("slow request context canceled before signal injection: %v", err)
	}

	// This is an injected channel send, not a signal delivered to the test process.
	signals <- syscall.SIGTERM
	// Start the fallback timeout only after injection so it cannot manufacture
	// the request cancellation that forced shutdown is expected to cause.
	requestTimer := time.AfterFunc(2*coverageSeamTimeout, cancelRequest)
	defer requestTimer.Stop()
	coverageSeamWait(t, requestCanceled, "forced request-context cancellation")
	coverageSeamWait(t, runDone, "Run after forced shutdown")
	if runPanic != nil {
		t.Fatalf("Run panicked after forced shutdown: %v", runPanic)
	}
	if err := coverageSeamWait(t, requestResult, "client request exit"); err == nil {
		t.Fatal("active request succeeded after its connection was force-closed")
	}
	if got := coverageSeamWait(t, stopped, "signal cleanup"); got != signals {
		t.Fatal("signal.Stop received a different channel")
	}

	if err := peer.SetReadDeadline(time.Now().Add(coverageSeamTimeout)); err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("set hijacked-connection read deadline: %v", err)
	}
	buffer := make([]byte, 1)
	n, readErr := peer.Read(buffer)
	if n != 0 || !errors.Is(readErr, io.EOF) {
		t.Fatalf("hijacked connection read after forced shutdown = (%d, %v), want (0, EOF)", n, readErr)
	}

	foundWarning := false
	for _, record := range logger.records {
		if record.level != "WARN" {
			continue
		}
		foundWarning = true
		if record.msg != "graceful shutdown failed; forcing connections closed" {
			t.Errorf("shutdown warning = %q", record.msg)
		}
		if len(record.kv) != 2 || record.kv[0] != "error" {
			t.Errorf("shutdown warning fields = %v, want error field", record.kv)
			continue
		}
		shutdownErr, ok := record.kv[1].(error)
		if !ok || !errors.Is(shutdownErr, context.DeadlineExceeded) {
			t.Errorf("shutdown warning error = %#v, want context deadline exceeded", record.kv[1])
		}
	}
	if !foundWarning {
		t.Fatal("signal-triggered shutdown timeout did not log forced closure")
	}
}

func TestCoverageSeamDWARFExecutableError(t *testing.T) {
	executableFailure := errors.New("executable lookup failed")
	loaderCalled := false
	coverageSeamReplace(t, &osExecutable, func() (string, error) {
		return "", executableFailure
	})
	coverageSeamReplace(t, &selfDWARFLoader, func(string) *dwarf.Data {
		loaderCalled = true
		return nil
	})
	if got := buildDWARFIndexFor(nil); got != nil {
		t.Fatalf("DWARF index after executable error = %v, want nil", got)
	}
	if loaderCalled {
		t.Fatal("DWARF loader ran after executable lookup failed")
	}
}

func TestCoverageSeamDWARFLoaderNil(t *testing.T) {
	const executable = "coverage-seam-executable"
	var loadedPath string
	coverageSeamReplace(t, &osExecutable, func() (string, error) {
		return executable, nil
	})
	coverageSeamReplace(t, &selfDWARFLoader, func(path string) *dwarf.Data {
		loadedPath = path
		return nil
	})
	if got := buildDWARFIndexFor(nil); got != nil {
		t.Fatalf("DWARF index without loader data = %v, want nil", got)
	}
	if loadedPath != executable {
		t.Fatalf("DWARF loader path = %q, want %q", loadedPath, executable)
	}
}

func TestCoverageSeamDWARFLoaderData(t *testing.T) {
	data, positions := dwarfTestBuildFixture(t)
	key := positions.functions["named"]
	const executable = "coverage-seam-executable"
	var loadedPath string
	coverageSeamReplace(t, &osExecutable, func() (string, error) {
		return executable, nil
	})
	coverageSeamReplace(t, &selfDWARFLoader, func(path string) *dwarf.Data {
		loadedPath = path
		return data
	})
	index := buildDWARFIndexFor(map[string]struct{}{key: {}})
	if loadedPath != executable {
		t.Fatalf("DWARF loader path = %q, want %q", loadedPath, executable)
	}
	if len(index) != 1 {
		t.Fatalf("targeted DWARF index has %d entries, want 1: %v", len(index), index)
	}
	dwarfTestCheckEntry(t, index, key, []string{"first", "", "third"}, false)
}

func TestCoverageSeamCompressWriteFailure(t *testing.T) {
	failure := errors.New("injected gzip failure")
	coverageSeamReplace(t, &gzipWriteAndClose,
		func(io.WriteCloser, []byte) (error, error) { return failure, nil })
	body := strings.Repeat("compressible failure payload", 100)
	e := mwTestNewEngine(t, func() string { return body }, Compress(DefaultCompression))
	recorder := mwTestGzipResponse(e, encodingGzip)
	mwTestRequirePlainBody(t, recorder, []byte(body))
	if got := recorder.Header().Get(contentEncoding); got != empty {
		t.Fatalf("Content-Encoding = %q, want empty after gzip failure", got)
	}
}
