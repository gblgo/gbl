// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

// RouteGroup is a path-prefixed, middleware-scoped registration handle returned
// by Group. Its methods mirror the package-level verbs but prepend the group's
// prefix to each path and attach the group's middleware to routes registered
// through it; every method returns the group itself, so calls chain fluently.
type RouteGroup interface {
	// Use appends middleware scoped to this group.
	Use(...HandlerFunc) RouteGroup
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
	// Any registers the handler for every supported method under the prefix.
	Any(string, any) RouteGroup
	// Match registers the handler for exactly the named methods under the prefix.
	Match([]string, string, any) RouteGroup
}

// routeGroup is the concrete RouteGroup: it remembers the engine it came from,
// the path prefix to prepend, and the middleware to attach to its routes.
type routeGroup struct {
	engine      *Engine
	group       string
	middlewares []HandlerFunc // middleware that runs only for this group's routes
}

// Group returns a route group on the default engine; every path registered
// through it is prefixed with group.
func Group(group string) RouteGroup {
	return defaultEngine.Group(group)
}

// Group is the instance-level counterpart of package-level Group.
func (e *Engine) Group(group string) RouteGroup {
	return &routeGroup{engine: e, group: group}
}

// Use appends middleware scoped to this group. They run after the engine's
// global middleware and only for routes registered through this group.
func (rg *routeGroup) Use(middleware ...HandlerFunc) RouteGroup {
	rg.middlewares = append(rg.middlewares, middleware...)
	return rg
}

func (rg *routeGroup) Get(path string, fn any) RouteGroup {
	rg.engine.registerWith(methodGet, rg.group+path, fn, rg.middlewares)
	return rg
}

func (rg *routeGroup) Head(path string, fn any) RouteGroup {
	rg.engine.registerWith(methodHead, rg.group+path, fn, rg.middlewares)
	return rg
}

func (rg *routeGroup) Post(path string, fn any) RouteGroup {
	rg.engine.registerWith(methodPost, rg.group+path, fn, rg.middlewares)
	return rg
}

func (rg *routeGroup) Put(path string, fn any) RouteGroup {
	rg.engine.registerWith(methodPut, rg.group+path, fn, rg.middlewares)
	return rg
}

func (rg *routeGroup) Patch(path string, fn any) RouteGroup {
	rg.engine.registerWith(methodPatch, rg.group+path, fn, rg.middlewares)
	return rg
}

func (rg *routeGroup) Delete(path string, fn any) RouteGroup {
	rg.engine.registerWith(methodDelete, rg.group+path, fn, rg.middlewares)
	return rg
}

func (rg *routeGroup) Options(path string, fn any) RouteGroup {
	rg.engine.registerWith(methodOptions, rg.group+path, fn, rg.middlewares)
	return rg
}

func (rg *routeGroup) Any(path string, fn any) RouteGroup {
	for _, method := range allMethods {
		rg.engine.registerWith(method, rg.group+path, fn, rg.middlewares)
	}
	return rg
}

func (rg *routeGroup) Match(methods []string, path string, fn any) RouteGroup {
	for _, name := range methods {
		method, known := methodOf(name)
		if !known {
			panic("unknown http method: " + name)
		}
		rg.engine.registerWith(method, rg.group+path, fn, rg.middlewares)
	}
	return rg
}
