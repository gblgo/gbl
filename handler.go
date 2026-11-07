// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"net/http"
)

type defaultHandler struct{}

func (dh *defaultHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	serveHTTP(w, r)
}

func serveHTTP(w http.ResponseWriter, r *http.Request) {

	rw := &gblResponseWriter{w: w}
	// Global panic capture
	defer catchRuntimePanic(rw)

	params := make(map[string]string)
	rh := matchNode(parseMethod(r.Method), r.URL.Path, params)
	if rh == nil {
		rw.WriteHeader(http.StatusNotFound)
		panic(http.StatusText(http.StatusNotFound))
	}

	// Interceptor judgment
	if interceptor != nil {
		if ok := interceptor(rw, r); !ok {
			return
		}
	}
}
