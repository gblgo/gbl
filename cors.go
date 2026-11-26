// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Constants used only by this file: the CORS response headers the spec names
// literally, and the fixed preflight rejection reasons.
const (
	// the only value credentials headers understand
	headerTrue = "true"
	// request header naming the cross-origin caller
	headerOrigin = "Origin"
	// preflight: method the real call will use
	accessControlRequestMethod = "Access-Control-Request-Method"
	// response: origin the browser may read from
	accessControlAllowOrigin = "Access-Control-Allow-Origin"
	// response: cookies and auth may accompany the call
	accessControlAllowCredentials = "Access-Control-Allow-Credentials"
	// response: extra headers the client may read
	accessControlExposeHeaders = "Access-Control-Expose-Headers"
	// preflight: headers the real call will send
	accessControlRequestHeaders = "Access-Control-Request-Headers"
	// response: methods permitted cross-origin
	accessControlAllowMethods = "Access-Control-Allow-Methods"
	// response: headers accepted cross-origin
	accessControlAllowHeaders = "Access-Control-Allow-Headers"
	// response: preflight cache lifetime, in seconds
	accessControlMaxAge = "Access-Control-Max-Age"
	// rejection reason: origin outside AllowedOrigins
	corsOriginNotAllowed = "CORS origin not allowed"
	// rejection reason: method outside AllowedMethods
	corsMethodNotAllowed = "CORS method not allowed"
	// rejection reason: header outside AllowedHeaders
	corsHeaderNotAllowed = "CORS header not allowed"
	// lists up to this size are scanned with EqualFold to avoid lowercasing allocations
	corsLinearScanLimit = 8
	// maximum accepted Access-Control-Request-Headers tokens per preflight
	corsMaxPreflightHeaders = 128
)

// CorsConfig describes the cross-origin policy applied by the CORS layer.
// All fields are exported: build one with a struct literal and install it
// with SetCorsConfig (nil disables CORS).
type CorsConfig struct {
	// origins accepted via Access-Control-Allow-Origin, or "*"
	AllowedOrigins []string
	// methods permitted for cross-origin requests
	AllowedMethods []string
	// request headers accepted in a preflight
	AllowedHeaders []string
	// response headers readable by the cross-origin client
	ExposedHeaders []string
	// whether cookies/auth may accompany cross-origin calls
	AllowCredentials bool
	// how long a client may cache the preflight result
	MaxAge time.Duration
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
				return errors.New("gbl: invalid CORS config: AllowCredentials cannot be " +
					"true while AllowedOrigins contains \"*\"; list the explicit origins " +
					"allowed to send credentials instead")
			}
		}
	}
	return nil
}

// SetCorsConfig installs the CORS configuration on the default engine; Run
// then wraps the handler chain with it. A nil configuration disables CORS.
// The engine keeps a private deep copy, so later edits to the caller struct
// can neither race the serving goroutines nor switch policy mid-flight.
// A configuration rejected by this security policy (see validate) aborts
// startup here, before Run. Like every assembling API it panics once the
// engine is serving: the handler chain has already been built.
func SetCorsConfig(cc *CorsConfig) {
	defaultEngine.useCors(cc)
}

// useCors installs the CORS configuration on this engine.
func (e *engine) useCors(cc *CorsConfig) {
	e.assertAssembling("SetCorsConfig")
	if cc != nil {
		if err := cc.validate(); err != nil {
			e.failStartup(err)
		}
		owned := *cc
		owned.AllowedOrigins = slices.Clone(cc.AllowedOrigins)
		owned.AllowedMethods = slices.Clone(cc.AllowedMethods)
		for i := range owned.AllowedMethods {
			owned.AllowedMethods[i] = strings.ToUpper(strings.TrimSpace(owned.AllowedMethods[i]))
		}
		owned.AllowedHeaders = slices.Clone(cc.AllowedHeaders)
		owned.ExposedHeaders = slices.Clone(cc.ExposedHeaders)
		cc = &owned
	}
	e.cors = cc
}

// isPreflightRequest reports whether r is a CORS preflight request.
func isPreflightRequest(r *http.Request) bool {
	return r.Method == http.MethodOptions && r.Header.Get(accessControlRequestMethod) != empty
}

// corsHandler is the http.Handler inserted outside the engine when a CorsConfig
// is present; it decides CORS headers and preflight handling, then delegates the
// actual request to the wrapped engine.
type corsHandler struct {
	// engine receives requests that are not answered as preflights
	engine *engine
	// config is the immutable-installed policy
	config *CorsConfig
	// allowMethods is the preflight Access-Control-Allow-Methods value
	allowMethods string
	// allowHeaders is the preflight Access-Control-Allow-Headers value
	allowHeaders string
	// exposeHeaders is the Access-Control-Expose-Headers value
	exposeHeaders string
	// maxAge is the preflight cache lifetime in seconds
	maxAge string
	// wildcard records whether any allowed origin is "*"
	wildcard bool
	// expose records whether response headers should be exposed
	expose bool
	// allowedOriginsMap holds lowercased origins for large lists
	allowedOriginsMap map[string]struct{}
	// allowedMethodsMap holds lowercased methods for large lists
	allowedMethodsMap map[string]struct{}
	// allowedHeadersMap holds lowercased headers for large lists
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
	var originsMap map[string]struct{}
	if len(cc.AllowedOrigins) > corsLinearScanLimit {
		originsMap = make(map[string]struct{}, len(cc.AllowedOrigins))
	}
	for _, origin := range cc.AllowedOrigins {
		lower := strings.ToLower(origin)
		if originsMap != nil {
			originsMap[lower] = struct{}{}
		}
		if lower == asterisk {
			wildcard = true
		}
	}
	var methodsMap map[string]struct{}
	if len(cc.AllowedMethods) > corsLinearScanLimit {
		methodsMap = make(map[string]struct{}, len(cc.AllowedMethods))
		for _, method := range cc.AllowedMethods {
			methodsMap[strings.ToLower(method)] = struct{}{}
		}
	}
	var headersMap map[string]struct{}
	if len(cc.AllowedHeaders) > corsLinearScanLimit {
		headersMap = make(map[string]struct{}, len(cc.AllowedHeaders))
		for _, header := range cc.AllowedHeaders {
			headersMap[strings.ToLower(header)] = struct{}{}
		}
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

// containsFold reports whether target matches a small allow-list value,
// case-insensitively, without allocating a lowercased lookup key.
func containsFold(values []string, target string) bool {
	for _, allowed := range values {
		if strings.EqualFold(allowed, target) {
			return true
		}
	}
	return false
}

// containsInMap reports whether target matches a large precomputed allow-list
// map. Already-normalized keys need no copy; short ASCII keys are lowered in a
// small stack buffer, while longer or non-ASCII keys use Unicode-aware lowering.
func containsInMap(m map[string]struct{}, target string) bool {
	if _, ok := m[target]; ok {
		return true
	}

	var lowered [64]byte
	if len(target) > len(lowered) {
		_, ok := m[strings.ToLower(target)]
		return ok
	}
	for i := range len(target) {
		c := target[i]
		if c >= 0x80 {
			_, ok := m[strings.ToLower(target)]
			return ok
		}
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		lowered[i] = c
	}
	_, ok := m[string(lowered[:len(target)])]
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
	var allowed bool
	if len(ch.config.AllowedOrigins) <= corsLinearScanLimit {
		allowed = containsFold(ch.config.AllowedOrigins, origin)
	} else {
		allowed = containsInMap(ch.allowedOriginsMap, origin)
	}
	if allowed {
		return origin, true
	}
	return empty, false
}

// handlePreflight answers a CORS preflight request. ServeHTTP has already added
// the request-method and request-header Vary fields before any rejection branch.
func (ch *corsHandler) handlePreflight(w http.ResponseWriter, r *http.Request) {
	header := w.Header()
	requestMethod := r.Header.Get(accessControlRequestMethod)

	var methodAllowed bool
	if len(ch.config.AllowedMethods) <= corsLinearScanLimit {
		methodAllowed = containsFold(ch.config.AllowedMethods, requestMethod)
	} else {
		methodAllowed = containsInMap(ch.allowedMethodsMap, requestMethod)
	}
	if !methodAllowed {
		http.Error(w, corsMethodNotAllowed, http.StatusForbidden)
		return
	}

	requestHeaders := r.Header.Get(accessControlRequestHeaders)
	if requestHeaders != empty {
		count := 0
		for h := range strings.SplitSeq(requestHeaders, comma) {
			count++
			if count > corsMaxPreflightHeaders {
				http.Error(w, corsHeaderNotAllowed, http.StatusForbidden)
				return
			}
			h = strings.TrimSpace(h)
			if h == empty {
				continue
			}
			var headerAllowed bool
			if len(ch.config.AllowedHeaders) <= corsLinearScanLimit {
				headerAllowed = containsFold(ch.config.AllowedHeaders, h)
			} else {
				headerAllowed = containsInMap(ch.allowedHeadersMap, h)
			}
			if !headerAllowed {
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
	w.WriteHeader(http.StatusNoContent)
}

// ServeHTTP applies the CORS policy to a request: non-cross-origin requests and
// disallowed-but-safe ones pass through to the engine unchanged, allowed
// requests gain the CORS response headers, and a preflight is answered directly.
func (ch *corsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	origin := strings.TrimSpace(r.Header.Get(headerOrigin))
	header := w.Header()
	if !ch.wildcard || ch.config.AllowCredentials {
		header.Add(vary, headerOrigin)
	}

	// Mark every preflight response before any origin, method or header check
	// can reject it, so caches never reuse a denial for different inputs.
	preflight := isPreflightRequest(r)
	if preflight {
		header.Add(vary, accessControlRequestMethod)
		header.Add(vary, accessControlRequestHeaders)
	}

	// Non-cross-origin request
	if origin == empty {
		ch.engine.serveHTTP(w, r)
		return
	}

	// Match Origin
	allowOrigin, allowed := ch.matchOrigin(origin)

	// Not allowed origin
	if !allowed {
		if preflight {
			http.Error(w, corsOriginNotAllowed, http.StatusForbidden)
			return
		}
		ch.engine.serveHTTP(w, r)
		return
	}

	// Set general cross-origin response headers
	header.Set(accessControlAllowOrigin, allowOrigin)

	if ch.config.AllowCredentials {
		header.Set(accessControlAllowCredentials, headerTrue)
	}

	if ch.expose {
		header.Set(accessControlExposeHeaders, ch.exposeHeaders)
	}

	// Handle preflight request
	if preflight {
		ch.handlePreflight(w, r)
		return
	}

	ch.engine.serveHTTP(w, r)
}
