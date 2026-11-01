// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import "net/http"

var routeMetaSlice = make([]*routeMeta, 0)

type routeMeta struct {
	method string
	path   string
	fn     any
}

func Get(path string, fn any) {
	routeMetaSlice = append(routeMetaSlice,
		&routeMeta{
			method: http.MethodGet,
			path:   path,
			fn:     fn,
		},
	)
}

func Post(path string, fn any) {
	routeMetaSlice = append(routeMetaSlice,
		&routeMeta{
			method: http.MethodPost,
			path:   path,
			fn:     fn,
		},
	)
}

func Put(path string, fn any) {
	routeMetaSlice = append(routeMetaSlice,
		&routeMeta{
			method: http.MethodPut,
			path:   path,
			fn:     fn,
		},
	)
}

func Delete(path string, fn any) {
	routeMetaSlice = append(routeMetaSlice,
		&routeMeta{
			method: http.MethodDelete,
			path:   path,
			fn:     fn,
		},
	)
}
