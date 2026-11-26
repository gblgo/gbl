// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"testing"
	"time"
)

func configTestPanicValue(fn func()) (recovered any) {
	defer func() { recovered = recover() }()
	fn()
	return nil
}

func TestConfigDefaultsAndIndependentCopies(t *testing.T) {
	want := Config{
		Port:              "9801",
		ReadTimeout:       10 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       time.Minute,
		useTLS:            false,
	}
	first := defaultConfig()
	second := defaultConfig()
	if first == nil || second == nil {
		t.Fatal("defaultConfig returned nil")
	}
	if *first != want || *second != want {
		t.Fatalf("defaultConfig() = %#v and %#v, want %#v", first, second, want)
	}
	if first == second {
		t.Fatal("defaultConfig calls share the same Config pointer")
	}
	first.Port = "changed"
	first.ReadTimeout = time.Nanosecond
	if *second != want {
		t.Fatalf("mutating one default changed another: %#v", second)
	}
}

func TestEngineConfigureOverlaysAndCopies(t *testing.T) {
	e := newEngine()
	defer e.cancelStreams()
	original := e.cfg
	originalValue := *original
	var callbackSnapshot *Config

	e.configure(func(c *Config) {
		callbackSnapshot = c
		if *c != originalValue {
			t.Fatalf("callback did not receive the current configuration: %#v", c)
		}
		if e.cfg != original || *e.cfg != originalValue {
			t.Fatal("callback mutation changed the live configuration before configure returned")
		}
		c.Port = "8080"
		c.ReadTimeout = 2 * time.Second
		c.WriteTimeout = 0
		c.CertFile = "server.crt"
		c.KeyFile = "server.key"
		c.useTLS = false
	})

	want := Config{
		Port:              "8080",
		CertFile:          "server.crt",
		KeyFile:           "server.key",
		ReadTimeout:       2 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      0,
		IdleTimeout:       time.Minute,
		useTLS:            true,
	}
	if e.cfg == original {
		t.Fatal("configure retained the old Config pointer instead of publishing a copy")
	}
	if *e.cfg != want {
		t.Fatalf("configured Config = %#v, want %#v", e.cfg, want)
	}
	if *original != originalValue {
		t.Fatalf("configure mutated the previous snapshot: %#v", original)
	}
	if callbackSnapshot == nil {
		t.Fatal("configure did not invoke the callback")
	}
	callbackSnapshot.Port = "mutated-after-return"
	if e.cfg.Port != "8080" {
		t.Fatalf("retained callback pointer changed the published snapshot: %#v", e.cfg)
	}
}

func TestEngineConfigureNilCopiesCurrentSnapshot(t *testing.T) {
	e := newEngine()
	defer e.cancelStreams()
	e.configure(func(c *Config) { c.Port = "9090" })
	before := e.cfg
	beforeValue := *before

	e.configure(nil)
	if e.cfg == before {
		t.Fatal("configure(nil) reused the mutable Config pointer")
	}
	if *e.cfg != beforeValue {
		t.Fatalf("configure(nil) changed values: got %#v, want %#v", e.cfg, beforeValue)
	}
}

func TestEngineConfigureDerivesTLS(t *testing.T) {
	tests := []struct {
		name       string
		configure  func(*Config)
		wantCert   string
		wantKey    string
		wantUseTLS bool
	}{
		{name: "empty paths disable TLS", configure: func(c *Config) { c.useTLS = true }},
		{name: "paired paths enable TLS", configure: func(c *Config) {
			c.CertFile = "cert.pem"
			c.KeyFile = "key.pem"
			c.useTLS = false
		}, wantCert: "cert.pem", wantKey: "key.pem", wantUseTLS: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEngine()
			defer e.cancelStreams()
			e.configure(tt.configure)
			if e.cfg.CertFile != tt.wantCert || e.cfg.KeyFile != tt.wantKey || e.cfg.useTLS != tt.wantUseTLS {
				t.Fatalf("TLS fields = (%q, %q, %t), want (%q, %q, %t)",
					e.cfg.CertFile, e.cfg.KeyFile, e.cfg.useTLS, tt.wantCert, tt.wantKey, tt.wantUseTLS)
			}
		})
	}
}

func TestEngineConfigureRejectsUnpairedTLSWithoutPublishing(t *testing.T) {
	tests := []struct {
		name        string
		startPaired bool
		configure   func(*Config)
	}{
		{name: "certificate only", configure: func(c *Config) { c.CertFile = "cert.pem" }},
		{name: "key only", configure: func(c *Config) { c.KeyFile = "key.pem" }},
		{name: "clearing only certificate", startPaired: true, configure: func(c *Config) {
			c.CertFile = ""
			c.KeyFile = "key.pem"
		}},
		{name: "clearing only key", startPaired: true, configure: func(c *Config) {
			c.CertFile = "cert.pem"
			c.KeyFile = ""
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEngine()
			defer e.cancelStreams()
			if tt.startPaired {
				e.configure(func(c *Config) {
					c.CertFile = "old-cert.pem"
					c.KeyFile = "old-key.pem"
				})
			}
			capture := &logTestCaptureLogger{}
			e.setLogger(capture)
			before := e.cfg
			beforeValue := *before

			recovered := configTestPanicValue(func() { e.configure(tt.configure) })
			const reason = "gbl: CertFile and KeyFile must both be set or both be empty"
			if recovered != reason {
				t.Fatalf("configure panic = %#v, want %q", recovered, reason)
			}
			if e.cfg != before || *e.cfg != beforeValue {
				t.Fatalf("failed configure published a partial snapshot: %#v", e.cfg)
			}
			if len(capture.records) != 1 {
				t.Fatalf("failed configure logged %d records, want 1", len(capture.records))
			}
			record := capture.records[0]
			if record.level != "ERROR" || record.msg != "startup aborted" || len(record.kv) != 2 || record.kv[0] != "reason" || record.kv[1] != reason {
				t.Fatalf("failed configure log record = %#v", record)
			}
		})
	}
}

func TestEngineConfigureCanClearTLS(t *testing.T) {
	e := newEngine()
	defer e.cancelStreams()
	e.configure(func(c *Config) {
		c.CertFile = "cert.pem"
		c.KeyFile = "key.pem"
	})
	e.configure(func(c *Config) {
		c.CertFile = ""
		c.KeyFile = ""
	})
	if e.cfg.useTLS || e.cfg.CertFile != "" || e.cfg.KeyFile != "" {
		t.Fatalf("clearing TLS paths left TLS state behind: %#v", e.cfg)
	}
}

func TestConfigureUsesDefaultEngine(t *testing.T) {
	previous := defaultEngine.cfg
	previousGenerated := defaultEngine.generated
	defaultEngine.generated = false
	t.Cleanup(func() {
		defaultEngine.cfg = previous
		defaultEngine.generated = previousGenerated
	})

	Configure(func(c *Config) {
		c.Port = "7070"
		c.ReadHeaderTimeout = 3 * time.Second
	})
	Configure(nil)
	if defaultEngine.cfg.Port != "7070" || defaultEngine.cfg.ReadHeaderTimeout != 3*time.Second {
		t.Fatalf("Configure did not overlay the default engine: %#v", defaultEngine.cfg)
	}
	if defaultEngine.cfg.ReadTimeout != 10*time.Second || defaultEngine.cfg.WriteTimeout != 10*time.Second || defaultEngine.cfg.IdleTimeout != time.Minute {
		t.Fatalf("Configure changed untouched defaults: %#v", defaultEngine.cfg)
	}
}

func TestConfigureIsFrozenAfterGeneration(t *testing.T) {
	previous := defaultEngine.cfg
	previousGenerated := defaultEngine.generated
	defaultEngine.generated = true
	t.Cleanup(func() {
		defaultEngine.cfg = previous
		defaultEngine.generated = previousGenerated
	})

	recovered := configTestPanicValue(func() { Configure(func(*Config) {}) })
	if recovered != "gbl.Configure must be called before Run; the engine is read-only while serving" {
		t.Fatalf("Configure panic = %#v", recovered)
	}
	if defaultEngine.cfg != previous {
		t.Fatal("rejected Configure changed the configuration")
	}
}
