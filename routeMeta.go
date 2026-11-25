// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

// routeMeta is a single queued route registration, recorded at Get/Post/... call
// time and turned into a real handler later, when generateRoute runs at startup.
type routeMeta struct {
	// method is the HTTP verb for this registration
	method httpMethod
	// path is the route pattern before normalization
	path string
	// fn is the user handler
	fn any
	// middleware contributed by the route group, if any
	mw []HandlerFunc
}

// register queues a route on the engine with no group middleware. Nothing is
// validated here: generateRoute does the whole check once the process is ready
// to serve.
func (e *engine) register(method httpMethod, path string, fn any) {
	e.registerWith(method, path, fn, nil)
}

// registerWith queues a route together with the middleware of the group it was
// registered through; the engine global middleware are merged in later, at
// route-generation time. The read-only-after-Run contract is checked by every
// public verb under its user-facing name, so nothing is asserted here.
func (e *engine) registerWith(method httpMethod, path string, fn any, mw []HandlerFunc) {
	e.routeMetaSlice = append(e.routeMetaSlice,
		&routeMeta{
			method: method,
			path:   path,
			fn:     fn,
			mw:     mw,
		},
	)
}

// Get registers a handler for GET requests at path.
func Get(path string, fn any) {
	defaultEngine.assertAssembling("Get")
	defaultEngine.register(methodGet, path, fn)
}

// Head registers a handler for HEAD.
//
// Registering it is optional: an unregistered HEAD automatically reuses the GET
// route for the same path.
func Head(path string, fn any) {
	defaultEngine.assertAssembling("Head")
	defaultEngine.register(methodHead, path, fn)
}

// Post registers a handler for POST requests at path. The request body is read
// and bound according to its Content-Type.
func Post(path string, fn any) {
	defaultEngine.assertAssembling("Post")
	defaultEngine.register(methodPost, path, fn)
}

// Put registers a handler for PUT requests at path. The request body is read and
// bound according to its Content-Type.
func Put(path string, fn any) {
	defaultEngine.assertAssembling("Put")
	defaultEngine.register(methodPut, path, fn)
}

// Patch registers a handler for PATCH requests at path. The request body is read
// and bound according to its Content-Type.
func Patch(path string, fn any) {
	defaultEngine.assertAssembling("Patch")
	defaultEngine.register(methodPatch, path, fn)
}

// Delete registers a handler for DELETE requests at path. Like GET, DELETE never
// reads the request body; only path and query parameters bind.
func Delete(path string, fn any) {
	defaultEngine.assertAssembling("Delete")
	defaultEngine.register(methodDelete, path, fn)
}

// Options registers a handler for OPTIONS.
//
// An unregistered OPTIONS is answered by the framework with 204 and an Allow
// header listing the methods the path really supports, so registering a handler
// opts that path out of the automatic answer.
func Options(path string, fn any) {
	defaultEngine.assertAssembling("Options")
	defaultEngine.register(methodOptions, path, fn)
}
