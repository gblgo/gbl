// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

// RouteGroup is a path-prefixed, middleware-scoped registration handle returned
// by Group. Its methods mirror the package-level verbs but prepend the group's
// prefix to each path and attach the group's middleware to routes registered
// through it; every method returns the group itself, so calls chain fluently.
type RouteGroup interface {
	// GroupUse appends middleware scoped to this group.
	GroupUse(...HandlerFunc) RouteGroup
	// Get registers a GET handler under the group's prefix.
	Get(string, any) RouteGroup
	// Head registers a HEAD handler under the group's prefix.
	Head(string, any) RouteGroup
	// Post registers a POST handler under the group's prefix.
	Post(string, any) RouteGroup
	// Put registers a PUT handler under the group's prefix.
	Put(string, any) RouteGroup
	// Patch registers a PATCH handler under the group's prefix.
	Patch(string, any) RouteGroup
	// Delete registers a DELETE handler under the group's prefix.
	Delete(string, any) RouteGroup
	// Options registers an OPTIONS handler under the group's prefix.
	Options(string, any) RouteGroup
}

// routeGroup is the concrete RouteGroup: it remembers the engine it came from,
// the path prefix to prepend, and the middleware to attach to its routes.
type routeGroup struct {
	// 24-byte fields (slice)
	// middleware that runs only for this group's routes
	middlewares []HandlerFunc

	// 16-byte fields (string)
	group string

	// 8-byte fields (pointer)
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

// GroupUse appends middleware scoped to this group. They run after the engine's
// global middleware and only for routes registered through this group. Like
// every assembling API, it panics once the engine has run.
func (rg *routeGroup) GroupUse(middleware ...HandlerFunc) RouteGroup {
	rg.engine.assertAssembling("GroupUse")
	rg.middlewares = append(rg.middlewares, middleware...)
	return rg
}

func (rg *routeGroup) Get(path string, fn any) RouteGroup {
	rg.engine.assertAssembling("Group.Get")
	rg.engine.registerWith(methodGet, rg.group+path, fn, rg.middlewares)
	return rg
}

func (rg *routeGroup) Head(path string, fn any) RouteGroup {
	rg.engine.assertAssembling("Group.Head")
	rg.engine.registerWith(methodHead, rg.group+path, fn, rg.middlewares)
	return rg
}

func (rg *routeGroup) Post(path string, fn any) RouteGroup {
	rg.engine.assertAssembling("Group.Post")
	rg.engine.registerWith(methodPost, rg.group+path, fn, rg.middlewares)
	return rg
}

func (rg *routeGroup) Put(path string, fn any) RouteGroup {
	rg.engine.assertAssembling("Group.Put")
	rg.engine.registerWith(methodPut, rg.group+path, fn, rg.middlewares)
	return rg
}

func (rg *routeGroup) Patch(path string, fn any) RouteGroup {
	rg.engine.assertAssembling("Group.Patch")
	rg.engine.registerWith(methodPatch, rg.group+path, fn, rg.middlewares)
	return rg
}

func (rg *routeGroup) Delete(path string, fn any) RouteGroup {
	rg.engine.assertAssembling("Group.Delete")
	rg.engine.registerWith(methodDelete, rg.group+path, fn, rg.middlewares)
	return rg
}

func (rg *routeGroup) Options(path string, fn any) RouteGroup {
	rg.engine.assertAssembling("Group.Options")
	rg.engine.registerWith(methodOptions, rg.group+path, fn, rg.middlewares)
	return rg
}
