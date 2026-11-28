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

func TestDecodeJSONFieldsListIntoAllocations(t *testing.T) {
	body := []byte(`{"id":42,"name":"alice","enabled":true,"noise":"ignored"}`)
	keys := []string{"id", "name", "enabled"}
	fields := acquireJSONFields(len(keys))
	defer releaseJSONFields(fields)
	var decodeErr error
	allocations := testing.AllocsPerRun(100, func() {
		clear(fields.values)
		decodeErr = decodeJSONFieldsListInto(body, keys, fields.values)
	})
	if decodeErr != nil {
		t.Fatalf("decode JSON fields: %v", decodeErr)
	}
	if allocations != 0 {
		t.Fatalf("JSON field selection allocations = %.0f, want 0", allocations)
	}
}

func TestDecodeJSONFieldsTableIntoAllocations(t *testing.T) {
	body := []byte(`{"id":42,"name":"alice","enabled":true,"city":"GZ","country":"CN","email":"a@example.com","phone":"1","status":"ok","role":"user","noise":"ignored"}`)
	keys := []string{"id", "name", "enabled", "city", "country", "email", "phone", "status", "role"}
	table := buildJSONKeyTable(keys)
	if table == nil {
		t.Fatal("JSON key table was not built for a large parameter list")
	}
	fields := acquireJSONFields(len(keys))
	defer releaseJSONFields(fields)
	var decodeErr error
	allocations := testing.AllocsPerRun(100, func() {
		clear(fields.values)
		decodeErr = decodeJSONFieldsInto(body, keys, table, fields.values)
	})
	if decodeErr != nil {
		t.Fatalf("decode JSON fields through table: %v", decodeErr)
	}
	if allocations != 0 {
		t.Fatalf("JSON key-table field selection allocations = %.0f, want 0", allocations)
	}
}

func TestContextValidatablePointerModeAvoidsBoxing(t *testing.T) {
	value := ctxTestValidatable{Name: "value"}
	addressable := reflect.ValueOf(&value).Elem()
	var target Validatable
	var ok bool
	allocations := testing.AllocsPerRun(100, func() {
		target, ok = asValidatableMode(addressable, true)
	})
	if allocations != 0 {
		t.Fatalf("pointer-mode validation allocations = %v, want 0", allocations)
	}
	if !ok || target != &value {
		t.Fatalf("pointer-mode target = (%v, %t), want original pointer", target, ok)
	}
	if target, ok := asValidatableMode(reflect.Value{}, true); ok || target != nil {
		t.Fatalf("invalid pointer-mode value = (%v, %t), want (nil, false)", target, ok)
	}
	if target, ok := asValidatableMode(reflect.ValueOf(value), true); ok || target != nil {
		t.Fatalf("unaddressable pointer-mode value = (%v, %t), want (nil, false)", target, ok)
	}
	plain := struct{ Name string }{}
	if target, ok := asValidatableMode(reflect.ValueOf(&plain).Elem(), true); ok || target != nil {
		t.Fatalf("plain pointer-mode value = (%v, %t), want (nil, false)", target, ok)
	}
}
