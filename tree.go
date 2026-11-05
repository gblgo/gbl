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

func newNode(segment string) *routeNode {
	return &routeNode{
		segment:    segment,
		exist:      false,
		handler:    nil,
		staticNode: make(map[string]*routeNode),
		varNode:    make([]*routeNode, 0),
	}
}

func insertNode(segmentMetas []segmentMeta, path string, method Method, handler *routeHandler) {

	// Get the root node of the corresponding method
	root := roots[method]
	// Check if the root node is null
	if root == nil {
		root = newNode(slash)
		roots[method] = root
	}
	// Insert route segments
	for i, meta := range segmentMetas {
		isLast := i == len(segmentMetas)-1

		if meta.isVar {
			// Find an existing variable node with the same name
			var node *routeNode

			for _, vn := range root.varNode {
				if vn.segment == meta.segment {
					node = vn
					break
				}
			}

			// Create a new variable node if not found
			if node == nil {
				node = newNode(meta.segment)
				root.varNode = append(root.varNode, node)
			}

			// Handle terminal node
			if isLast {
				if node.exist {
					panic("The variable route already exists: " + path)
				}

				node.exist = true
				node.handler = handler
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
				panic("The static route already exists: " + path)
			}

			node.exist = true
			node.handler = handler
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

func matchNode(method Method, path string) (*routeHandler, map[string]string) {

	segments := getLegalSegments(path)

	root := roots[method]
	if root == nil {
		return nil, nil
	}

	params := make(map[string]string)

	handler := searchNode(root, segments, 0, params)

	if handler == nil {
		return nil, nil
	}

	return handler, params
}

func searchNode(
	root *routeNode,
	segments []string,
	index int,
	params map[string]string,
) *routeHandler {

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

	// 1. Priority matching for static nodes
	staticNode := root.staticNode[segment]

	if staticNode != nil {
		handler := searchNode(staticNode, segments, index+1, params)

		if handler != nil {
			return handler
		}
	}

	// 2. Static node matching failed, attempting dynamic node
	for _, vn := range root.varNode {

		// Record the current dynamic parameters
		params[vn.segment] = segment

		handler := searchNode(vn, segments, index+1, params)

		if handler != nil {
			return handler
		}

		// The current branch matching failed, delete the parameters just recorded
		delete(params, vn.segment)
	}

	return nil
}

