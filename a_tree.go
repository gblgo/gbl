// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
)

var (
	errRequestPathTooManySegments = errors.New("request path has too many segments")
	errRequestPathEncodedSlash    = errors.New("request path contains an encoded slash")
)

// routeNode is one segment in a method routing tree. A path is matched by
// walking segments from the root; the node where the last segment lands either
// terminates in a handler (exist) or is only a waypoint on a longer route.
type routeNode struct {
	// full route path, set only on a terminal node
	path string
	// true when a handler is registered at this node
	exist bool
	// handler reached at this node, valid when exist
	handler *routeHandler
	// literal children, keyed by exact segment text
	staticNode map[string]*routeNode
	// structural wildcard child, matched against any segment. Marker spelling and
	// parameter names do not create distinct branches, so a generated node has at
	// most one entry.
	varNode *routeNode
}

// pathParam binds one dynamic segment position to a route-specific name.
type pathParam struct {
	// name is the route-specific request binding key.
	name string
	// index is the matched path segment position.
	index int
}

// segmentMeta is one parsed segment of a registration path: its text, whether
// it is a variable, and the parameter name to bind when it is.
type segmentMeta struct {
	// segment is the literal or variable segment text
	segment string
	// paramName is the binding name for a variable segment
	paramName string
	// isVar distinguishes parameters from literal segments
	isVar bool
}

// findOverlappingRoute walks only existing branches compatible with the new
// pattern, avoiding a full tree scan and pattern allocations during registration.
func findOverlappingRoute(node *routeNode, segments []segmentMeta, index int) (bool, string) {
	if node == nil {
		return false, empty
	}
	if index == len(segments) {
		return node.exist, node.path
	}
	meta := segments[index]
	if meta.isVar {
		for _, child := range node.staticNode {
			if found, path := findOverlappingRoute(child, segments, index+1); found {
				return true, path
			}
		}
	} else if found, path := findOverlappingRoute(
		node.staticNode[meta.segment], segments, index+1); found {
		return true, path
	}
	// Generated trees have one structural wildcard child.
	if node.varNode != nil {
		if found, path := findOverlappingRoute(node.varNode, segments, index+1); found {
			return true, path
		}
	}
	return false, empty
}

// newNode builds an empty, non-terminal node ready to receive children.
func newNode() *routeNode {
	return &routeNode{
		exist:      false,
		handler:    nil,
		staticNode: make(map[string]*routeNode),
	}
}

// insertNode walks segmentMetas down the tree for method, creating static and
// variable nodes as needed and attaching handler at the terminal node. Duplicate
// concrete routes and structurally equivalent variable routes abort startup with
// a panic, since both would leave matching ambiguous. All wildcard segments at
// one position share a node regardless of marker or parameter name; the terminal
// handler owns the names used to bind that matched route.
func (e *engine) insertNode(segmentMetas []segmentMeta, path string,
	method httpMethod, handler *routeHandler) {
	root := e.roots[method]
	if root == nil {
		root = newNode()
		e.roots[method] = root
	}
	if found, existingPath := findOverlappingRoute(root, segmentMetas, 0); found {
		e.failStartup(fmt.Errorf("variable route conflicts: [%s %s]", path, existingPath))
	}

	handler.pathParams = handler.pathParams[:0]
	for i, meta := range segmentMetas {
		if meta.isVar {
			handler.pathParams = append(handler.pathParams, pathParam{name: meta.paramName, index: i})
		}
	}

	// Insert route segments.
	for i, meta := range segmentMetas {
		isLast := i == len(segmentMetas)-1
		if meta.isVar {
			var node *routeNode
			if root.varNode != nil {
				node = root.varNode
			} else {
				node = newNode()
				root.varNode = node
			}

			if isLast {
				node.exist = true
				node.handler = handler
				node.path = path
			}

			root = node
			continue
		}

		// Find or create a static node.
		node := root.staticNode[meta.segment]
		if node == nil {
			node = newNode()
			root.staticNode[meta.segment] = node
		}

		if isLast {
			node.exist = true
			node.handler = handler
			node.path = path
		}

		root = node
	}
}

// segmentPool recycles the per-request slice of path segments so matchNode
// allocates nothing once warm. A pointer to the slice is stored to keep the
// Get/Put boxing itself allocation-free.
var segmentPool = sync.Pool{
	New: func() any { s := make([]string, 0, 8); return &s },
}

// largest segment slice retained by the pool
const maxPooledPathSegments = 64

// maxRoutePathSegments bounds recursive dynamic-route matching.
const maxRoutePathSegments = 1024

// releaseSegments clears path references and pools a reasonably sized slice.
func releaseSegments(bufPool *[]string, segments []string) {
	clear(segments)
	if cap(segments) > maxPooledPathSegments {
		return
	}
	*bufPool = segments[:0]
	segmentPool.Put(bufPool)
}

// maxPooledPathBytes bounds temporary normalized-path storage.
const maxPooledPathBytes = 4 << 10

// pathBufPool reuses byte storage for static-route normalization probes.
var pathBufPool = sync.Pool{
	New: func() any {
		buffer := make([]byte, 0, 64)
		return &buffer
	},
}

// normalizePathBytes joins path segments without allocating a string.
func normalizePathBytes(dst []byte, segments []string) []byte {
	for _, segment := range segments {
		dst = append(dst, '/')
		dst = append(dst, segment...)
	}
	return dst
}

// normalizeRequestPath gives routing and middleware the same path view while
// preserving RequestURI as the original request target for logging.
func normalizeRequestPath(r *http.Request) error {
	if hasEncodedPathSlash(r.URL.RawPath) {
		return errRequestPathEncodedSlash
	}
	if len(r.URL.Path) > maxRoutePathSegments*2 && pathExceedsSegmentLimit(r.URL.Path) {
		return errRequestPathTooManySegments
	}
	if r.URL.Path == slash || isNormalized(r.URL.Path) {
		return nil
	}

	bufPool := segmentPool.Get().(*[]string)
	segments, _ := splitPathIntoLimited((*bufPool)[:0], r.URL.Path)
	pathBuf := pathBufPool.Get().(*[]byte)
	normalized := normalizePathBytes((*pathBuf)[:0], segments)
	if len(normalized) == 0 {
		normalized = append(normalized, '/')
	}
	path := string(normalized)
	releasePathBytes(pathBuf, normalized)
	releaseSegments(bufPool, segments)
	r.URL.Path = path
	r.URL.RawPath = empty
	return nil
}

func pathExceedsSegmentLimit(path string) bool {
	segments := 0
	for i := 0; i < len(path); {
		if path[i] == '/' {
			i++
			continue
		}
		segments++
		if segments > maxRoutePathSegments {
			return true
		}
		for i < len(path) && path[i] != '/' {
			i++
		}
	}
	return false
}

func hasEncodedPathSlash(rawPath string) bool {
	for i := 0; i+2 < len(rawPath); i++ {
		if rawPath[i] == '%' && rawPath[i+1] == '2' &&
			(rawPath[i+2] == 'f' || rawPath[i+2] == 'F') {
			return true
		}
	}
	return false
}

// releasePathBytes pools bounded normalization storage.
func releasePathBytes(bufPool *[]byte, buffer []byte) {
	if cap(buffer) > maxPooledPathBytes {
		return
	}
	*bufPool = buffer[:0]
	pathBufPool.Put(bufPool)
}

// splitPathInto appends the non-empty "/"-separated segments of path to dst and
// returns the grown slice, reusing dst backing array. Segments are substrings
// sharing path storage, so unlike strings.Split this copies nothing.
func splitPathInto(dst []string, path string) []string {
	for {
		for len(path) > 0 && path[0] == '/' {
			path = path[1:]
		}
		if len(path) == 0 {
			return dst
		}
		var segment string
		segment, path, _ = strings.Cut(path, slash)
		dst = append(dst, segment)
	}
}

// splitPathIntoLimited stops before allocating a segment beyond the router's
// maximum depth. The boolean reports whether the whole path fit.
func splitPathIntoLimited(dst []string, path string) ([]string, bool) {
	for {
		for len(path) > 0 && path[0] == '/' {
			path = path[1:]
		}
		if len(path) == 0 {
			return dst, true
		}
		if len(dst) >= maxRoutePathSegments {
			return dst, false
		}
		var segment string
		segment, path, _ = strings.Cut(path, slash)
		dst = append(dst, segment)
	}
}

// matchRequestNode preserves the exact-static fast path while ensuring every
// path that reaches middleware has already been normalized.
func (e *engine) matchRequestNode(method httpMethod, r *http.Request,
	params map[string]string) (*routeHandler, map[string]string, error) {
	if hasEncodedPathSlash(r.URL.RawPath) {
		return nil, params, errRequestPathEncodedSlash
	}
	path := r.URL.Path
	if handler, ok := e.staticRoutes[method][path]; ok {
		return handler, params, nil
	}
	if method == methodHead {
		if handler, ok := e.staticRoutes[methodGet][path]; ok {
			return handler, params, nil
		}
	}
	if err := normalizeRequestPath(r); err != nil {
		return nil, params, err
	}
	handler, params := e.matchNode(method, r.URL.Path, params)
	return handler, params, nil
}

// matchNode finds the handler registered for method and path, filling params
// with any bound path-variable values along the way. It returns nil when the
// method has no tree yet or nothing under it matches. params may be nil; the
// dynamic path acquires a pooled map only when needed, and the returned map is
// caller-owned when non-nil.
//
// Exact static routes keep the allocation-free map fast path. A non-normalized
// request is normalized and rechecked against that map before the dynamic tree,
// so repeated or trailing separators cannot let a variable route shadow a
// static one.
func (e *engine) matchNode(
	method httpMethod,
	path string,
	params map[string]string,
) (*routeHandler, map[string]string) {
	// Fast path: try static route map first (O(1))
	statics := e.staticRoutes[method]
	if handler, ok := statics[path]; ok {
		return handler, params
	}

	if len(statics) > 0 && !isNormalized(path) {
		segmentBuf := segmentPool.Get().(*[]string)
		segments, ok := splitPathIntoLimited((*segmentBuf)[:0], path)
		if !ok {
			releaseSegments(segmentBuf, segments)
			return nil, params
		}
		pathBuf := pathBufPool.Get().(*[]byte)
		normalized := normalizePathBytes((*pathBuf)[:0], segments)
		handler, ok := statics[string(normalized)]
		releasePathBytes(pathBuf, normalized)
		releaseSegments(segmentBuf, segments)
		if ok {
			return handler, params
		}
	}

	// Slow path: fall back to the segment trie for dynamic routes
	root := e.roots[method]
	if root == nil {
		return nil, params
	}

	if params == nil {
		params = pathParamsPool.Get().(map[string]string)
	}
	bufPool := segmentPool.Get().(*[]string)
	segments, ok := splitPathIntoLimited((*bufPool)[:0], path)
	if !ok {
		releaseSegments(bufPool, segments)
		return nil, params
	}
	handler := searchNode(root, segments, 0, params)

	releaseSegments(bufPool, segments)

	return handler, params
}

// isNormalized reports whether the path already stores itself: no trailing
// separator and no doubled one inside. Exact requests, which are the overwhelming
// majority, therefore never reach the join the normalized probe needs.
func isNormalized(path string) bool {
	if len(path) < 2 || path[len(path)-1] == '/' {
		return false
	}
	return !strings.Contains(path, "//")
}

// allowFor lists the methods that can actually reach path.
//
// Every supported method owns a separate tree, so a miss only proves the path is
// unreachable for that one verb. Probing the rest tells "no such path" apart
// from "path exists, wrong verb", which is what the 405 Allow header needs.
// This runs on the 404/405 path only, so the extra tree walks are affordable.
func (e *engine) allowFor(path string) []string {
	reachable := make([]bool, methodCount)
	scratch := make(map[string]string, 4)

	for m := range methodCount {
		clear(scratch)
		if handler, _ := e.matchNode(m, path, scratch); handler != nil {
			reachable[m] = true
		}
	}

	// HEAD is served by the GET route even when it was never registered, and
	// OPTIONS is answered automatically for every reachable path. The advertised
	// list therefore includes both implicit verbs, without duplicating an
	// explicitly registered OPTIONS handler.
	if reachable[methodGet] {
		reachable[methodHead] = true
	}
	for _, ok := range reachable {
		if ok {
			reachable[methodOptions] = true
			break
		}
	}

	var allowed []string
	for m, ok := range reachable {
		if ok {
			allowed = append(allowed, methodNames[m])
		}
	}

	return allowed
}

// searchNode recursively matches segments[index:] starting at root, preferring
// a static child over the single structural variable child at every level.
// Parameters are bound only after a terminal handler is found, using that
// handler route-specific names; failed branches therefore leave no stray keys
// and shared wildcard nodes need no backtracking cleanup.
func searchNode(root *routeNode, segments []string, index int,
	params map[string]string) *routeHandler {
	if root == nil {
		return nil
	}
	if index == len(segments) {
		if !root.exist {
			return nil
		}
		for _, param := range root.handler.pathParams {
			params[param.name] = segments[param.index]
		}
		return root.handler
	}

	segment := segments[index]
	if staticNode := root.staticNode[segment]; staticNode != nil {
		if handler := searchNode(staticNode, segments, index+1, params); handler != nil {
			return handler
		}
	}
	if root.varNode != nil {
		if handler := searchNode(root.varNode, segments, index+1, params); handler != nil {
			return handler
		}
	}
	return nil
}
