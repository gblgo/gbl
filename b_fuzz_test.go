// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// fuzzJSONFilterNode exercises nested exact-field filtering, including recursion
// through slices, maps, and pointers.
type fuzzJSONFilterNode struct {
	Value    int                           `json:"value"`
	Children []fuzzJSONFilterNode          `json:"children"`
	Lookup   map[string]fuzzJSONFilterNode `json:"lookup"`
}

// fuzzJSONFilterTypes covers concrete and interface-shaped JSON containers.
var fuzzJSONFilterTypes = []reflect.Type{
	reflect.TypeFor[fuzzJSONFilterNode](),
	reflect.TypeFor[*fuzzJSONFilterNode](),
	reflect.TypeFor[[]fuzzJSONFilterNode](),
	reflect.TypeFor[[2]fuzzJSONFilterNode](),
	reflect.TypeFor[map[string]fuzzJSONFilterNode](),
	reflect.TypeFor[[]any](),
	reflect.TypeFor[map[string]any](),
}

// FuzzFilterExactJSONValueNoPanic rejects non-JSON inputs and requires every
// filtered result to remain valid JSON. The fuzz engine treats any panic as a
// failure.
func FuzzFilterExactJSONValueNoPanic(f *testing.F) {
	f.Add([]byte(`{}`), uint8(0))
	f.Add([]byte(`{"value":1,"unknown":{"nested":[true,null,"x"]}}`), uint8(0))
	f.Add([]byte(`{"\u0076alue":2,"Value":3}`), uint8(0))
	f.Add([]byte(`[{"value":1},{"value":2}]`), uint8(2))
	f.Add([]byte(`{"value":"bad","value":1}`), uint8(0))
	f.Add([]byte(`null`), uint8(1))
	f.Add([]byte(`{"key":{"value":7},"other":[{"value":8}]}`), uint8(4))
	f.Fuzz(func(t *testing.T, data []byte, kind uint8) {
		if !json.Valid(data) {
			t.Skip()
		}
		typ := fuzzJSONFilterTypes[int(kind)%len(fuzzJSONFilterTypes)]
		filtered := filterExactJSONValue(data, typ)
		if !json.Valid(filtered) {
			t.Fatalf("filterExactJSONValue produced invalid JSON for type %v: input=%q output=%q",
				typ, data, filtered)
		}
	})
}

// FuzzWriteSSEDataNoPanic requires arbitrary bytes to produce one correctly
// framed SSE event without panicking. CRLF and bare CR normalize to LF, and a
// trailing line break remains visible as a final empty data field.
func FuzzWriteSSEDataNoPanic(f *testing.F) {
	seeds := [][]byte{
		{},
		[]byte("plain"),
		[]byte("line1\nline2"),
		[]byte("line1\n"),
		[]byte("\r\n"),
		[]byte("a\rb\r\nc\n"),
		[]byte("data: \x00\xff"),
	}
	for _, seed := range seeds {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		recorder := httptest.NewRecorder()
		if err := writeSSEData(recorder, data); err != nil {
			t.Fatalf("writeSSEData() error = %v", err)
		}
		normalized := strings.ReplaceAll(string(data), "\r\n", "\n")
		normalized = strings.ReplaceAll(normalized, "\r", "\n")
		want := "data: " + strings.ReplaceAll(normalized, "\n", "\ndata: ") + "\n\n"
		if got := recorder.Body.String(); got != want {
			t.Fatalf("writeSSEData(%q) = %q, want %q", data, got, want)
		}
	})
}

// FuzzSplitPathIntoNoPanic requires arbitrary paths to split into the same
// non-empty slash-separated segments without panicking. Each input also checks
// nil, empty, pre-populated, and path-derived destination prefixes.
func FuzzSplitPathIntoNoPanic(f *testing.F) {
	for _, path := range []string{"", "/", "///", "/a//b/", "a/b", "/世界//\x00/"} {
		f.Add(path)
	}
	f.Fuzz(func(t *testing.T, path string) {
		for _, dst := range [][]string{nil, {}, {"prefix/"}, {"prefix/", ""}, {path}} {
			got := splitPathInto(append([]string(nil), dst...), path)
			want := append([]string(nil), dst...)
			for _, segment := range strings.Split(path, "/") {
				if segment != "" {
					want = append(want, segment)
				}
			}
			if !slices.Equal(got, want) {
				t.Fatalf("splitPathInto(%q, %q) = %q, want %q", dst, path, got, want)
			}
		}
	})
}
