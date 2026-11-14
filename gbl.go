// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"context"
	"errors"
	"go/ast"
	"go/token"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// engine holds all mutable framework state: the routing trees, the queued
// registrations, the middleware chain and the panic renderer.
//
// GBL exposes exactly one public engine — the package-level API (Get, Post,
// Group, Use, Run, ...) operates on the single defaultEngine behind the scenes,
// so registration and startup can never land on two different instances. The
// type stays unexported: it exists only so the package's own tests can build
// isolated engines (each with its own routes and logger) without a listening
// port, by driving generateRoute and ServeHTTP directly.
type engine struct {
	roots          []*routeNode         // one routing tree per supported method
	routeMetaSlice []*routeMeta         // registrations queued before generateRoute
	middlewares    []HandlerFunc        // global chain, merged with each group's at generation
	panicProcessor PanicProcessor       // user hook that renders panic values
	logger         Logger               // sink for internal and access logs; never nil
	loggerReplaced bool                 // true once SetLogger installs a custom Logger
	logFiles       []*os.File           // files opened for level logging; closed by Shutdown
	cors           *CorsConfig          // nil disables the CORS layer
	astFileMap     map[string]*ast.File // parsed source files for parameter names
	fest           *token.FileSet       // file set backing astFileMap
	generated      bool                 // routes have been inserted into the trees
	server         *http.Server         // live server, set by Run; nil before start
	shutdownOnce   sync.Once            // collapse signal and manual shutdown
	startMu        sync.Mutex           // guards listener/server assignment
}

// defaultEngine is the one and only engine, backing the entire package-level
// API. It is created through newEngine so the constructor has a single path.
var defaultEngine = newEngine()

// newEngine builds an empty engine with no routes and no hooks. It is internal:
// callers reach the framework through the package-level functions, while tests
// use it to stand up an isolated instance.
func newEngine() *engine {
	return &engine{
		roots:      make([]*routeNode, methodCount),
		astFileMap: make(map[string]*ast.File),
		fest:       token.NewFileSet(),
		logger:     newStdLogger(),
	}
}

// Run starts the single public engine on the configured address and blocks
// until it stops.
//
// On SIGINT or SIGTERM it performs a graceful shutdown instead of exiting
// abruptly: the listener closes and in-flight requests get a window to finish.
// Any fatal startup error panics, matching the framework's existing contract.
func Run() { defaultEngine.Run() }

// Shutdown gracefully stops the server started by Run, waiting for active
// requests until ctx expires. It is safe to call more than once and from
// another goroutine than Run; calling it before Run returns "http: Server
// closed".
func Shutdown(ctx context.Context) error { return defaultEngine.Shutdown(ctx) }

// Run is the instance-level counterpart of the package-level Run.
func (e *engine) Run() {
	// Read the frozen configuration snapshot once. A missing application.ini
	// yields an empty snapshot; a file that exists but cannot be parsed panics
	// here, before logging can be configured — its destinations live in that same
	// file, so there is nothing to log through yet.
	snapshot := iniSnapshot()

	// Bring logging up first, before anything that can fail on a bad
	// configuration. The log destinations are plain strings, so they cannot fail
	// to parse. Resolving them ahead of the full config puts every later startup
	// error in the configured log file, not only on stderr. Such errors include
	// an invalid config value, a route conflict, and a busy port. With no
	// destination configured, the default stderr logger stays and no file opens.
	// If the destination itself is broken, failLoggingSetup falls back to the
	// console plus a last-resort gbl-start-error.log. The operator can then see
	// and fix the misconfiguration, and startup still fails fast.
	if err := e.setupLogging(loggingConfig(snapshot)); err != nil {
		e.failLoggingSetup(err)
	}

	// Resolve the full configuration now that logging is in place, so a bad value
	// is reported through the logger before it aborts startup.
	//
	// The snapshot is shared with IniBySection and is loaded on first access, so
	// registration code may already have pinned it.
	cfg, err := generateConfig(snapshot)
	if err != nil {
		e.failStartup(err)
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
	// Shutdown), so it never leaks.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		select {
		case <-stop:
			ctx, cancel := context.WithTimeout(context.Background(), cfg.shutdownGracePeriod())
			defer cancel()
			_ = e.Shutdown(ctx)
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

	// Stop watching for signals and release the watcher goroutine.
	signal.Stop(stop)
	close(done)

	// ErrServerClosed is the expected result of a graceful shutdown, not a
	// crash: only genuine failures propagate as a panic.
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		e.failStartup(serveErr)
	}
}

// Shutdown is the instance-level counterpart of the package-level Shutdown.
func (e *engine) Shutdown(ctx context.Context) error {
	e.startMu.Lock()
	server := e.server
	e.startMu.Unlock()

	if server == nil {
		return http.ErrServerClosed
	}

	var err error
	e.shutdownOnce.Do(func() { err = server.Shutdown(ctx) })

	// Release any files opened for level logging. Close errors are intentionally
	// dropped, and the slice is cleared so a repeat shutdown closes nothing twice.
	for _, f := range e.logFiles {
		_ = f.Close()
	}
	e.logFiles = nil
	return err
}

// buildHandler assembles the http.Handler chain: the CORS layer wraps the
// engine only when a configuration has been registered.
func (e *engine) buildHandler() http.Handler {
	if e.cors == nil {
		return e
	}
	return &corsHandler{engine: e, config: e.cors}
}

// ServeHTTP lets an engine act directly as an http.Handler, delegating to the
// full request pipeline.
func (e *engine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	e.serveHTTP(w, r)
}

// shutdownGracePeriod is how long a signal-triggered shutdown waits for
// in-flight requests before forcing connections closed.
func (gblConfig) shutdownGracePeriod() time.Duration { return 5 * time.Second }
