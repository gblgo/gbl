// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import "strings"

var roots = make([]*routeNode, methodCount)

type routeNode struct {
	segment    string
	exist      bool
	handler    *routeHandler
	staticNode map[string]*routeNode
	varNode    []*routeNode
}

type segmentMeta struct {
	segment string
	isVar   bool
}

func insertNode(segmentMetas []segmentMeta, path string, method Method, handler *routeHandler) {

	// Get the root node of the corresponding method
	root := roots[method]
	// Check if the root node is null
	if root == nil {
		root = &routeNode{
			segment:    "/",
			exist:      false,
			handler:    nil,
			staticNode: make(map[string]*routeNode),
			varNode:    make([]*routeNode, 0),
		}
		roots[method] = root
	}
	for i, meta := range segmentMetas {
		if meta.isVar {
			if len(root.varNode) == 0 {
				node := &routeNode{
					segment:    meta.segment,
					exist:      false,
					handler:    nil,
					staticNode: make(map[string]*routeNode),
					varNode:    make([]*routeNode, 0),
				}
				if i+1 == len(segmentMetas) {
					node.exist = true
					node.handler = handler
				}
				root.varNode = append(root.varNode, node)
				root = node
				continue
			}
			for _, vn := range root.varNode {
				if vn.exist && i+1 == len(segmentMetas) {
					panic("There exist variable routes with conflicts: " + path)
				}
				if vn.segment == meta.segment {
					if i+1 == len(segmentMetas) {
						vn.exist = true
						vn.handler = handler
					}
					root = vn
					break
				}
			}
			continue
		}
		node := root.staticNode[meta.segment]
		if node == nil {
			node = &routeNode{
				segment:    meta.segment,
				exist:      false,
				handler:    nil,
				staticNode: make(map[string]*routeNode),
				varNode:    make([]*routeNode, 0),
			}
			root.staticNode[meta.segment] = node
		}
		if i+1 == len(segmentMetas) {
			if node.exist {
				panic("The finally route segment already exists: " + path)
			}
			node.exist = true
			node.handler = handler
			break
		}
		root = node
	}
}

func getLegalSegments(path string) []string {
	path = strings.TrimSpace(path)
	parts := strings.Split(path, slash)
	segments := make([]string, 0, len(parts))

	for _, part := range parts {
		if part == null {
			continue
		}
		segments = append(segments, part)
	}

	return segments
}

func matchNode(method Method, path string) *routeHandler {

	segments := getLegalSegments(path)

	root := roots[method]
	if root == nil {
		return nil
	}

	for i, segment := range segments {
		// First, find the static route
		node := root.staticNode[segment]
		if node != nil {
			root = node
			continue
		}
		// If the static route does not exist, then check the variable route
		if len(root.varNode) == 0 {
			return nil
		}
		for _, vn := range root.varNode {
			if vn.exist && i+1 == len(segments) {
				return vn.handler
			}

		}
	}

	return nil
}
