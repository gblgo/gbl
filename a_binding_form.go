// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"fmt"
	"mime/multipart"
	"net/url"
	"reflect"
	"slices"
	"strconv"
)

// This file holds form/query sources, reflection form binding, uploaded
// file headers, and scalar text conversion.

// nameSources bundles the three by-name request sources a binder reads. It
// lives inside the pooled Context, so priming it costs no allocation. Lookups
// are exact: the request key must equal the bind name byte-for-byte. Struct
// field names are pre-lowercased at registration so clients can send lowercase
// keys without tags; the request data itself is never transformed.
type nameSources struct {
	// path holds matched path parameters
	path map[string]string
	// form holds query and body form values
	form url.Values
	// files holds multipart file headers by field name
	files filesMap
}

// pathGet looks up an exact path-parameter name.
func (s *nameSources) pathGet(name string) (string, bool) {
	v, ok := s.path[name]
	return v, ok
}

// formGet looks up an exact query or form key.
func (s *nameSources) formGet(name string) ([]string, bool) {
	v, ok := s.form[name]
	return v, ok
}

// has reports whether any request source carries name directly. Recursive
// pointer allocation probes individual leaf fields rather than inferring data
// from the outer struct parameter name.
func (s *nameSources) has(name string) bool {
	if _, ok := s.path[name]; ok {
		return true
	}
	if _, ok := s.form[name]; ok {
		return true
	}
	_, ok := s.files[name]
	return ok
}

// filesGet looks up an exact multipart field name.
func (s *nameSources) filesGet(name string) ([]*multipart.FileHeader, bool) {
	v, ok := s.files[name]
	return v, ok
}

// lookupString finds a map value by exact key. It serves one-off lookups outside
// the binding path (Context.PathParam); request keys are matched verbatim.
func lookupString(values map[string]string, name string) (string, bool) {
	value, ok := values[name]
	return value, ok
}

// bindFormParameter binds one non-JSON parameter of any kind: pointers stay nil
// when their source is absent, except that non-recursive struct pointers are
// allocated for field binding and recursive struct pointers are allocated when
// a reachable leaf field has data. Structs recurse over their fields, file header
// types pull from uploaded files, slices/arrays consume repeated values, and
// scalars take the first value. name is matched exactly.
func bindFormParameter(t reflect.Type, name string, src *nameSources) (reflect.Value, error) {
	var state formBindState
	return bindFormParameterDepth(t, name, src, 0, &state)
}

// formBindState tracks the struct types on the current form-binding branch.
// The inline stack avoids a per-request map while still distinguishing repeated
// sibling fields (which are popped) from an actual recursive type cycle.
type formBindState struct {
	// active holds the struct types on the current binding branch.
	active [maxTypePointerDepth]reflect.Type
	// size is the number of active entries.
	size int
	// allocations bounds eager struct and pointer construction per request.
	allocations int
}

// contains reports whether t is already active on the current branch.
func (s *formBindState) contains(t reflect.Type) bool {
	return slices.Contains(s.active[:s.size], t)
}

// push adds t to the active branch and rejects excessive nesting.
func (s *formBindState) push(t reflect.Type) error {
	if s.size >= len(s.active) {
		return fmt.Errorf("struct nesting exceeds %d levels", len(s.active))
	}
	s.active[s.size] = t
	s.size++
	return nil
}

// allocate records one eager form value and rejects explosive request-time trees.
func (s *formBindState) allocate() error {
	s.allocations++
	if s.allocations > maxFormPointerExpansion {
		return fmt.Errorf("form pointer expansion exceeds %d nodes", maxFormPointerExpansion)
	}
	return nil
}

// pop removes the most recently active struct type.
func (s *formBindState) pop() {
	s.size--
	s.active[s.size] = nil
}

// hasStructLeafSource reports whether any non-struct or file field reachable
// from t has request data. Recursive back-edges are ignored on the active branch;
// the actual binder still uses state.contains to stop the resulting object walk.
func hasStructLeafSource(t reflect.Type, src *nameSources,
	state *formBindState) (bool, error) {
	t, ok := derefType(t)
	if !ok || t.Kind() != reflect.Struct || state.contains(t) {
		return false, nil
	}
	if err := state.push(t); err != nil {
		return false, err
	}
	defer state.pop()

	for _, field := range structFieldsOf(t) {
		if isFileHeaderType(field.typ) {
			if _, exists := src.filesGet(field.name); exists {
				return true, nil
			}
			continue
		}
		fieldType, ok := derefType(field.typ)
		if !ok {
			continue
		}
		if fieldType.Kind() == reflect.Struct {
			present, err := hasStructLeafSource(fieldType, src, state)
			if err != nil || present {
				return present, err
			}
			continue
		}
		if src.has(field.name) {
			return true, nil
		}
	}
	return false, nil
}

// bindFormParameterDepth bounds pointer chains and stops recursive struct walks
// at request time. A recursive struct pointer stays nil unless a reachable leaf
// field has request data; a missing non-recursive struct pointer retains its
// historical zero-value allocation semantics.
func bindFormParameterDepth(t reflect.Type, name string, src *nameSources, depth int,
	state *formBindState) (reflect.Value, error) {
	// The file-header shapes are checked first: multipart.FileHeader is itself a
	// struct, and letting the struct branch below catch it would bind the
	// header's exported fields from form keys instead of the uploaded file.
	if isFileHeaderType(t) {
		headers, _ := src.filesGet(name)
		return bindFileHeaders(t, headers)
	}

	if t.Kind() == reflect.Pointer {
		if depth >= maxTypePointerDepth {
			return reflect.Value{}, fmt.Errorf("recursive pointer type: %s", t)
		}
		if terminal, ok := derefType(t); ok && terminal.Kind() == reflect.Struct &&
			isRecursiveStruct(terminal) {
			if state.contains(terminal) {
				return reflect.Zero(t), nil
			}
			hasLeaf, err := hasStructLeafSource(terminal, src, state)
			if err != nil {
				return reflect.Value{}, err
			}
			if !hasLeaf {
				return reflect.Zero(t), nil
			}
		}
		// A missing non-struct pointer stays nil; a pointer chain leading to a
		// non-recursive struct is allocated so its fields can be bound individually.
		if _, ok := src.pathGet(name); !ok {
			if _, ok = src.formGet(name); !ok {
				if _, ok = src.filesGet(name); !ok && !selfDescribing(t) {
					return reflect.Zero(t), nil
				}
			}
		}
		if terminal, ok := derefType(t); ok && terminal.Kind() == reflect.Struct {
			if err := state.allocate(); err != nil {
				return reflect.Value{}, err
			}
		}
		value, err := bindFormParameterDepth(t.Elem(), name, src, depth+1, state)
		if err != nil {
			return reflect.Value{}, err
		}
		return wrapPointer(t, value), nil
	}

	if t.Kind() == reflect.Struct {
		return bindStructFieldsState(t, structFieldsOf(t), src, state)
	}

	values, _ := src.formGet(name)
	switch t.Kind() {
	case reflect.Array, reflect.Slice:
		// A byte slice in a form field is the raw bytes of its first string.
		// Construct it element by element: []byte is not convertible to a slice
		// whose byte element is a named type, even when their kinds are identical.
		if t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8 {
			if len(values) == 0 {
				return reflect.Zero(t), nil
			}
			data := values[0]
			result := reflect.MakeSlice(t, len(data), len(data))
			for i := range len(data) {
				result.Index(i).SetUint(uint64(data[i]))
			}
			return result, nil
		}
		var result reflect.Value
		if t.Kind() == reflect.Array {
			if len(values) == 0 {
				return reflect.Zero(t), nil
			}
			if len(values) != t.Len() {
				return reflect.Value{}, fmt.Errorf("array requires %d values, got %d", t.Len(), len(values))
			}
			result = reflect.New(t).Elem()
		} else {
			result = reflect.New(t).Elem()
			result.Set(reflect.MakeSlice(t, len(values), len(values)))
		}
		for i, text := range values {
			elem, err := bindTextValue(t.Elem(), []string{text})
			if err != nil {
				return reflect.Value{}, fmt.Errorf("%s index %d: %w", t.Kind(), i, err)
			}
			result.Index(i).Set(elem)
		}
		return result, nil

	default:
		return bindTextValue(t, values)
	}
}

// bindFormParameterInto writes a bound value directly into an existing destination.
func bindFormParameterInto(dst reflect.Value, name string, src *nameSources, depth int,
	state *formBindState) error {
	t := dst.Type()
	if isFileHeaderType(t) {
		headers, _ := src.filesGet(name)
		value, _ := bindFileHeaders(t, headers)
		dst.Set(value)
		return nil
	}

	if t.Kind() == reflect.Pointer {
		if depth >= maxTypePointerDepth {
			return fmt.Errorf("recursive pointer type: %s", t)
		}
		if terminal, ok := derefType(t); ok && terminal.Kind() == reflect.Struct &&
			isRecursiveStruct(terminal) {
			if state.contains(terminal) {
				dst.SetZero()
				return nil
			}
			hasLeaf, err := hasStructLeafSource(terminal, src, state)
			if err != nil {
				return err
			}
			if !hasLeaf {
				dst.SetZero()
				return nil
			}
		}
		if _, ok := src.pathGet(name); !ok {
			if _, ok = src.formGet(name); !ok {
				if _, ok = src.filesGet(name); !ok && !selfDescribing(t) {
					dst.SetZero()
					return nil
				}
			}
		}
		if terminal, ok := derefType(t); ok && terminal.Kind() == reflect.Struct {
			if err := state.allocate(); err != nil {
				return err
			}
		}
		if dst.IsNil() {
			dst.Set(reflect.New(t.Elem()))
		}
		return bindFormParameterInto(dst.Elem(), name, src, depth+1, state)
	}

	if t.Kind() == reflect.Struct {
		return bindStructFieldsInto(dst, structFieldsOf(t), src, state)
	}

	values, _ := src.formGet(name)
	if t.Kind() == reflect.Array || t.Kind() == reflect.Slice {
		if t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8 {
			if len(values) == 0 {
				dst.SetZero()
				return nil
			}
			data := values[0]
			slice := reflect.MakeSlice(t, len(data), len(data))
			copy(slice.Bytes(), data)
			dst.Set(slice)
			return nil
		}
		if len(values) == 0 {
			dst.SetZero()
			return nil
		}
		if t.Kind() == reflect.Array {
			if len(values) != t.Len() {
				return fmt.Errorf("array requires %d values, got %d", t.Len(), len(values))
			}
		} else {
			dst.Set(reflect.MakeSlice(t, len(values), len(values)))
		}
		for i, text := range values {
			if err := setTextValue(dst.Index(i), []string{text}); err != nil {
				return fmt.Errorf("%s index %d: %w", t.Kind(), i, err)
			}
		}
		return nil
	}
	return setTextValue(dst, values)
}

// bindStruct fills an exported-field struct from form and path values, matching
// each field's exact bind name (JSON tag, or the field name with its first letter
// lowercased). The field list is cached per type, so nested structs cost no
// per-request reflection introspection either.
func bindStruct(t reflect.Type, src *nameSources) (reflect.Value, error) {
	return bindStructFields(t, structFieldsOf(t), src)
}

// bindStructFields populates a fresh struct of type t from a precomputed field
// list, so the request path skips reflection-based field introspection entirely.
func bindStructFields(t reflect.Type, fields []structField,
	src *nameSources) (reflect.Value, error) {
	var state formBindState
	return bindStructFieldsState(t, fields, src, &state)
}

// fieldByIndex resolves a promoted field path and allocates nil embedded pointers.
func fieldByIndex(root reflect.Value, index []int) reflect.Value {
	current := root
	for _, position := range index[:len(index)-1] {
		next := current.Field(position)
		if next.Kind() == reflect.Pointer {
			if next.IsNil() {
				next.Set(reflect.New(next.Type().Elem()))
			}
			next = next.Elem()
		}
		current = next
	}
	return current.Field(index[len(index)-1])
}

// bindStructFieldsState allocates one root value and binds fields directly into it.
func bindStructFieldsState(t reflect.Type, fields []structField, src *nameSources,
	state *formBindState) (reflect.Value, error) {
	result := reflect.New(t).Elem()
	if err := bindStructFieldsInto(result, fields, src, state); err != nil {
		return reflect.Value{}, err
	}
	return result, nil
}

// bindStructFieldsInto binds nested fields without allocating temporary field values.
func bindStructFieldsInto(result reflect.Value, fields []structField, src *nameSources,
	state *formBindState) error {
	t := result.Type()
	if state.contains(t) {
		return nil
	}
	if err := state.push(t); err != nil {
		return err
	}
	defer state.pop()
	if err := state.allocate(); err != nil {
		return err
	}

	for _, field := range fields {
		dst := fieldByIndex(result, field.index)
		if pathValue, ok := src.pathGet(field.name); ok {
			if err := setTextValue(dst, []string{pathValue}); err != nil {
				return fmt.Errorf("path field %q: %w", field.name, err)
			}
			continue
		}
		if err := bindFormParameterInto(dst, field.name, src, 0, state); err != nil {
			return fmt.Errorf("field %q: %w", field.name, err)
		}
	}
	return nil
}

// isFileHeaderType reports whether t is one of the four accepted file
// parameter shapes: multipart.FileHeader, *multipart.FileHeader,
// []*multipart.FileHeader or []multipart.FileHeader.
func isFileHeaderType(t reflect.Type) bool {
	return t == fileHeaderType || t == fileHeaderPtr || t == fileHeaderPs || t == fileHeaderSlice
}

// bindFileHeaders converts the looked-up file headers into the exact shape the
// handler parameter asks for, taking the first upload for single-value types.
func bindFileHeaders(t reflect.Type, headers []*multipart.FileHeader) (reflect.Value, error) {
	switch t {
	case fileHeaderPtr:
		if len(headers) == 0 {
			return reflect.Zero(t), nil
		}
		return reflect.ValueOf(headers[0]), nil
	case fileHeaderType:
		if len(headers) == 0 {
			return reflect.Zero(t), nil
		}
		return reflect.ValueOf(*headers[0]), nil
	case fileHeaderPs:
		return reflect.ValueOf(slices.Clone(headers)), nil
	case fileHeaderSlice:
		result := make([]multipart.FileHeader, len(headers))
		for i, header := range headers {
			if header != nil {
				result[i] = *header
			}
		}
		return reflect.ValueOf(result), nil
	default:
		return reflect.Value{}, fmt.Errorf("unsupported file parameter type: %s", t)
	}
}

// bindTextValue converts raw request strings into a scalar (or pointer to
// scalar) of type t. Only the first value is used; repeated values should be
// bound to a slice or array parameter instead.
func bindTextValue(t reflect.Type, values []string) (reflect.Value, error) {
	return bindTextValueDepth(t, values, 0)
}

// bindTextValueDepth bounds pointer recursion for path and scalar binding.
func bindTextValueDepth(t reflect.Type, values []string, depth int) (reflect.Value, error) {
	if len(values) == 0 {
		return reflect.New(t).Elem(), nil
	}
	if t.Kind() == reflect.Pointer && depth >= maxTypePointerDepth {
		return reflect.Value{}, fmt.Errorf("recursive pointer type: %s", t)
	}
	result := reflect.New(t).Elem()
	if err := setTextValueDepth(result, values, depth); err != nil {
		return reflect.Value{}, err
	}
	return result, nil
}

// setTextValue writes a scalar or scalar pointer directly into an existing field.
func setTextValue(dst reflect.Value, values []string) error {
	return setTextValueDepth(dst, values, 0)
}

// setTextValueDepth avoids allocating a temporary reflect.Value for every struct field.
func setTextValueDepth(dst reflect.Value, values []string, depth int) error {
	if dst.Kind() == reflect.Pointer {
		if depth >= maxTypePointerDepth {
			return fmt.Errorf("recursive pointer type: %s", dst.Type())
		}
		if len(values) == 0 {
			dst.SetZero()
			return nil
		}
		if dst.IsNil() {
			dst.Set(reflect.New(dst.Type().Elem()))
		}
		return setTextValueDepth(dst.Elem(), values, depth+1)
	}
	if len(values) == 0 {
		dst.SetZero()
		return nil
	}

	value := values[0]
	switch dst.Kind() {
	case reflect.String:
		dst.SetString(value)
	case reflect.Bool:
		v, err := strconv.ParseBool(value)
		if err != nil {
			return err
		}
		dst.SetBool(v)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v, err := strconv.ParseInt(value, 10, dst.Type().Bits())
		if err != nil {
			return err
		}
		dst.SetInt(v)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		v, err := strconv.ParseUint(value, 10, dst.Type().Bits())
		if err != nil {
			return err
		}
		dst.SetUint(v)
	case reflect.Float32, reflect.Float64:
		v, err := strconv.ParseFloat(value, dst.Type().Bits())
		if err != nil {
			return err
		}
		dst.SetFloat(v)
	case reflect.Complex64, reflect.Complex128:
		v, err := strconv.ParseComplex(value, dst.Type().Bits())
		if err != nil {
			return err
		}
		dst.SetComplex(v)
	default:
		return fmt.Errorf("unsupported parameter type: %s", dst.Type())
	}
	return nil
}
