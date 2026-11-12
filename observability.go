// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"time"
)

// AccessLog is a ready-made access-log middleware. Install it with
// e.Use(gbl.AccessLog) (or gbl.Use) to record one line per request after the
// chain completes: method, path, status, response size and how long it took.
//
// It writes through the engine's Logger, so the destination and format follow
// SetLogger: the default prints one plain line per request, and a structured
// backend receives the same fields as key/value pairs. Richer observability —
// metrics, tracing — is a middleware too: read c.StatusCode(), c.Size() and the
// elapsed time around c.Next() the same way, so the framework stays free of
// hard-coded lifecycle hooks and of any transport dependency.
func AccessLog(c *Context) {
	start := time.Now()
	c.Next()
	c.logger.Info("access",
		"method", c.Request.Method,
		"path", c.Request.URL.Path,
		"status", c.StatusCode(),
		"size", c.Size(),
		"duration", time.Since(start),
	)
}
