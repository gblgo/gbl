// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
)

// This file holds JSON field selection and exact struct decoding.

// decodeJSONFields validates one JSON object and retains only the raw fields
// named by keys. Duplicate names keep the last value, matching encoding/json map
// semantics, and the returned slices alias body until binding completes.
func decodeJSONFields(body []byte, keys []string) (map[string]json.RawMessage, error) {
	return decodeJSONFieldsList(body, keys)
}

// maxPooledJSONFieldKeys bounds retained multi-parameter JSON field maps.
const maxPooledJSONFieldKeys = 64

// pooledJSONFields owns one request-scoped raw-member map.
type pooledJSONFields struct {
	values map[string]json.RawMessage
	large  bool
}

// jsonFieldsPool reuses maps whose RawMessage values alias the request body only during binding.
var jsonFieldsPool = sync.Pool{
	New: func() any {
		return &pooledJSONFields{values: make(map[string]json.RawMessage)}
	},
}

// acquireJSONFields clears a pooled field map before decoding.
func acquireJSONFields(keyCount int) *pooledJSONFields {
	fields := jsonFieldsPool.Get().(*pooledJSONFields)
	clear(fields.values)
	fields.large = keyCount > maxPooledJSONFieldKeys
	return fields
}

// releaseJSONFields drops body aliases and pools only bounded maps.
func releaseJSONFields(fields *pooledJSONFields) {
	if fields == nil {
		return
	}
	clear(fields.values)
	if !fields.large {
		jsonFieldsPool.Put(fields)
	}
}

// jsonKeySlot is one immutable open-addressed JSON key entry. index is -1 for
// an empty slot so blank parameter names cannot look occupied.
type jsonKeySlot struct {
	hash  uint64
	key   string
	index int
}

// Small parameter lists are faster to scan directly; larger lists use the
// startup-built table to avoid request-time O(parameters × fields) work.
const jsonKeyLinearScanLimit = 8

const (
	jsonKeyHashOffset uint64 = 14695981039346656037
	jsonKeyHashPrime  uint64 = 1099511628211
)

func buildJSONKeyTable(keys []string) []jsonKeySlot {
	if len(keys) <= jsonKeyLinearScanLimit {
		return nil
	}
	size := 1
	for size < len(keys)*2 {
		size <<= 1
	}
	table := make([]jsonKeySlot, size)
	for i := range table {
		table[i].index = -1
	}
	for i, key := range keys {
		hash := hashJSONKeyString(key)
		slot := hash & uint64(size-1)
		for table[slot].index >= 0 {
			slot = (slot + 1) & uint64(size-1)
		}
		table[slot] = jsonKeySlot{hash: hash, key: key, index: i}
	}
	return table
}

func jsonKeyIndexTable(table []jsonKeySlot, raw []byte) int {
	if len(table) == 0 {
		return -1
	}
	hash := hashJSONKeyBytes(raw)
	slot := hash & uint64(len(table)-1)
	for {
		entry := &table[slot]
		if entry.index < 0 {
			return -1
		}
		if entry.hash == hash && equalJSONKeyBytes(entry.key, raw) {
			return entry.index
		}
		slot = (slot + 1) & uint64(len(table)-1)
	}
}

func jsonKeyIndexTableString(table []jsonKeySlot, name string) int {
	if len(table) == 0 {
		return -1
	}
	hash := hashJSONKeyString(name)
	slot := hash & uint64(len(table)-1)
	for {
		entry := &table[slot]
		if entry.index < 0 {
			return -1
		}
		if entry.hash == hash && entry.key == name {
			return entry.index
		}
		slot = (slot + 1) & uint64(len(table)-1)
	}
}

func hashJSONKeyBytes(key []byte) uint64 {
	hash := jsonKeyHashOffset
	for _, b := range key {
		hash ^= uint64(b)
		hash *= jsonKeyHashPrime
	}
	return hash
}

func hashJSONKeyString(key string) uint64 {
	hash := jsonKeyHashOffset
	for i := range len(key) {
		hash ^= uint64(key[i])
		hash *= jsonKeyHashPrime
	}
	return hash
}

func equalJSONKeyBytes(key string, raw []byte) bool {
	if len(key) != len(raw) {
		return false
	}
	for i := range len(raw) {
		if key[i] != raw[i] {
			return false
		}
	}
	return true
}

// decodeJSONFieldsList is the direct-call variant with a fresh map.
func decodeJSONFieldsList(body []byte, keys []string) (map[string]json.RawMessage, error) {
	fields := make(map[string]json.RawMessage, len(keys))
	if err := decodeJSONFieldsListInto(body, keys, fields); err != nil {
		return nil, err
	}
	return fields, nil
}

// decodeJSONFieldsSet adapts the startup key set for direct callers.
func decodeJSONFieldsSet(body []byte, keys requestKeySet) (map[string]json.RawMessage, error) {
	list := make([]string, 0, len(keys))
	for key := range keys {
		list = append(list, key)
	}
	fields := make(map[string]json.RawMessage, len(keys))
	if err := decodeJSONFieldsListInto(body, list, fields); err != nil {
		return nil, err
	}
	return fields, nil
}

// decodeJSONFieldsListInto fills a caller-owned map for direct list callers.
func decodeJSONFieldsListInto(body []byte, keys []string,
	fields map[string]json.RawMessage) error {
	return decodeJSONFieldsInto(body, keys, nil, fields)
}

// decodeJSONFieldsInto uses the immutable startup table on the request path and
// retains the list lookup for direct callers that do not have a bind plan.
func decodeJSONFieldsInto(body []byte, keys []string, table []jsonKeySlot,
	fields map[string]json.RawMessage) error {
	if !json.Valid(body) {
		return fmt.Errorf("invalid JSON body: %w", json.Unmarshal(body, &json.RawMessage{}))
	}

	index := skipJSONSpace(body, 0)
	if index+4 <= len(body) && bytes.Equal(body[index:index+4], []byte("null")) {
		return multipleRequireError
	}
	if body[index] != '{' {
		return errors.New("invalid JSON body: expected an object")
	}

	index = skipJSONSpace(body, index+1)
	for body[index] != '}' {
		keyStart := index
		index = scanJSONStringEnd(body, index)
		rawKey := body[keyStart+1 : index-1]
		var keyIndex int
		if table != nil {
			keyIndex = jsonKeyIndexTable(table, rawKey)
		} else {
			keyIndex = jsonKeyIndex(keys, rawKey)
		}
		if keyIndex < 0 && bytes.IndexByte(rawKey, '\\') >= 0 {
			// Escaped keys are uncommon; standard decoding preserves exact JSON semantics.
			var name string
			_ = json.Unmarshal(body[keyStart:index], &name)
			if table != nil {
				keyIndex = jsonKeyIndexTableString(table, name)
			} else {
				keyIndex = jsonKeyIndexString(keys, name)
			}
		}
		index = skipJSONSpace(body, index)
		index = skipJSONSpace(body, index+1)
		valueStart := index
		index = skipJSONValueEnd(body, index)
		if keyIndex >= 0 {
			fields[keys[keyIndex]] = body[valueStart:index]
		}
		index = skipJSONSpace(body, index)
		switch body[index] {
		case ',':
			index = skipJSONSpace(body, index+1)
		case '}':
			return nil
		}
	}
	return nil
}

// jsonKeyIndex matches an unescaped JSON key against the startup key slice.
func jsonKeyIndex(keys []string, raw []byte) int {
	for i, key := range keys {
		if len(key) != len(raw) {
			continue
		}
		matched := true
		for j := range len(raw) {
			if key[j] != raw[j] {
				matched = false
				break
			}
		}
		if matched {
			return i
		}
	}
	return -1
}

func jsonKeyIndexString(keys []string, name string) int {
	for i, key := range keys {
		if key == name {
			return i
		}
	}
	return -1
}

// skipJSONSpace returns the first non-whitespace position at or after index.
func skipJSONSpace(body []byte, index int) int {
	for index < len(body) {
		switch body[index] {
		case ' ', '\t', '\r', '\n':
			index++
		default:
			return index
		}
	}
	return index
}

// scanJSONStringEnd returns the position after a valid JSON string.
func scanJSONStringEnd(body []byte, index int) int {
	index++
	for body[index] != '"' {
		if body[index] == '\\' {
			index++
		}
		index++
	}
	return index + 1
}

// skipJSONValueEnd returns the end of one already validated JSON value.
func skipJSONValueEnd(body []byte, index int) int {
	switch body[index] {
	case '"':
		return scanJSONStringEnd(body, index)
	case '{', '[':
		depth := 0
		for index < len(body) {
			switch body[index] {
			case '"':
				index = scanJSONStringEnd(body, index)
				continue
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return index + 1
				}
			}
			index++
		}
		return index
	default:
		for index < len(body) {
			switch body[index] {
			case ',', '}', ']', ' ', '\t', '\r', '\n':
				return index
			}
			index++
		}
		return index
	}
}

// decodeJSON unmarshals data into a fresh value of type t and returns the
// populated value (not a pointer to it).
func decodeJSON(t reflect.Type, data []byte) (reflect.Value, error) {
	if t == nil {
		return reflect.Value{}, errors.New("nil JSON target type")
	}
	result := reflect.New(t)
	if err := json.Unmarshal(data, result.Interface()); err != nil {
		return reflect.Value{}, err
	}
	return result.Elem(), nil
}

// decodeJSONExactStruct applies the binding subsystem's exact member names to a
// lone business parameter, including structs nested in top-level containers.
// Valid values are filtered recursively before the standard decoder runs,
// preserving its pointer, slice, map, optional-field and UnmarshalTypeError
// behavior. Types with their own JSON decoder keep ownership of their
// representation.
func decodeJSONExactStruct(t reflect.Type, data []byte) (reflect.Value, error) {
	if t == nil {
		return reflect.Value{}, errors.New("nil JSON target type")
	}
	terminal, ok := derefType(t)
	if ok && !hasCustomJSONDecoder(terminal) && json.Valid(data) {
		if jsonNeedsExactFilter(data, terminal) {
			data = filterExactJSONValue(data, terminal)
		}
	}
	result := reflect.New(t)
	if err := json.Unmarshal(data, result.Interface()); err != nil {
		return reflect.Value{}, err
	}
	return result.Elem(), nil
}

// hasCustomJSONDecoder reports whether t owns its JSON representation.
func hasCustomJSONDecoder(t reflect.Type) bool {
	return t.Implements(jsonUnmarshalerType) || reflect.PointerTo(t).Implements(jsonUnmarshalerType)
}

// jsonNeedsExactFilter reports whether removing a wrong-case member or filtering
// a nested struct would change data. The linear index parser traverses known
// containers directly, and the common all-exact path allocates nothing.
func jsonNeedsExactFilter(data []byte, t reflect.Type) bool {
	parser := jsonExactParser{data: data}
	return parser.parse(t, nil)
}

// filterExactJSONValue rewrites an already validated value in O(N) time. Known
// containers recurse directly instead of being skipped and then scanned again;
// scalar, custom-decoder and shape-mismatched values are copied once into one
// shared buffer. Every exact member occurrence retains its order, so the
// standard decoder still reports a type error in an earlier duplicate even when
// a later duplicate has a valid value.
func filterExactJSONValue(data []byte, t reflect.Type) []byte {
	t, ok := derefType(t)
	if !ok || hasCustomJSONDecoder(t) {
		return data
	}
	parser := jsonExactParser{data: data}
	parser.index = skipJSONSpace(data, 0)
	switch t.Kind() {
	case reflect.Struct, reflect.Map:
		if parser.index == len(data) || data[parser.index] != '{' {
			return data
		}
	case reflect.Array, reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 ||
			parser.index == len(data) ||
			data[parser.index] != '[' {
			return data
		}
	default:
		return data
	}

	var out bytes.Buffer
	out.Grow(len(data))
	parser.value(t, &out)
	return out.Bytes()
}

// jsonExactParser advances one index through validated JSON. A nil output buffer
// selects change detection instead of writing a minified exact-field value.
type jsonExactParser struct {
	// data holds the already validated input.
	data []byte
	// index is the next unread input position.
	index int
}

// parse consumes one value and its trailing JSON whitespace.
func (p *jsonExactParser) parse(t reflect.Type, out *bytes.Buffer) bool {
	p.index = skipJSONSpace(p.data, 0)
	changed := p.value(t, out)
	p.index = skipJSONSpace(p.data, p.index)
	return changed
}

// value consumes one value, recursing directly into matching known containers.
// Scalar, custom-decoder and shape-mismatched values are retained verbatim.
func (p *jsonExactParser) value(t reflect.Type, out *bytes.Buffer) bool {
	if p.index == len(p.data) || t == nil {
		return false
	}
	t, ok := derefType(t)
	if !ok || hasCustomJSONDecoder(t) {
		p.copyRawValue(out)
		return false
	}
	switch t.Kind() {
	case reflect.Struct:
		if p.data[p.index] != '{' {
			p.copyRawValue(out)
			return false
		}
		return p.object(t, jsonStructFieldsOf(t), true, out)
	case reflect.Map:
		if p.data[p.index] != '{' {
			p.copyRawValue(out)
			return false
		}
		return p.object(t, nil, false, out)
	case reflect.Array, reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 || p.data[p.index] != '[' {
			p.copyRawValue(out)
			return false
		}
		return p.array(t.Elem(), out)
	default:
		p.copyRawValue(out)
		return false
	}
}

// object consumes an object, filtering struct members or retaining every map
// member while recursively handling its values.
func (p *jsonExactParser) object(
	t reflect.Type,
	fields map[string]reflect.Type,
	filterMembers bool,
	out *bytes.Buffer,
) bool {
	p.index++
	if out != nil {
		out.WriteByte('{')
	}
	p.index = skipJSONSpace(p.data, p.index)
	first := true
	changed := false
	for p.index < len(p.data) && p.data[p.index] != '}' {
		rawKey, escaped := p.scanKey()
		p.index = skipJSONSpace(p.data, p.index)
		p.index++
		p.index = skipJSONSpace(p.data, p.index)

		var fieldType reflect.Type
		exists := true
		if filterMembers {
			fieldType, exists = jsonFieldTypeByKey(fields, rawKey, escaped)
		} else {
			fieldType = t.Elem()
		}
		if !exists {
			changed = true
			p.discardRawValue()
			if out == nil {
				return true
			}
		} else {
			if out != nil {
				if !first {
					out.WriteByte(',')
				}
				out.Write(rawKey)
				out.WriteByte(':')
			}
			if p.value(fieldType, out) {
				changed = true
			}
			if out == nil && changed {
				return true
			}
			first = false
		}

		p.index = skipJSONSpace(p.data, p.index)
		if p.index < len(p.data) && p.data[p.index] == ',' {
			p.index = skipJSONSpace(p.data, p.index+1)
		}
	}
	p.index++
	if out != nil {
		out.WriteByte('}')
	}
	return changed
}

// array consumes an array and recursively handles each element.
func (p *jsonExactParser) array(elem reflect.Type, out *bytes.Buffer) bool {
	p.index++
	if out != nil {
		out.WriteByte('[')
	}
	p.index = skipJSONSpace(p.data, p.index)
	first := true
	changed := false
	for p.index < len(p.data) && p.data[p.index] != ']' {
		if out != nil {
			if !first {
				out.WriteByte(',')
			}
			first = false
		}
		if p.value(elem, out) {
			changed = true
		}
		if out == nil && changed {
			return true
		}
		p.index = skipJSONSpace(p.data, p.index)
		if p.index < len(p.data) && p.data[p.index] == ',' {
			p.index = skipJSONSpace(p.data, p.index+1)
		}
	}
	p.index++
	if out != nil {
		out.WriteByte(']')
	}
	return changed
}

// scanKey consumes one JSON string and reports whether it contains an escape.
// Validated input always has a closing quote; the fallback retains the consumed
// prefix defensively when this helper receives an unterminated string.
func (p *jsonExactParser) scanKey() ([]byte, bool) {
	start := p.index
	p.index++
	escaped := false
	for p.index < len(p.data) {
		switch p.data[p.index] {
		case '"':
			p.index++
			return p.data[start:p.index], escaped
		case '\\':
			escaped = true
			p.index += 2
		default:
			p.index++
		}
	}
	return p.data[start:p.index], escaped
}

// copyRawValue consumes and retains one value without inspecting its target type.
func (p *jsonExactParser) copyRawValue(out *bytes.Buffer) {
	end := skipJSONValueEnd(p.data, p.index)
	if out != nil {
		out.Write(p.data[p.index:end])
	}
	p.index = end
}

// discardRawValue consumes one value without retaining it.
func (p *jsonExactParser) discardRawValue() {
	p.index = skipJSONValueEnd(p.data, p.index)
}

// jsonFieldTypeByKey uses the parser's escape flag to avoid rescanning raw keys.
func jsonFieldTypeByKey(fields map[string]reflect.Type, rawKey []byte,
	escaped bool) (reflect.Type, bool) {
	if !escaped {
		fieldType, exists := fields[string(rawKey[1:len(rawKey)-1])]
		return fieldType, exists
	}
	var name string
	_ = json.Unmarshal(rawKey, &name)
	fieldType, exists := fields[name]
	return fieldType, exists
}

// jsonFirstNonSpace returns the first non-whitespace byte or zero for empty data.
func jsonFirstNonSpace(data []byte) byte {
	index := skipJSONSpace(data, 0)
	if index == len(data) {
		return 0
	}
	return data[index]
}

// walkJSONObject calls visit with raw keys and values for each member of an
// already validated object. It starts at the first non-whitespace opening brace,
// so leading whitespace neither hides fields nor makes an empty object panic.
// Iteration stops early when visit returns true.
func walkJSONObject(data []byte, visit func(rawKey, value []byte) bool) bool {
	index := skipJSONSpace(data, 0)
	if index == len(data) || data[index] != '{' {
		return false
	}
	index = skipJSONSpace(data, index+1)
	for index < len(data) && data[index] != '}' {
		keyStart := index
		keyEnd := scanJSONStringEnd(data, keyStart)
		index = skipJSONSpace(data, keyEnd)
		valueStart := skipJSONSpace(data, index+1)
		valueEnd := skipJSONValueEnd(data, valueStart)
		if visit(data[keyStart:keyEnd], data[valueStart:valueEnd]) {
			return true
		}
		index = skipJSONSpace(data, valueEnd)
		if index < len(data) && data[index] == ',' {
			index = skipJSONSpace(data, index+1)
		}
	}
	return false
}

// visitJSONObject decodes each raw key and calls visit for every object member.
// Iteration stops early when visit returns true.
func visitJSONObject(data []byte, visit func(name string, rawKey, value []byte) bool) bool {
	return walkJSONObject(data, func(rawKey, value []byte) bool {
		var name string
		// The whole object is already valid, so decoding its key cannot fail.
		_ = json.Unmarshal(rawKey, &name)
		return visit(name, rawKey, value)
	})
}

// walkJSONArray calls visit for each element of an already validated array. It
// starts at the first non-whitespace opening bracket and handles empty arrays.
// Iteration stops early when visit returns true.
func walkJSONArray(data []byte, visit func(value []byte) bool) bool {
	index := skipJSONSpace(data, 0)
	if index == len(data) || data[index] != '[' {
		return false
	}
	index = skipJSONSpace(data, index+1)
	for index < len(data) && data[index] != ']' {
		valueEnd := skipJSONValueEnd(data, index)
		if visit(data[index:valueEnd]) {
			return true
		}
		index = skipJSONSpace(data, valueEnd)
		if index < len(data) && data[index] == ',' {
			index = skipJSONSpace(data, index+1)
		}
	}
	return false
}

// visitJSONArray calls visit for each element of an already validated array.
// Iteration stops early when visit returns true.
func visitJSONArray(data []byte, visit func(value []byte) bool) bool {
	return walkJSONArray(data, visit)
}

// wrapPointer boxes a bound element value into a fresh pointer of type t,
// the shared tail of the pointer branches in the form and text binders.
func wrapPointer(t reflect.Type, elem reflect.Value) reflect.Value {
	result := reflect.New(t.Elem())
	result.Elem().Set(elem)
	return result
}
