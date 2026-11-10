// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

type RouteGroup interface {
	Get(string, any) RouteGroup
	Head(string, any) RouteGroup
	Post(string, any) RouteGroup
	Put(string, any) RouteGroup
	Patch(string, any) RouteGroup
	Delete(string, any) RouteGroup
	Options(string, any) RouteGroup
	Any(string, any) RouteGroup
	Match([]string, string, any) RouteGroup
}

type routeGroup struct {
	group string
}

func Group(group string) RouteGroup {
	return &routeGroup{group}
}

func (rg *routeGroup) Get(path string, fn any) RouteGroup {
	Get(rg.group+path, fn)
	return rg
}

func (rg *routeGroup) Head(path string, fn any) RouteGroup {
	Head(rg.group+path, fn)
	return rg
}

func (rg *routeGroup) Post(path string, fn any) RouteGroup {
	Post(rg.group+path, fn)
	return rg
}

func (rg *routeGroup) Put(path string, fn any) RouteGroup {
	Put(rg.group+path, fn)
	return rg
}

func (rg *routeGroup) Patch(path string, fn any) RouteGroup {
	Patch(rg.group+path, fn)
	return rg
}

func (rg *routeGroup) Delete(path string, fn any) RouteGroup {
	Delete(rg.group+path, fn)
	return rg
}

func (rg *routeGroup) Options(path string, fn any) RouteGroup {
	Options(rg.group+path, fn)
	return rg
}

func (rg *routeGroup) Any(path string, fn any) RouteGroup {
	Any(rg.group+path, fn)
	return rg
}

func (rg *routeGroup) Match(methods []string, path string, fn any) RouteGroup {
	Match(methods, rg.group+path, fn)
	return rg
}
