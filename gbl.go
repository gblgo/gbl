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
// type stays unexported: it exists only so the package own tests can build
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
	// file set backing astFileMap
	fest *token.FileSet
	// lazily built "<file>:<line>"->param names from the binary DWARF; the sourceless fallback
	dwarfIndex map[string]dwarfParamNames
	// live server, set by Run; nil before start
	server *http.Server
	// owns connections removed from net/http by Hijack
	hijacks *hijackRegistry
	// cancels streamCtx so long-lived SSE handlers can drain
	cancelStreams context.CancelFunc

	// sync primitives
	// guards listener/server assignment
	startMu sync.Mutex
	// serializes shutdown attempts so Run can wait for an in-progress drain
	shutdownMu sync.Mutex

	// routes frozen into trees; also gates the read-only-after-Run contract
	generated bool
	// the server has been drained successfully; later Shutdown calls are no-ops
	shutdownDone atomic.Bool
}

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
		fest:          token.NewFileSet(),
		logger:        newStdLogger(),
		cfg:           defaultConfig(),
		hijacks:       newHijackRegistry(),
		streamCtx:     streamCtx,
		cancelStreams: cancelStreams,
	}
}

// Run starts the single public engine on the configured address and blocks
// until it stops.
//
// On SIGINT or SIGTERM it performs a graceful shutdown instead of exiting
// abruptly: the listener closes and in-flight requests get a window to finish.
// Any fatal startup error panics, matching the framework existing contract.
func Run() { defaultEngine.run() }

// Shutdown gracefully stops the server started by Run, waiting for active
// requests until ctx expires. A successful shutdown also closes connections
// previously hijacked by handlers. It is safe to call more than once and from
// another goroutine than Run; a failed attempt can be retried for the remaining
// active connections. Calling it before Run returns "http: Server closed".
func Shutdown(ctx context.Context) error { return defaultEngine.shutdown(ctx) }

// run is the instance-level counterpart of the package-level Run.
func (e *engine) run() {
	// The engine serves exactly once: a second Run would re-enter startup on a
	// frozen route set, so it is a misuse of the same contract the assembling
	// guards below enforce.
	if e.generated {
		panic("gbl.Run called twice; the engine serves only once")
	}

	// The configuration is whatever Configure left on the engine, defaults
	// included. The kernel reads no file; an integration layer resolves its
	// own sources and calls Configure before Run.
	cfg := e.cfg
	if cfg.useTLS {
		if _, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile); err != nil {
			e.failStartup(err)
		}
	}

	// Build the routing trees from everything registered so far.
	e.generateRoute()

	// Concatenate address
	addr := colon + cfg.Port
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		e.failStartup(err)
	}

	server := &http.Server{
		Addr:              addr,
		Handler:           e.buildHandler(),
		ReadTimeout:       cfg.ReadTimeout,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
	}

	// Publish the server before serving so a concurrent Shutdown can reach it.
	e.startMu.Lock()
	e.server = server
	e.startMu.Unlock()

	// A signal-triggered graceful shutdown returns nil from Serve; without this
	// goroutine a bare Serve error would panic and skip listener cleanup. The done
	// channel lets the goroutine exit when Serve returns on its own (e.g. a manual
	// Shutdown), while watcherDone lets Run wait for any signal drain already in
	// progress before it returns.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	done := make(chan struct{})
	watcherDone := make(chan struct{})
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

	// Determine whether to enable TLS
	var serveErr error
	if cfg.useTLS {
		serveErr = server.ServeTLS(ln, cfg.CertFile, cfg.KeyFile)
	} else {
		serveErr = server.Serve(ln)
	}

	// Stop watching for signals and wait for a signal-triggered drain that may
	// already be running. A manual Shutdown owns its own context, so Run waits
	// for that attempt to release shutdownMu before returning as well.
	signal.Stop(stop)
	close(done)
	<-watcherDone
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
	e.startMu.Lock()
	server := e.server
	e.startMu.Unlock()

	if server == nil {
		return http.ErrServerClosed
	}

	e.shutdownMu.Lock()
	defer e.shutdownMu.Unlock()

	// Only a completed drain latches: an expired grace period or an interrupted
	// wait keeps remaining non-SSE connections alive, so the same call can be
	// made again. SSE streams are canceled as soon as shutdown starts.
	if e.shutdownDone.Load() {
		return nil
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

// shutdownGracePeriod is how long a signal-triggered shutdown waits for
// in-flight requests before forcing connections closed.
const shutdownGracePeriod = 5 * time.Second

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
