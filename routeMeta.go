// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

var routeMetaSlice = make([]*routeMeta, 0)

type routeMeta struct {
	method Method
	path   string
	fn     any
}

// register queues a route. Nothing is validated here: generateRoute does the
// whole check once the process is ready to serve.
func register(method Method, path string, fn any) {
	routeMetaSlice = append(routeMetaSlice,
		&routeMeta{
			method: method,
			path:   path,
			fn:     fn,
		},
	)
}

func Get(path string, fn any) {
	register(methodGet, path, fn)
}

// Head registers a handler for HEAD.
//
// Registering it is optional: an unregistered HEAD automatically reuses the GET
// route for the same path.
func Head(path string, fn any) {
	register(methodHead, path, fn)
}

func Post(path string, fn any) {
	register(methodPost, path, fn)
}

func Put(path string, fn any) {
	register(methodPut, path, fn)
}

func Patch(path string, fn any) {
	register(methodPatch, path, fn)
}

func Delete(path string, fn any) {
	register(methodDelete, path, fn)
}

// Options registers a handler for OPTIONS.
//
// An unregistered OPTIONS is answered by the framework with 204 and an Allow
// header listing the methods the path really supports, so registering a handler
// opts that path out of the automatic answer.
func Options(path string, fn any) {
	register(methodOptions, path, fn)
}

// Any registers fn for every method GBL supports, OPTIONS included.
func Any(path string, fn any) {
	for _, method := range allMethods {
		register(method, path, fn)
	}
}

// Match registers fn for exactly the named methods.
//
// Names are the standard HTTP verbs, for example
// Match([]string{http.MethodGet, http.MethodPost}, path, handler), and are
// accepted in any case. An unsupported verb aborts startup.
func Match(methods []string, path string, fn any) {
	for _, name := range methods {
		method, known := methodOf(name)
		if !known {
			panic("unknown http method: " + name)
		}
		register(method, path, fn)
	}
}
