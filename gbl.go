// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"fmt"
	"net"
	"net/http"
	"os"
)

func Run() {

	// Configuration initialization
	var cfg *gblConfig

	// Read the application.ini configuration file in the project's root directory
	ini, err := ReadIni("application.ini")
	if err != nil {
		if os.IsNotExist(err) {
			cfg = defaultConfig()
		} else {
			panic(err)
		}
	} else {
		// Save global parameters
		iniConfigData = ini
		// Generate configuration
		cfg, err = generateConfig(ini)
		if err != nil {
			panic(err)
		}
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

	// Determine whether to enable TSL
	if cfg.useTSL {
		err = server.ServeTLS(ln, cfg.CertFile, cfg.KeyFile)
	} else {
		err = server.Serve(ln)
	}

	if err != nil {
		panic(err)
	}
}
