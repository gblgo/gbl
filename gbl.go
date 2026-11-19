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
	cfg            *Config              // runtime configuration; defaults overlaid by Configure
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
		cfg:        defaultConfig(),
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
	// The configuration is whatever Configure left on the engine, defaults
	// included. The kernel reads no file; an integration layer resolves its
	// own sources and calls Configure before Run.
	cfg := e.cfg

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
	return err
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
func (Config) shutdownGracePeriod() time.Duration { return 5 * time.Second }
