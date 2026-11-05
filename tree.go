// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"fmt"
	"strings"
)

var roots = make([]*routeNode, methodCount)

type routeNode struct {
	segment    string
	path       string
	paramName  string
	exist      bool
	handler    *routeHandler
	staticNode map[string]*routeNode
	varNode    []*routeNode
}

type segmentMeta struct {
	segment   string
	paramName string
	isVar     bool
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

func hasSameRoute(node *routeNode, segments []segmentMeta, index int) (bool, string) {
	if node == nil {
		return false, null
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
		return false, null
	}

	next := node.staticNode[meta.segment]
	return hasSameRoute(next, segments, index+1)
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
						panic(fmt.Errorf("variable route conflicts: [%s %s]", path, existPath))
					}
				}

				node = newNode(meta.segment)
				node.paramName = meta.paramName
				root.varNode = append(root.varNode, node)
			}

			if isLast {
				if node.exist {
					panic(fmt.Errorf("variable route already exists: [%s %s]", path, node.path))
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
				panic(fmt.Errorf("static route already exists: [%s %s]", path, node.path))
			}

			node.exist = true
			node.handler = handler
			node.path = path
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

func matchNode(method Method, path string, params map[string]string) *routeHandler {

	segments := getLegalSegments(path)

	root := roots[method]
	if root == nil {
		return nil
	}

	handler := searchNode(root, segments, 0, params)

	if handler == nil {
		return nil
	}

	return handler
}

func searchNode(root *routeNode, segments []string, index int, params map[string]string) *routeHandler {

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
