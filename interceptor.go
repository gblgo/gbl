// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import "net/http"

var interceptor Interceptor = nil

type Interceptor func(r *http.Request) bool

func RegisterInterceptor(i Interceptor) {
	interceptor = i
}
