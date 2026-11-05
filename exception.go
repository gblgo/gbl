// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import "net/http"

func catchRuntimePanic(gh *gblHandler, w http.ResponseWriter) {
	err := recover()
	if err == nil {
		return
	}
	gh.panicProcessor(w, err)
}

type PanicProcessor func(w http.ResponseWriter, err any)

func defaultPanicProcessor(w http.ResponseWriter, err any) {}
