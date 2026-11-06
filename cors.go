// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

var globalCorsConfig *CorsConfig = nil

type CorsConfig struct {
	allowedOrigins   []string
	allowedMethods   []string
	allowedHeaders   []string
	exposedHeaders   []string
	allowCredentials bool
	maxAge           time.Duration
}

func SetCorsConfig(cc *CorsConfig) {
	globalCorsConfig = cc
}

// isPreflightRequest Determine whether it is a preflight request
func isPreflightRequest(r *http.Request) bool {
	return r.Method == http.MethodOptions && r.Header.Get(accessControlRequestMethod) != null
}

type corsHandler struct {
	config *CorsConfig
}

func (ch *corsHandler) contains(values []string, target string) bool {
	for _, value := range values {
		if strings.EqualFold(value, target) {
			return true
		}
	}
	return false
}

func (ch *corsHandler) matchOrigin(origin string) (string, bool) {
	if ch.contains(globalCorsConfig.allowedOrigins, asterisk) {
		if globalCorsConfig.allowCredentials {
			return null, false
		}
		return null, true
	}

	if slices.Contains(globalCorsConfig.allowedOrigins, origin) {
		return origin, true
	}

	return null, false
}

// handlePreflight Handle preflight request
func (ch *corsHandler) handlePreflight(w http.ResponseWriter, r *http.Request) {

	header := w.Header()
	// Methods to get browser requests
	requestMethod := r.Header.Get(accessControlRequestMethod)

	if !ch.contains(globalCorsConfig.allowedMethods, requestMethod) {
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

			if !ch.contains(globalCorsConfig.allowedHeaders, h) {
				http.Error(w, corsHeaderNotAllowed, http.StatusForbidden)
				return
			}
		}
	}

	// Set preflight response headers
	header.Set(accessControlAllowMethods, strings.Join(globalCorsConfig.allowedMethods, headerComma))

	header.Set(accessControlAllowHeaders, strings.Join(globalCorsConfig.allowedHeaders, headerComma))

	if globalCorsConfig.maxAge > 0 {
		header.Set(accessControlMaxAge, strconv.FormatInt(int64(globalCorsConfig.maxAge.Seconds()), 10))
	}

	header.Add(vary, accessControlRequestMethod)
	header.Add(vary, accessControlRequestHeaders)

	w.WriteHeader(http.StatusNoContent)
}

func (ch *corsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {

	origin := strings.TrimSpace(r.Header.Get(headerOrigin))

	// Non-cross-origin request
	if origin == null {
		serveHTTP(w, r)
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
		serveHTTP(w, r)
		return
	}

	// Set general cross-origin response headers
	header := w.Header()
	header.Set(accessControlAllowOrigin, allowOrigin)
	header.Add(vary, headerOrigin)

	if globalCorsConfig.allowCredentials {
		header.Set(accessControlAllowCredentials, headerTrue)
	}

	if len(globalCorsConfig.exposedHeaders) > 0 {
		header.Set(accessControlExposeHeaders, strings.Join(globalCorsConfig.exposedHeaders, headerComma))
	}

	// Handle preflight request
	if isPreflightRequest(r) {
		ch.handlePreflight(w, r)
		return
	}

	serveHTTP(w, r)
}
