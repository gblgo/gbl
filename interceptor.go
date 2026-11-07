// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import "net/http"

var interceptor Interceptor = nil

// Interceptor
// Return 'true' to allow the request, 'false' to deny the request
// Note: If returning false to prohibit the request, you need to set httpStatus yourself and use w.Write to write back the information
type Interceptor func(w http.ResponseWriter, r *http.Request) bool

func RegisterInterceptor(i Interceptor) {
	interceptor = i
}
