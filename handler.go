// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"net/http"
)

var gh = &gblHandler{panicProcessor: defaultPanicProcessor}

type gblHandler struct {
	panicProcessor PanicProcessor
	interceptor    Interceptor
	processor      Processor
}

func (gbl *gblHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {

	defer catchRuntimePanic(gbl, w)

	params := make(map[string]string)
	rh := matchNode(parseMethod(r.Method), r.URL.Path, params)
	if rh == nil {
		http.NotFound(w, r)
		return
	}

	if gh.interceptor != nil {
		if ok := gbl.interceptor.PreHandle(r); !ok {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
	}
}
