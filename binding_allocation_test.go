// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

//go:build !race

package gbl

import (
	"reflect"
	"testing"
)

// TestJSONNeedsExactFilterAllExactAllocations verifies that index-based change
// detection consumes the whole value without allocating on the common path.
func TestJSONNeedsExactFilterAllExactAllocations(t *testing.T) {
	data := []byte(` {"map":{"Key":{"value":1}},"slice":[{"value":2},null],"array":[{"value":3},{"value":4}],"custom":{"Value":5},"bytes":"YQ=="} `)
	typ := reflect.TypeFor[CorrectnessJSONContainers]()
	if jsonNeedsExactFilter(data, typ) {
		t.Fatal("all-exact container payload requested filtering")
	}
	allocs := testing.AllocsPerRun(100, func() {
		correctnessNeedsExactSink = jsonNeedsExactFilter(data, typ)
	})
	if allocs != 0 {
		t.Fatalf("all-exact detection allocations = %.0f, want 0", allocs)
	}
	parser := jsonExactParser{data: data}
	if parser.parse(typ, nil) {
		t.Fatal("all-exact parser payload requested filtering")
	}
	if parser.index != len(data) {
		t.Fatalf("parser index = %d, want %d", parser.index, len(data))
	}
}

// TestFilterExactJSONValueDeepAllocationCount ensures the rewrite itself keeps
// exactly one shared output-buffer allocation at every depth.
func TestFilterExactJSONValueDeepAllocationCount(t *testing.T) {
	typ := reflect.TypeFor[*CorrectnessDeepNode]()
	_ = jsonStructFieldsOf(reflect.TypeFor[CorrectnessDeepNode]())
	for _, depth := range []int{64, 512, 1024} {
		body, _ := correctnessDeepJSON(depth)
		allocs := testing.AllocsPerRun(10, func() {
			correctnessFilteredSink = filterExactJSONValue(body, typ)
		})
		if allocs != 1 {
			t.Fatalf("depth %d rewrite allocations = %.0f, want 1", depth, allocs)
		}
	}
}
