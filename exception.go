// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import "net/http"

type PanicProcessor func(w http.ResponseWriter, err any)

var panicProcessor PanicProcessor = nil

func catchRuntimePanic(w http.ResponseWriter) {
	err := recover()
	if err == nil {
		return
	}
	if panicProcessor == nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	panicProcessor(w, err)
}
