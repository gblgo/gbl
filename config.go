// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import "time"

// Config is the engine's runtime configuration, set entirely in code. The
// kernel reads no configuration file: defaults apply as-is, and Configure
// overlays only the fields the application touches. File-based configuration
// (YAML, environment, profiles, config centers) belongs to an integration
// layer that resolves its sources and calls Configure like any other caller.
//
// useTLS is derived, not set directly: it turns on when both CertFile and
// KeyFile are present.
type Config struct {
	// application name
	Name string
	// listen port, without the leading colon
	Port string
	// TLS certificate file path
	CertFile string
	// TLS private key file path
	KeyFile string
	// max duration to read an entire request
	ReadTimeout time.Duration
	// max duration to read request headers
	ReadHeaderTimeout time.Duration
	// max duration to write an entire response
	WriteTimeout time.Duration
	// max idle wait before a keep-alive connection closes
	IdleTimeout time.Duration
	// derived: true when both CertFile and KeyFile are set
	useTLS bool
}

// defaultConfig returns the baseline every engine starts from: name "gbl" on
// port 9801 with the standard server timeouts.
func defaultConfig() *Config {
	return &Config{
		Name:              "gbl",
		Port:              "9801",
		ReadTimeout:       10 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       time.Minute,
		useTLS:            false,
	}
}

// Configure overlays the default engine's configuration in code. The function
// receives the live Config already filled with defaults, so it only assigns
// the fields it cares about:
//
//	gbl.Configure(func(c *gbl.Config) {
//	    c.Port = "8080"
//	    c.ReadTimeout = 5 * time.Second
//	})
//
// Call it before Run. Passing nil is a no-op.
func Configure(fn func(*Config)) { defaultEngine.configure(fn) }

// configure overlays this engine's configuration. After fn returns, useTLS is
// re-derived from the certificate and key paths, so setting or clearing either
// path takes effect without a separate switch. Once the engine runs its
// configuration snapshot is in effect on the live server and re-Configure
// panics: it could no longer change what is already serving.
func (e *engine) configure(fn func(*Config)) {
	e.assertAssembling("Configure")
	if fn != nil {
		fn(e.cfg)
		e.cfg.useTLS = e.cfg.CertFile != empty && e.cfg.KeyFile != empty
	}
}
