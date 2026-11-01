// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

type RouteGroup interface {
	Get(string, any) RouteGroup
	Post(string, any) RouteGroup
	Put(string, any) RouteGroup
	Delete(string, any) RouteGroup
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

func (rg *routeGroup) Post(path string, fn any) RouteGroup {
	Post(rg.group+path, fn)
	return rg
}

func (rg *routeGroup) Put(path string, fn any) RouteGroup {
	Put(rg.group+path, fn)
	return rg
}

func (rg *routeGroup) Delete(path string, fn any) RouteGroup {
	Delete(rg.group+path, fn)
	return rg
}
