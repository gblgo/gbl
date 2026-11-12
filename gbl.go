// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"context"
	"errors"
	"fmt"
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

// Engine holds all mutable framework state: the routing trees, the queued
// registrations, the middleware chain and the panic renderer. Once it has been
// created it is fully self-contained, so a process may run several isolated
// engines (for example one per test) without them observing each other's routes.
//
// The package-level functions (Get, Post, Run, Group, Use, ...) are a thin
// facade over defaultEngine, so existing global-style code keeps working
// unchanged while New() offers the instance-isolated alternative.
type Engine struct {
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

// defaultEngine backs the package-level API. It is created lazily through New
// so the two constructor paths cannot drift apart.
var defaultEngine = New()

// New builds an empty Engine with no routes and no hooks.
func New() *Engine {
	return &Engine{
		roots:      make([]*routeNode, methodCount),
		astFileMap: make(map[string]*ast.File),
		fest:       token.NewFileSet(),
		logger:     newStdLogger(),
	}
}

// Run starts the server on the configured address and blocks until it stops.
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
func (e *Engine) Run() {
	// Configuration initialization
	//
	// The snapshot is shared with IniBySection and is loaded on first access,
	// so registration code may already have pinned it. A missing application.ini
	// yields an empty snapshot, which generateConfig resolves to pure defaults.
	cfg, err := generateConfig(iniSnapshot())
	if err != nil {
		panic(err)
	}

	// Install level-aware file logging before routing so even startup-time route
	// warnings reach the right file. With nothing configured this leaves the
	// default stderr logger in place and opens no file.
	if err := e.setupLogging(cfg); err != nil {
		panic(fmt.Errorf("gbl: open log file: %w", err))
	}

	// Build the routing trees from everything registered so far.
	e.generateRoute()

	// Concatenate address
	addr := colon + cfg.Port
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		panic(err)
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
	// goroutine a bare Serve error would panic and skip listener cleanup.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-stop
		ctx, cancel := context.WithTimeout(context.Background(), cfg.shutdownGracePeriod())
		defer cancel()
		_ = e.Shutdown(ctx)
	}()

	e.printBanner(cfg, ln.Addr())

	// Determine whether to enable TLS
	var serveErr error
	if cfg.useTLS {
		serveErr = server.ServeTLS(ln, cfg.CertFile, cfg.KeyFile)
	} else {
		serveErr = server.Serve(ln)
	}

	// ErrServerClosed is the expected result of a graceful shutdown, not a
	// crash: only genuine failures propagate as a panic.
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		panic(serveErr)
	}
}

// Shutdown is the instance-level counterpart of the package-level Shutdown.
func (e *Engine) Shutdown(ctx context.Context) error {
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
func (e *Engine) buildHandler() http.Handler {
	if e.cors == nil {
		return e
	}
	return &corsHandler{engine: e, config: e.cors}
}

// ServeHTTP lets an Engine act directly as an http.Handler, delegating to the
// full request pipeline.
func (e *Engine) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	e.serveHTTP(w, r)
}

// shutdownGracePeriod is how long a signal-triggered shutdown waits for
// in-flight requests before forcing connections closed.
func (gblConfig) shutdownGracePeriod() time.Duration { return 5 * time.Second }
