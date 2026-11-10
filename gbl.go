// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"fmt"
	"net"
	"net/http"
)

func Run() {

	// Configuration initialization
	//
	// The snapshot is shared with IniBySection and is loaded on first access,
	// so registration code may already have pinned it. A missing application.ini
	// yields an empty snapshot, which generateConfig resolves to pure defaults.
	cfg, err := generateConfig(iniSnapshot())
	if err != nil {
		panic(err)
	}

	// Concatenate address
	addr := colon + cfg.Port
	ln, err := net.Listen("tcp", addr)

	if err != nil {
		panic(err)
	}

	defer func(ln net.Listener) {
		err = ln.Close()
		if err != nil {
			panic(err)
		}
	}(ln)

	// CORS
	var handler http.Handler
	if globalCorsConfig == nil {
		handler = &defaultHandler{}
	} else {
		handler = &corsHandler{config: globalCorsConfig}
	}

	server := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadTimeout:       cfg.ReadTimeout,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
	}

	generateRoute()

	fmt.Printf("gbl server listening on %s\n", addr)

	// Determine whether to enable TLS
	if cfg.useTLS {
		err = server.ServeTLS(ln, cfg.CertFile, cfg.KeyFile)
	} else {
		err = server.Serve(ln)
	}

	if err != nil {
		panic(err)
	}
}
