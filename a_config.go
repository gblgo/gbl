// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import "time"

// Config is the engine runtime configuration, set entirely in code. The
// kernel reads no configuration file: defaults apply as-is, and Configure
// overlays only the fields the application touches. File-based configuration
// (YAML, environment, profiles, config centers) belongs to an integration
// layer that resolves its sources and calls Configure like any other caller.
//
// useTLS is derived, not set directly: CertFile and KeyFile must both be set
// or both be empty.
type Config struct {
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

// defaultConfig returns the baseline every engine starts from: port 9801 with
// the standard server timeouts.
func defaultConfig() *Config {
	return &Config{
		Port:              "9801",
		ReadTimeout:       10 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       time.Minute,
		useTLS:            false,
	}
}

// Configure overlays the default engine configuration in code. The function
// receives a copy of the current configuration snapshot, already filled with
// defaults, so it only assigns the fields it cares about:
//
//	gbl.Configure(func(c *gbl.Config) {
//	    c.Port = "8080"
//	    c.ReadTimeout = 5 * time.Second
//	})
//
// Call it before Run. Passing nil is a no-op.
func Configure(fn func(*Config)) { defaultEngine.configure(fn) }

// mergeConfigChanges applies only fields changed since the snapshot, preserving
// independent updates published by an overlapping or nested Configure call.
func mergeConfigChanges(dst, base, changed *Config) {
	if changed.Port != base.Port {
		dst.Port = changed.Port
	}
	if changed.CertFile != base.CertFile {
		dst.CertFile = changed.CertFile
	}
	if changed.KeyFile != base.KeyFile {
		dst.KeyFile = changed.KeyFile
	}
	if changed.ReadTimeout != base.ReadTimeout {
		dst.ReadTimeout = changed.ReadTimeout
	}
	if changed.ReadHeaderTimeout != base.ReadHeaderTimeout {
		dst.ReadHeaderTimeout = changed.ReadHeaderTimeout
	}
	if changed.WriteTimeout != base.WriteTimeout {
		dst.WriteTimeout = changed.WriteTimeout
	}
	if changed.IdleTimeout != base.IdleTimeout {
		dst.IdleTimeout = changed.IdleTimeout
	}
}

// configure overlays this engine configuration. After fn returns, useTLS is
// re-derived from the certificate and key paths, so setting or clearing either
// path takes effect without a separate switch. Once Run is claimed the
// configuration is frozen and re-Configure panics.
func (e *engine) configure(fn func(*Config)) {
	e.assemblyMu.Lock()
	if e.runState.Load() != runStateIdle {
		e.assemblyMu.Unlock()
		e.failAssembling("Configure")
		return
	}
	next := *e.cfg
	base := next
	baseVersion := e.configVersion
	e.assemblyMu.Unlock()

	if fn != nil {
		callbackCfg := next
		fn(&callbackCfg)
		next = callbackCfg
	}
	e.assemblyMu.Lock()
	if e.runState.Load() != runStateIdle {
		e.assemblyMu.Unlock()
		e.failAssembling("Configure")
		return
	}
	latest := next
	if e.configVersion != baseVersion {
		latest = *e.cfg
		mergeConfigChanges(&latest, &base, &next)
	}
	if (latest.CertFile == empty) != (latest.KeyFile == empty) {
		e.assemblyMu.Unlock()
		e.failStartup("gbl: CertFile and KeyFile must both be set or both be empty")
		return
	}
	latest.useTLS = latest.CertFile != empty && latest.KeyFile != empty
	e.cfg = &latest
	e.configVersion++
	e.assemblyMu.Unlock()
}
