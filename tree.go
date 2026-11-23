// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"fmt"
	"strings"
	"sync"
)

// routeNode is one segment in a method's routing tree. A path is matched by
// walking segments from the root; the node where the last segment lands either
// terminates in a handler (exist) or is only a waypoint on a longer route.
type routeNode struct {
	// raw segment text this node was created for
	segment string
	// full route path, set only on a terminal node
	path string
	// binding name when segment is a variable, e.g. "id" for ":id"
	paramName string
	// true when a handler is registered at this node
	exist bool
	// handler reached at this node, valid when exist
	handler *routeHandler
	// literal children, keyed by exact segment text
	staticNode map[string]*routeNode
	// wildcard children, matched against any segment
	varNode []*routeNode
}

// segmentMeta is one parsed segment of a registration path: its text, whether
// it is a variable, and the parameter name to bind when it is.
type segmentMeta struct {
	segment   string
	paramName string
	isVar     bool
}

// newNode builds an empty, non-terminal node for segment, ready to receive children.
func newNode(segment string) *routeNode {
	return &routeNode{
		segment:    segment,
		exist:      false,
		handler:    nil,
		staticNode: make(map[string]*routeNode),
		varNode:    make([]*routeNode, 0),
	}
}

// hasSameRoute reports whether segments[index:] already terminates on an
// existing route beneath node, returning that route's full path. It is the
// conflict probe used before inserting a new variable node: two differently
// named variables occupying the same position would match identically, and the
// caller rejects that.
func hasSameRoute(node *routeNode, segments []segmentMeta, index int) (bool, string) {
	if node == nil {
		return false, empty
	}

	if index == len(segments) {
		return node.exist, node.path
	}

	meta := segments[index]

	if meta.isVar {
		for _, vn := range node.varNode {
			if found, path := hasSameRoute(vn, segments, index+1); found {
				return true, path
			}
		}
		return false, empty
	}

	next := node.staticNode[meta.segment]
	return hasSameRoute(next, segments, index+1)
}

// insertNode walks segmentMetas down the tree for method, creating static and
// variable nodes as needed and attaching handler at the terminal node. Duplicate
// concrete routes and structurally equivalent variable routes abort startup with
// a panic, since both would leave matching ambiguous.
func (e *engine) insertNode(segmentMetas []segmentMeta, path string,
	method httpMethod, handler *routeHandler) {

	// Get the root node of the corresponding method
	root := e.roots[method]
	// Check if the root node is nil
	if root == nil {
		root = newNode(slash)
		e.roots[method] = root
	}
	// Insert route segments
	for i, meta := range segmentMetas {
		isLast := i == len(segmentMetas)-1
		if meta.isVar {
			var node *routeNode
			// First, find the dynamic node with the same name
			for _, vn := range root.varNode {
				if vn.segment == meta.segment {
					node = vn
					break
				}
			}

			// If there is no node with the same name, check whether the same routing structure exists
			if node == nil {
				for _, vn := range root.varNode {
					if found, existPath := hasSameRoute(vn, segmentMetas, i+1); found {
						e.failStartup(fmt.Errorf("variable route conflicts: [%s %s]", path, existPath))
					}
				}

				node = newNode(meta.segment)
				node.paramName = meta.paramName
				root.varNode = append(root.varNode, node)
			}

			if isLast {
				if node.exist {
					e.failStartup(fmt.Errorf("variable route already exists: [%s %s]", path, node.path))
				}

				node.exist = true
				node.handler = handler
				node.path = path
			}

			root = node
			continue
		}

		// Find or create a static node
		node := root.staticNode[meta.segment]

		if node == nil {
			node = newNode(meta.segment)
			root.staticNode[meta.segment] = node
		}

		// Handle terminal node
		if isLast {
			if node.exist {
				e.failStartup(fmt.Errorf("static route already exists: [%s %s]", path, node.path))
			}

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

// splitPathInto appends the non-empty "/"-separated segments of path to dst and
// returns the grown slice, reusing dst's backing array. Segments are substrings
// sharing path's storage, so unlike strings.Split this copies nothing.
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

// matchNode finds the handler registered for method and path, filling params
// with any bound path-variable values along the way. It returns nil when the
// method has no tree yet or nothing under it matches.
//
// Optimization: static routes (no path parameters) are stored in a map for O(1)
// lookup. Only dynamic routes (with path parameters like :id) use the radix tree.
//
// A request path carrying empty segments ("/user//x", "/user/") is not in
// normal form, while every stored route is: registration dropped the same empty
// segments. The tree already ignores them through splitPathInto, so the static
// map probes the '/'-joined segments too, and both tables answer such a path
// identically.
func (e *engine) matchNode(method httpMethod, path string, params map[string]string) *routeHandler {
	// Fast path: try static route map first (O(1))
	statics := e.staticRoutes[method]
	if handler, ok := statics[path]; ok {
		return handler
	}

	// Slow path: fall back to radix tree for dynamic routes (O(path length))
	root := e.roots[method]
	if root == nil && (len(statics) == 0 || isNormalized(path)) {
		// No tree to walk. A normalized path also cannot hit the rejoined
		// static reprobe below, so splitting it would be wasted work — the
		// common shape when allowFor probes static-only methods for a 404.
		return nil
	}

	bufPool := segmentPool.Get().(*[]string)
	segments := splitPathInto((*bufPool)[:0], path)

	var handler *routeHandler
	if root != nil {
		handler = searchNode(root, segments, 0, params)
	}
	if handler == nil && len(statics) > 0 && !isNormalized(path) {
		normalized := slash + strings.Join(segments, slash)
		handler = statics[normalized]
	}

	*bufPool = segments
	segmentPool.Put(bufPool)

	return handler
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
		if e.matchNode(m, path, scratch) != nil {
			reachable[m] = true
		}
	}

	// HEAD is served by the GET route even when it was never registered, so the
	// advertised method list has to match what serveHTTP really accepts.
	if reachable[methodGet] {
		reachable[methodHead] = true
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
// a static child over a variable one at every level. When it commits to a
// variable branch it records the segment under that node's parameter name and,
// if the branch later proves dead, deletes it again so a failed search leaves
// no stray params behind.
func searchNode(root *routeNode, segments []string, index int,
	params map[string]string) *routeHandler {

	if root == nil {
		return nil
	}

	// All path segments have matched successfully
	if index == len(segments) {
		if root.exist {
			return root.handler
		}
		return nil
	}

	segment := segments[index]

	// Priority matching for static nodes
	staticNode := root.staticNode[segment]

	if staticNode != nil {
		handler := searchNode(staticNode, segments, index+1, params)

		if handler != nil {
			return handler
		}
	}

	// Static node matching failed, attempting dynamic node
	for _, vn := range root.varNode {

		// Record the current dynamic parameters
		params[vn.paramName] = segment

		handler := searchNode(vn, segments, index+1, params)

		if handler != nil {
			return handler
		}

		// The current branch matching failed, delete the parameters just recorded
		delete(params, vn.paramName)
	}

	return nil
}
