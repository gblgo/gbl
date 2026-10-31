// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
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
		// Build configuration
		cfg, err = generateConfig(ini)
		if err != nil {
			panic(err)
		}
	}

	// Concatenate address
	addr := ":" + cfg.Port
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

	handle := &gblHandler{}

	server := &http.Server{
		Addr:              addr,
		Handler:           handle,
		ReadTimeout:       cfg.ReadTimeout,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
	}

	err = server.Serve(ln)

	if err != nil {
		panic(err)
	}
}
