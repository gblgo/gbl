// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// CorsConfig describes the cross-origin policy applied by the CORS layer.
// All fields are exported: build one with a struct literal and install it
// with SetCorsConfig (nil disables CORS).
type CorsConfig struct {
	AllowedOrigins   []string      // origins accepted via Access-Control-Allow-Origin, or "*"
	AllowedMethods   []string      // methods permitted for cross-origin requests
	AllowedHeaders   []string      // request headers accepted in a preflight
	ExposedHeaders   []string      // response headers readable by the cross-origin client
	AllowCredentials bool          // whether cookies/auth may accompany cross-origin calls
	MaxAge           time.Duration // how long a client may cache the preflight result
}

// validate enforces the CORS security policy at wiring time, so an unsafe
// configuration can never reach Run and silently weaken every response. The
// CORS spec forbids Access-Control-Allow-Credentials: true together with a
// wildcard Access-Control-Allow-Origin: "*", because that would let any site
// make credentialed (cookie/auth-bearing) cross-origin calls. Such a config is
// a mistake, not a runtime condition, so it is reported as an error that the
// caller turns into a logged startup abort instead of being quietly downgraded
// per request.
func (cc *CorsConfig) validate() error {
	if cc.AllowCredentials {
		for _, origin := range cc.AllowedOrigins {
			if strings.EqualFold(strings.TrimSpace(origin), asterisk) {
				return errors.New("gbl: invalid CORS config: AllowCredentials cannot be true while AllowedOrigins contains \"*\"; list the explicit origins allowed to send credentials instead")
			}
		}
	}
	return nil
}

// SetCorsConfig installs the CORS configuration on the default engine; Run
// then wraps the handler chain with it. A nil configuration disables CORS.
// A configuration rejected by this security policy (see validate) aborts
// startup here, before Run.
func SetCorsConfig(cc *CorsConfig) {
	if cc != nil {
		if err := cc.validate(); err != nil {
			defaultEngine.failStartup(err)
		}
	}
	defaultEngine.cors = cc
}

// UseCors installs the CORS configuration on this engine.
func (e *engine) UseCors(cc *CorsConfig) *engine {
	if cc != nil {
		if err := cc.validate(); err != nil {
			e.failStartup(err)
		}
	}
	e.cors = cc
	return e
}

// isPreflightRequest reports whether r is a CORS preflight request.
func isPreflightRequest(r *http.Request) bool {
	return r.Method == http.MethodOptions && r.Header.Get(accessControlRequestMethod) != null
}

// corsHandler is the http.Handler inserted outside the engine when a CorsConfig
// is present; it decides CORS headers and preflight handling, then delegates the
// actual request to the wrapped engine.
type corsHandler struct {
	engine *engine
	config *CorsConfig
	// Fixed header values precomputed once at wiring time, so the request path
	// never re-joins or re-formats static configuration per response or preflight.
	allowMethods  string
	allowHeaders  string
	exposeHeaders string
	maxAge        string
	// wildcard records whether AllowedOrigins contains "*", and expose whether
	// any ExposedHeaders are configured. Both are resolved once here so the
	// request path never rescans the origin list or rechecks the slice length.
	wildcard bool
	expose   bool
}

// newCorsHandler wires the CORS layer over e, precomputing the header values that
// stay constant for the lifetime of the configuration.
func newCorsHandler(e *engine, cc *CorsConfig) *corsHandler {
	var maxAge string
	if cc.MaxAge > 0 {
		maxAge = strconv.FormatInt(int64(cc.MaxAge.Seconds()), 10)
	}
	var wildcard bool
	for _, origin := range cc.AllowedOrigins {
		if strings.EqualFold(origin, asterisk) {
			wildcard = true
			break
		}
	}
	return &corsHandler{
		engine:        e,
		config:        cc,
		allowMethods:  strings.Join(cc.AllowedMethods, headerComma),
		allowHeaders:  strings.Join(cc.AllowedHeaders, headerComma),
		exposeHeaders: strings.Join(cc.ExposedHeaders, headerComma),
		maxAge:        maxAge,
		wildcard:      wildcard,
		expose:        len(cc.ExposedHeaders) > 0,
	}
}

// contains reports whether target matches any value, case-insensitively. It
// backs the origin/method/header allow-lists.
func (ch *corsHandler) contains(values []string, target string) bool {
	for _, value := range values {
		if strings.EqualFold(value, target) {
			return true
		}
	}
	return false
}

// matchOrigin resolves the Access-Control-Allow-Origin value for an origin and
// reports whether it is permitted. A "*" wildcard yields the literal "*" (unless
// credentials are enabled, which forbids wildcards); an allow-listed origin is
// echoed back verbatim.
func (ch *corsHandler) matchOrigin(origin string) (string, bool) {
	if ch.wildcard {
		if ch.config.AllowCredentials {
			return null, false
		}
		// The literal "*" must reach the browser: an empty
		// Access-Control-Allow-Origin value fails the CORS check exactly like a
		// missing one, so returning "" here silently rejected every cross-origin
		// response.
		return asterisk, true
	}

	// Compared case-insensitively, consistent with the wildcard check above.
	if ch.contains(ch.config.AllowedOrigins, origin) {
		return origin, true
	}

	return null, false
}

// handlePreflight answers a CORS preflight request.
func (ch *corsHandler) handlePreflight(w http.ResponseWriter, r *http.Request) {

	header := w.Header()
	// The method the browser intends to use.
	requestMethod := r.Header.Get(accessControlRequestMethod)

	if !ch.contains(ch.config.AllowedMethods, requestMethod) {
		http.Error(w, corsMethodNotAllowed, http.StatusForbidden)
		return
	}

	// Get the headers of the browser request
	requestHeaders := r.Header.Get(accessControlRequestHeaders)

	if requestHeaders != null {
		for h := range strings.SplitSeq(requestHeaders, comma) {
			h = strings.TrimSpace(h)

			if h == null {
				continue
			}

			if !ch.contains(ch.config.AllowedHeaders, h) {
				http.Error(w, corsHeaderNotAllowed, http.StatusForbidden)
				return
			}
		}
	}

	// Set preflight response headers
	header.Set(accessControlAllowMethods, ch.allowMethods)

	header.Set(accessControlAllowHeaders, ch.allowHeaders)

	if ch.maxAge != null {
		header.Set(accessControlMaxAge, ch.maxAge)
	}

	header.Add(vary, accessControlRequestMethod)
	header.Add(vary, accessControlRequestHeaders)

	w.WriteHeader(http.StatusNoContent)
}

// ServeHTTP applies the CORS policy to a request: non-cross-origin requests and
// disallowed-but-safe ones pass through to the engine unchanged, allowed
// requests gain the CORS response headers, and a preflight is answered directly.
func (ch *corsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {

	origin := strings.TrimSpace(r.Header.Get(headerOrigin))

	// Non-cross-origin request
	if origin == null {
		ch.engine.serveHTTP(w, r)
		return
	}

	// Match Origin
	allowOrigin, allowed := ch.matchOrigin(origin)

	// Not allowed origin
	if !allowed {
		if isPreflightRequest(r) {
			http.Error(w, corsOriginNotAllowed, http.StatusForbidden)
			return
		}
		ch.engine.serveHTTP(w, r)
		return
	}

	// Set general cross-origin response headers
	header := w.Header()
	header.Set(accessControlAllowOrigin, allowOrigin)
	header.Add(vary, headerOrigin)

	if ch.config.AllowCredentials {
		header.Set(accessControlAllowCredentials, headerTrue)
	}

	if ch.expose {
		header.Set(accessControlExposeHeaders, ch.exposeHeaders)
	}

	// Handle preflight request
	if isPreflightRequest(r) {
		ch.handlePreflight(w, r)
		return
	}

	ch.engine.serveHTTP(w, r)
}
