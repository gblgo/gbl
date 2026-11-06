// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import "net/http"

var processor Processor = nil

type Processor func(w http.ResponseWriter, r *http.Request)

func RegisterProcessor(p Processor) {
	processor = p
}
