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
// startup here, before Run. Like every assembling API it panics once the
// engine is serving: the handler chain has already been built.
func SetCorsConfig(cc *CorsConfig) {
	defaultEngine.useCors(cc)
}

// useCors installs the CORS configuration on this engine.
func (e *engine) useCors(cc *CorsConfig) *engine {
	e.assertAssembling("SetCorsConfig")
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
	return r.Method == http.MethodOptions && r.Header.Get(accessControlRequestMethod) != empty
}

// corsHandler is the http.Handler inserted outside the engine when a CorsConfig
// is present; it decides CORS headers and preflight handling, then delegates the
// actual request to the wrapped engine.
type corsHandler struct {
	engine *engine
	config *CorsConfig
	allowMethods  string
	allowHeaders  string
	exposeHeaders string
	maxAge        string
	wildcard bool
	expose   bool
	allowedOriginsMap map[string]struct{}
	allowedMethodsMap map[string]struct{}
	allowedHeadersMap map[string]struct{}
}

// newCorsHandler wires the CORS layer over e, precomputing the header values and
// allow-list maps that stay constant for the lifetime of the configuration.
func newCorsHandler(e *engine, cc *CorsConfig) *corsHandler {
	var maxAge string
	if cc.MaxAge > 0 {
		maxAge = strconv.FormatInt(int64(cc.MaxAge.Seconds()), 10)
	}
	var wildcard bool
	originsMap := make(map[string]struct{}, len(cc.AllowedOrigins))
	for _, origin := range cc.AllowedOrigins {
		lower := strings.ToLower(origin)
		originsMap[lower] = struct{}{}
		if lower == asterisk {
			wildcard = true
		}
	}
	methodsMap := make(map[string]struct{}, len(cc.AllowedMethods))
	for _, m := range cc.AllowedMethods {
		methodsMap[strings.ToLower(m)] = struct{}{}
	}
	headersMap := make(map[string]struct{}, len(cc.AllowedHeaders))
	for _, h := range cc.AllowedHeaders {
		headersMap[strings.ToLower(h)] = struct{}{}
	}
	return &corsHandler{
		engine:            e,
		config:            cc,
		allowMethods:      strings.Join(cc.AllowedMethods, headerComma),
		allowHeaders:      strings.Join(cc.AllowedHeaders, headerComma),
		exposeHeaders:     strings.Join(cc.ExposedHeaders, headerComma),
		maxAge:            maxAge,
		wildcard:          wildcard,
		expose:            len(cc.ExposedHeaders) > 0,
		allowedOriginsMap: originsMap,
		allowedMethodsMap: methodsMap,
		allowedHeadersMap: headersMap,
	}
}

// containsInMap reports whether target matches any key in the precomputed map,
// case-insensitively.
func containsInMap(m map[string]struct{}, target string) bool {
	_, ok := m[strings.ToLower(target)]
	return ok
}

// matchOrigin resolves the Access-Control-Allow-Origin value for an origin and
// reports whether it is permitted. A "*" wildcard yields the literal "*" (unless
// credentials are enabled, which forbids wildcards); an allow-listed origin is
// echoed back verbatim.
func (ch *corsHandler) matchOrigin(origin string) (string, bool) {
	if ch.wildcard {
		if ch.config.AllowCredentials {
			return empty, false
		}
		return asterisk, true
	}
	if containsInMap(ch.allowedOriginsMap, origin) {
		return origin, true
	}
	return empty, false
}

// handlePreflight answers a CORS preflight request.
func (ch *corsHandler) handlePreflight(w http.ResponseWriter, r *http.Request) {
	header := w.Header()
	requestMethod := r.Header.Get(accessControlRequestMethod)

	if !containsInMap(ch.allowedMethodsMap, requestMethod) {
		http.Error(w, corsMethodNotAllowed, http.StatusForbidden)
		return
	}

	requestHeaders := r.Header.Get(accessControlRequestHeaders)
	if requestHeaders != empty {
		for h := range strings.SplitSeq(requestHeaders, comma) {
			h = strings.TrimSpace(h)
			if h == empty {
				continue
			}
			if !containsInMap(ch.allowedHeadersMap, h) {
				http.Error(w, corsHeaderNotAllowed, http.StatusForbidden)
				return
			}
		}
	}

	header.Set(accessControlAllowMethods, ch.allowMethods)
	header.Set(accessControlAllowHeaders, ch.allowHeaders)
	if ch.maxAge != empty {
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
	if origin == empty {
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
