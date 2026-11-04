// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"net/http"
)

type gblHandler struct{}

func (gbl *gblHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {

	rh := matchNode(parseMethod(r.Method), r.URL.Path)
	if rh == nil {
		http.NotFound(w, r)
		return
	}
	//handler, params := match(r.Method, r.URL.Path)
	//if handler == nil {
	//	http.NotFound(w, r)
	//	return
	//}
	//args, err := convertParams(handler, params)
	//if err != nil {
	//	http.Error(w, err.Error(), http.StatusBadRequest)
	//	return
	//}
	//result, err := callHandler(handler, args)
	//if err != nil {
	//	http.Error(w, err.Error(), http.StatusInternalServerError)
	//	return
	//}
	//if result.IsValid() {
	//	w.Header().Set("Content-Type", "application/json")
	//	if err := json.NewEncoder(w).Encode(result.Interface()); err != nil {
	//		http.Error(w, "response encoding failed", http.StatusInternalServerError)
	//	}
	//	return
	//}
	//
	//w.WriteHeader(http.StatusOK)
}
