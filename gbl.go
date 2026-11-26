// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"context"
	"crypto/tls"
	"errors"
	"go/ast"
	"go/token"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// engine holds all mutable framework state: the routing trees, the queued
// registrations, the middleware chain and the panic renderer.
//
// GBL exposes exactly one public engine — the package-level API (Get, Post,
// Group, Use, Run, ...) operates on the single defaultEngine behind the scenes,
// so registration and startup can never land on two different instances. The
// type stays unexported: it exists only so package tests can build
// isolated engines (each with its own routes and logger) without a listening
// port, by driving generateRoute and ServeHTTP directly.
type engine struct {
	// one routing tree per supported method (dynamic routes with path params)
	roots []*routeNode
	// registrations queued before generateRoute
	routeMetaSlice []*routeMeta
	// global chain, merged with each group at generation
	middlewares []HandlerFunc

	// user hook that renders panic values
	panicProcessor PanicProcessor
	// sink for internal logs; never nil
	logger Logger
	// lifetime shared by every SSE stream and canceled when shutdown starts
	streamCtx context.Context

	// O(1) lookup for static routes (no path params)
	staticRoutes [methodCount]map[string]*routeHandler
	// runtime configuration; defaults overlaid by Configure
	cfg *Config
	// nil disables the CORS layer
	cors *CorsConfig
	// parsed source files for parameter names
	astFileMap map[string]*ast.File
	// per-file function line spans, collected once per file during name recovery
	astSpans map[*ast.File][]funcSpan
	// per-file line index built from astSpans for logarithmic handler lookup
	astSpanIndex map[*ast.File]*funcSpanIndex
	// file set backing astFileMap
	fest *token.FileSet
	// lazily built "<file>:<line>"->param names from the binary DWARF; the sourceless fallback
	dwarfIndex map[string]dwarfParamNames
	// handler positions collected before generation so DWARF indexes only relevant entries
	dwarfTargets map[string]struct{}
	// live server, published by Run; nil before publication
	server *http.Server
	// owns connections removed from net/http by Hijack
	hijacks *hijackRegistry
	// cancels streamCtx so long-lived SSE handlers can drain
	cancelStreams context.CancelFunc

	// sync primitives
	// guards server publication only; net.Listen deliberately runs outside this lock
	startMu sync.Mutex
	// serializes shutdown attempts so Run can wait for an in-progress drain
	shutdownMu sync.Mutex

	// runState makes the single-use claim atomic, independently of route generation
	runState atomic.Uint32
	// closed when Run publishes its server or aborts startup, unblocking an early Shutdown
	startDone chan struct{}
	// routes frozen into trees; also gates the read-only-after-Run contract
	generated bool
	// the server has been drained successfully; later Shutdown calls are no-ops
	shutdownDone atomic.Bool
}

// runState values. A failed first Run still consumes the single-use claim, while
// runStateIdle alone allows Shutdown before Run to return without latching.
const (
	// runStateIdle permits the first Run and a pre-start Shutdown.
	runStateIdle uint32 = iota
	// runStateStarting makes concurrent Run calls fail and lets Shutdown wait.
	runStateStarting
	// runStateRunning marks a published server.
	runStateRunning
	// runStateStopped permanently consumes the single-use Run claim.
	runStateStopped
)

// defaultEngine is the one and only engine, backing the entire package-level
// API. It is created through newEngine so the constructor has a single path.
var defaultEngine = newEngine()

// newEngine builds an empty engine with no routes and no hooks. It is internal:
// callers reach the framework through the package-level functions, while tests
// use it to stand up an isolated instance.
func newEngine() *engine {
	streamCtx, cancelStreams := context.WithCancel(context.Background())
	return &engine{
		roots:         make([]*routeNode, methodCount),
		astFileMap:    make(map[string]*ast.File),
		astSpans:      make(map[*ast.File][]funcSpan),
		astSpanIndex:  make(map[*ast.File]*funcSpanIndex),
		fest:          token.NewFileSet(),
		logger:        newStdLogger(),
		cfg:           defaultConfig(),
		hijacks:       newHijackRegistry(),
		streamCtx:     streamCtx,
		cancelStreams: cancelStreams,
		startDone:     make(chan struct{}),
	}
}

// Run starts the single public engine on the configured address and blocks
// until it stops. Only the first call may start it; concurrent and subsequent
// calls panic with a "called twice" diagnostic, even if the first startup fails.
//
// On SIGINT or SIGTERM it performs a graceful shutdown instead of exiting
// abruptly: the listener closes and in-flight requests get a window to finish.
// Any fatal startup error panics, matching the existing framework contract.
func Run() { defaultEngine.run() }

// Shutdown gracefully stops the server started by Run, waiting for active
// requests until ctx expires. A successful shutdown also closes connections
// previously hijacked by handlers. It is safe to call more than once and from
// another goroutine than Run; a failed attempt can be retried for the remaining
// active connections. Calling it before Run returns "http: Server closed". A
// call during startup waits for server publication, then drains that server.
func Shutdown(ctx context.Context) error { return defaultEngine.shutdown(ctx) }

// Package-level seams for Run's process and network boundaries. Tests replace
// them serially and restore them with t.Cleanup; production keeps the standard
// library behavior and five-second signal drain.
var (
	// signalNotify registers the process signal channel.
	signalNotify = signal.Notify
	// signalStop unregisters the process signal channel.
	signalStop = signal.Stop
	// netListen creates the server listener.
	netListen = net.Listen
	// shutdownGracePeriod bounds signal-triggered graceful shutdown.
	shutdownGracePeriod = 5 * time.Second
)

// run is the instance-level counterpart of the package-level Run.
func (e *engine) run() {
	// Claim the engine before any startup work. Route generation is not a usable
	// guard here: concurrent callers can all observe generated == false and race
	// into net.Listen, producing port errors instead of the single-use diagnostic.
	if !e.runState.CompareAndSwap(runStateIdle, runStateStarting) {
		panic("gbl.Run called twice; the engine serves only once")
	}
	defer e.runState.Store(runStateStopped)

	// Startup failure must also release a Shutdown already waiting for publication.
	// The successful path closes the channel at publication instead of at Run exit.
	// The same deferred cleanup stops the signal watcher on every exit path.
	startDoneClosed := false
	var stop chan os.Signal
	var done, watcherDone chan struct{}
	defer func() {
		if !startDoneClosed {
			close(e.startDone)
		}
		if stop != nil {
			signalStop(stop)
			close(done)
			<-watcherDone
		}
	}()

	// Preserve the original frozen-route guard for callers that generate routes
	// manually; the atomic claim above is what makes concurrent Run calls safe.
	if e.generated {
		panic("gbl.Run called twice; the engine serves only once")
	}

	// The configuration is whatever Configure left on the engine, defaults
	// included. The kernel reads no file; an integration layer resolves its
	// own sources and calls Configure before Run. Load TLS material once and let
	// ServeTLS reuse it rather than reading the same files a second time.
	cfg := e.cfg
	var certificate tls.Certificate
	if cfg.useTLS {
		var err error
		certificate, err = tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			e.failStartup(err)
		}
	}

	// Build the routing trees from everything registered so far.
	e.generateRoute()

	// Listen outside startMu: callers may hold that publication lock while
	// waiting for the listener to become observable.
	addr := colon + cfg.Port
	ln, err := netListen("tcp", addr)
	if err != nil {
		e.failStartup(err)
	}
	defer func() { _ = ln.Close() }()

	server := &http.Server{
		Addr:              addr,
		Handler:           e.buildHandler(),
		ReadTimeout:       cfg.ReadTimeout,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
	}
	if cfg.useTLS {
		server.TLSConfig = &tls.Config{Certificates: []tls.Certificate{certificate}}
	}

	// A signal-triggered graceful shutdown makes Serve return ErrServerClosed;
	// without this watcher the signal would never initiate the drain. The done
	// channel lets the goroutine exit when Serve returns on its own (e.g. a manual
	// Shutdown), while watcherDone lets Run wait for any signal drain already in
	// progress before it returns.
	stop = make(chan os.Signal, 1)
	signalNotify(stop, syscall.SIGINT, syscall.SIGTERM)
	done = make(chan struct{})
	watcherDone = make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case <-stop:
			ctx, cancel := context.WithTimeout(context.Background(), shutdownGracePeriod)
			defer cancel()
			if err := e.shutdown(ctx); err != nil {
				e.logger.Warn("graceful shutdown failed; forcing connections closed",
					"error", err)
				_ = server.Close()
				e.hijacks.closeAll()
			}
		case <-done:
		}
	}()

	e.printBanner(cfg, ln.Addr())

	// Publish before serving. A Shutdown that arrived during startup is waiting
	// on startDone; closing it here makes that server visible even if Shutdown
	// wins the lock before Serve has registered the listener.
	e.startMu.Lock()
	e.server = server
	e.runState.Store(runStateRunning)
	close(e.startDone)
	startDoneClosed = true
	e.startMu.Unlock()

	// Empty file names tell ServeTLS to use the certificate already installed in
	// TLSConfig, preserving HTTP/2 setup without reopening the key pair.
	var serveErr error
	if cfg.useTLS {
		serveErr = server.ServeTLS(ln, empty, empty)
	} else {
		serveErr = server.Serve(ln)
	}

	// A manual Shutdown owns its own context, so Run waits for that attempt to
	// release shutdownMu before returning as well. Signal-watcher cleanup itself
	// is deferred above so startup panics cannot leave a registered watcher behind.
	if errors.Is(serveErr, http.ErrServerClosed) {
		e.shutdownMu.Lock()
		e.shutdownMu.Unlock()
	}

	// ErrServerClosed is the expected result of a graceful shutdown, not a
	// crash: only genuine failures propagate as a panic.
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		e.failStartup(serveErr)
	}
}

// shutdown is the instance-level counterpart of the package-level Shutdown.
func (e *engine) shutdown(ctx context.Context) error {
	// Taking startMu first preserves the before-Run behavior while also waiting
	// behind a test or integration layer that deliberately holds the publication
	// lock. Never wait for startDone while holding it: Run needs the lock to publish.
	e.startMu.Lock()
	state := e.runState.Load()
	server := e.server
	e.startMu.Unlock()

	if state == runStateIdle && server == nil {
		return http.ErrServerClosed
	}
	if state == runStateStarting {
		select {
		case <-e.startDone:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	e.shutdownMu.Lock()
	defer e.shutdownMu.Unlock()

	// Only a completed drain latches: an expired grace period or an interrupted
	// wait keeps remaining non-SSE connections alive, so the same call can be
	// made again. SSE streams are canceled as soon as shutdown starts.
	if e.shutdownDone.Load() {
		return nil
	}

	e.startMu.Lock()
	server = e.server
	e.startMu.Unlock()
	if server == nil {
		return http.ErrServerClosed
	}

	e.cancelStreams()
	if err := server.Shutdown(ctx); err != nil {
		return err
	}
	e.hijacks.closeAll()
	e.shutdownDone.Store(true)
	return nil
}

// buildHandler assembles the http.Handler chain: the CORS layer wraps the
// engine only when a configuration has been registered.
func (e *engine) buildHandler() http.Handler {
	if e.cors == nil {
		return e
	}
	return newCorsHandler(e, e.cors)
}

// ServeHTTP lets an engine act directly as an http.Handler, delegating to the
// full request pipeline.
func (e *engine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	e.serveHTTP(w, r)
}

// assertAssembling is the engine read-only-after-Run contract: route
// registration, middleware, loggers, CORS, configuration, panic renderers and
// size limits are all assembling-time decisions, merged into frozen structures
// by generateRoute. Calling one of those setters afterward cannot take effect
// (the chains are built, the handler is wired, the config is copied), so
// instead of silently swallowing the call the engine fails fast: a wrong
// registration ORDER is a programming error exactly like a wrong registration
// CONTENT, and deserves the same loud treatment. The panic is raised without
// the failStartup log because the process is past startup: the caller is a
// running program holding a stack trace that names its own mistake.
func (e *engine) assertAssembling(op string) {
	if e.generated {
		panic("gbl." + op + " must be called before Run; the engine is read-only while serving")
	}
}
