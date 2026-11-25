// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

// RouteGroup is a path-prefixed, middleware-scoped registration handle returned
// by Group. Its methods mirror the package-level verbs, prepend the group prefix
// to each path, and attach group middleware to routes registered through it.
// Every method returns the group itself, so calls chain fluently.
type RouteGroup interface {
	// GroupUse appends middleware scoped to this group.
	GroupUse(...HandlerFunc) RouteGroup
	// Get registers a GET handler under the group prefix.
	Get(string, any) RouteGroup
	// Head registers a HEAD handler under the group prefix.
	Head(string, any) RouteGroup
	// Post registers a POST handler under the group prefix.
	Post(string, any) RouteGroup
	// Put registers a PUT handler under the group prefix.
	Put(string, any) RouteGroup
	// Patch registers a PATCH handler under the group prefix.
	Patch(string, any) RouteGroup
	// Delete registers a DELETE handler under the group prefix.
	Delete(string, any) RouteGroup
	// Options registers an OPTIONS handler under the group prefix.
	Options(string, any) RouteGroup
}

// routeGroup is the concrete RouteGroup: it remembers the engine it came from,
// the path prefix to prepend, and the middleware to attach to its routes.
type routeGroup struct {
	// middleware that runs only for this group routes
	middlewares []HandlerFunc

	// group is the path prefix prepended to each route
	group string

	// engine owns registration and the frozen route set
	engine *engine
}

// Group returns a route group on the default engine; every path registered
// through it is prefixed with group.
func Group(group string) RouteGroup {
	return defaultEngine.group(group)
}

// group is the instance-level counterpart of package-level Group.
func (e *engine) group(group string) RouteGroup {
	return &routeGroup{engine: e, group: group}
}

// GroupUse appends middleware scoped to this group. It runs after engine global
// middleware and only for routes registered through this group. Like every
// assembling API, it panics once the engine has run.
func (rg *routeGroup) GroupUse(middleware ...HandlerFunc) RouteGroup {
	rg.engine.assertAssembling("GroupUse")
	rg.middlewares = append(rg.middlewares, middleware...)
	return rg
}

// Get registers a GET handler under the group prefix.
func (rg *routeGroup) Get(path string, fn any) RouteGroup {
	rg.engine.assertAssembling("Group.Get")
	rg.engine.registerWith(methodGet, rg.group+path, fn, rg.middlewares)
	return rg
}

// Head registers a HEAD handler under the group prefix.
func (rg *routeGroup) Head(path string, fn any) RouteGroup {
	rg.engine.assertAssembling("Group.Head")
	rg.engine.registerWith(methodHead, rg.group+path, fn, rg.middlewares)
	return rg
}

// Post registers a POST handler under the group prefix.
func (rg *routeGroup) Post(path string, fn any) RouteGroup {
	rg.engine.assertAssembling("Group.Post")
	rg.engine.registerWith(methodPost, rg.group+path, fn, rg.middlewares)
	return rg
}

// Put registers a PUT handler under the group prefix.
func (rg *routeGroup) Put(path string, fn any) RouteGroup {
	rg.engine.assertAssembling("Group.Put")
	rg.engine.registerWith(methodPut, rg.group+path, fn, rg.middlewares)
	return rg
}

// Patch registers a PATCH handler under the group prefix.
func (rg *routeGroup) Patch(path string, fn any) RouteGroup {
	rg.engine.assertAssembling("Group.Patch")
	rg.engine.registerWith(methodPatch, rg.group+path, fn, rg.middlewares)
	return rg
}

// Delete registers a DELETE handler under the group prefix.
func (rg *routeGroup) Delete(path string, fn any) RouteGroup {
	rg.engine.assertAssembling("Group.Delete")
	rg.engine.registerWith(methodDelete, rg.group+path, fn, rg.middlewares)
	return rg
}

// Options registers an OPTIONS handler under the group prefix.
func (rg *routeGroup) Options(path string, fn any) RouteGroup {
	rg.engine.assertAssembling("Group.Options")
	rg.engine.registerWith(methodOptions, rg.group+path, fn, rg.middlewares)
	return rg
}
