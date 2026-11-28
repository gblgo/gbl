// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"bytes"
	"encoding/json"
	"errors"
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

// FuzzFilterExactJSONValueNoPanic requires valid JSON output, input immutability, and an exact-field fixed point.
func FuzzFilterExactJSONValueNoPanic(f *testing.F) {
	f.Add([]byte(`{}`), uint8(0))
	f.Add([]byte(`{"value":1,"unknown":{"nested":[true,null,"x"]}}`), uint8(0))
	f.Add([]byte(`{"\u0076alue":2,"Value":3}`), uint8(0))
	f.Add([]byte(`[{"value":1},{"value":2}]`), uint8(2))
	f.Add([]byte(`[{"Value":99,"value":1},{"value":2}]`), uint8(2))
	f.Add([]byte(`{"Value":99,"value":1,"children":[{"Value":2,"value":3}]}`), uint8(4))
	f.Add([]byte(`{"value":"bad","value":1}`), uint8(0))
	f.Add([]byte(`null`), uint8(1))
	f.Add([]byte(`{"key":{"value":7},"other":[{"value":8}]}`), uint8(4))
	f.Fuzz(func(t *testing.T, data []byte, kind uint8) {
		if !json.Valid(data) {
			t.Skip()
		}
		typ := fuzzJSONFilterTypes[int(kind)%len(fuzzJSONFilterTypes)]
		original := bytes.Clone(data)
		filtered := filterExactJSONValue(data, typ)
		if !bytes.Equal(data, original) {
			t.Fatalf("filterExactJSONValue mutated input: before=%q after=%q", original, data)
		}
		if !json.Valid(filtered) {
			t.Fatalf("filterExactJSONValue produced invalid JSON for type %v: input=%q output=%q",
				typ, data, filtered)
		}
		if jsonNeedsExactFilter(filtered, typ) {
			t.Fatalf("filterExactJSONValue did not reach an exact-field fixed point: input=%q output=%q",
				data, filtered)
		}
		actual := reflect.New(typ)
		actualErr := json.Unmarshal(filtered, actual.Interface())
		expected, expectedErr := fuzzJSONFilterOracle(data, typ)
		if (actualErr == nil) != (expectedErr == nil) {
			t.Fatalf("filtered decode error mismatch for type %v: input=%q output=%q actual=%v expected=%v",
				typ, data, filtered, actualErr, expectedErr)
		}
		if actualErr == nil && !reflect.DeepEqual(actual.Elem().Interface(), expected) {
			t.Fatalf("filtered value mismatch for type %v: input=%q output=%q got=%#v want=%#v",
				typ, data, filtered, actual.Elem().Interface(), expected)
		}
	})
}

// fuzzJSONObject preserves duplicate members and their order so the independent
// oracle can retain the standard decoder's first-type-error behavior.
type fuzzJSONObject []fuzzJSONMember

type fuzzJSONMember struct {
	key   string
	value any
}

func fuzzJSONDecodeValue(decoder *json.Decoder) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return token, nil
	}
	switch delim {
	case '{':
		var object fuzzJSONObject
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			value, err := fuzzJSONDecodeValue(decoder)
			if err != nil {
				return nil, err
			}
			object = append(object, fuzzJSONMember{key: keyToken.(string), value: value})
		}
		_, err = decoder.Token()
		return object, err
	case '[':
		var array []any
		for decoder.More() {
			value, err := fuzzJSONDecodeValue(decoder)
			if err != nil {
				return nil, err
			}
			array = append(array, value)
		}
		_, err = decoder.Token()
		return array, err
	default:
		return nil, errors.New("unexpected JSON delimiter")
	}
}

func fuzzJSONFilterOracle(data []byte, typ reflect.Type) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	value, err := fuzzJSONDecodeValue(decoder)
	if err != nil {
		return nil, err
	}
	filtered := fuzzJSONFilterOracleValue(value, typ)
	expectedData, err := fuzzJSONMarshalOracle(filtered)
	if err != nil {
		return nil, err
	}
	target := reflect.New(typ)
	if err := json.Unmarshal(expectedData, target.Interface()); err != nil {
		return nil, err
	}
	return target.Elem().Interface(), nil
}

func fuzzJSONFilterOracleValue(value any, typ reflect.Type) any {
	for typ.Kind() == reflect.Pointer {
		if value == nil {
			return nil
		}
		typ = typ.Elem()
	}
	switch typ {
	case reflect.TypeFor[fuzzJSONFilterNode]():
		return fuzzJSONFilterOracleNode(value)
	case reflect.TypeFor[[]fuzzJSONFilterNode](),
		reflect.TypeFor[[2]fuzzJSONFilterNode]():
		array, ok := value.([]any)
		if !ok {
			return value
		}
		filtered := make([]any, len(array))
		for i, item := range array {
			filtered[i] = fuzzJSONFilterOracleNode(item)
		}
		return filtered
	case reflect.TypeFor[map[string]fuzzJSONFilterNode]():
		object, ok := value.(fuzzJSONObject)
		if !ok {
			return value
		}
		filtered := make(fuzzJSONObject, len(object))
		for i, member := range object {
			filtered[i] = fuzzJSONMember{key: member.key, value: fuzzJSONFilterOracleNode(member.value)}
		}
		return filtered
	default:
		return value
	}
}

func fuzzJSONFilterOracleNode(value any) any {
	object, ok := value.(fuzzJSONObject)
	if !ok {
		return value
	}
	filtered := make(fuzzJSONObject, 0, len(object))
	for _, member := range object {
		switch member.key {
		case "value":
			filtered = append(filtered, member)
		case "children":
			filtered = append(filtered, fuzzJSONMember{key: member.key,
				value: fuzzJSONFilterOracleValue(member.value, reflect.TypeFor[[]fuzzJSONFilterNode]())})
		case "lookup":
			filtered = append(filtered, fuzzJSONMember{key: member.key,
				value: fuzzJSONFilterOracleValue(member.value, reflect.TypeFor[map[string]fuzzJSONFilterNode]())})
		}
	}
	return filtered
}

func fuzzJSONMarshalOracle(value any) ([]byte, error) {
	switch typed := value.(type) {
	case fuzzJSONObject:
		var out bytes.Buffer
		out.WriteByte('{')
		for i, member := range typed {
			if i > 0 {
				out.WriteByte(',')
			}
			key, err := json.Marshal(member.key)
			if err != nil {
				return nil, err
			}
			valueData, err := fuzzJSONMarshalOracle(member.value)
			if err != nil {
				return nil, err
			}
			out.Write(key)
			out.WriteByte(':')
			out.Write(valueData)
		}
		out.WriteByte('}')
		return out.Bytes(), nil
	case []any:
		var out bytes.Buffer
		out.WriteByte('[')
		for i, item := range typed {
			if i > 0 {
				out.WriteByte(',')
			}
			itemData, err := fuzzJSONMarshalOracle(item)
			if err != nil {
				return nil, err
			}
			out.Write(itemData)
		}
		out.WriteByte(']')
		return out.Bytes(), nil
	default:
		return json.Marshal(value)
	}
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
