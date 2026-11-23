// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// This file holds the parameter-binding subsystem: the precomputed bind plan,
// the per-request injection orchestration, and the scalar/struct/file/JSON
// binders it dispatches to. The request dispatch itself lives in handler.go.

type (
	// filesMap maps a multipart form field name to the headers of the files
	// uploaded under that name. It stays unexported on purpose: the binder
	// recognizes only the four multipart.FileHeader shapes as handler parameter
	// types, so this map is internal plumbing with no usable public role.
	filesMap map[string][]*multipart.FileHeader

	// paramClass is the precomputed binding strategy for one handler parameter,
	// decided once at route-generation time so the request path never re-runs
	// reflection-based type classification.
	paramClass uint8

	// structField is one settable (exported) field of a struct parameter,
	// cached so bindStruct can skip reflection-based field introspection per
	// request.
	// name is the exact bind key: the JSON tag's name when present, else the Go
	// field name with its first letter lowercased, so a lowercase request key
	// binds to an exported PascalCase field without the user adding any tag.
	structField struct {
		index int
		name  string
		typ   reflect.Type
	}

	// bindPlan is a handler's immutable binding blueprint, built in generateRoute
	// from the handler signature. It carries the per-parameter type and class, the
	// business-parameter indexes, cached top-level struct fields, and whether the
	// request body/query must be parsed at all, so serveHTTP can skip work for
	// handlers that bind nothing.
	bindPlan struct {
		types           []reflect.Type
		classes         []paramClass
		structFields    [][]structField // parallel to types; non-nil only for struct business params
		businessIndexes []int
		// writerOK is parallel to types, non-nil only when a parameter is a writer
		// (classWriter): it records whether the pooled writer satisfies that
		// parameter's type, decided once here so the request path injects without
		// re-running the reflection package's interface-satisfaction probe.
		writerOK []bool
		// validatableIndexes is the subset of businessIndexes whose type (or its
		// pointer) implements Validatable, precomputed here so a handler that does
		// not validate pays no per-request reflection or interface boxing.
		validatableIndexes []int
		needsParse         bool
		// needsNames is true when at least one business parameter must be matched
		// against the request by its handler parameter name — any scalar, or more
		// than one business argument (whose JSON members are split by name). A
		// lone struct/pointer-to-struct is self-describing: its fields come from
		// reflection, so it binds correctly even when source names are unavailable.
		needsNames bool
	}
)

// Parameter binding classes; the zero value classBind is the common case.
const (
	// resolved from path/query/body by name
	classBind paramClass = iota
	// *http.Request, injected directly
	classRequest
	// http.ResponseWriter (or a satisfying interface)
	classWriter
	// one of the multipart.FileHeader shapes
	classFiles
	// *Context, injected directly
	classContext
)

// Media-type fragments and error formats used only by this file. The enums
// above stay in their own block so exhaustive-switch tooling keeps working.
const (
	// media-type family carrying JSON payloads for body binding
	applicationPrefix = "application/"
	// structured-suffix marker identifying JSON subtypes
	plusSignJson = "+json"
	// error wrapper naming the parameter being injected
	injectParameterErrFormat = "inject parameter %q: %w"
	// error wrapper naming the key decoded from the JSON body
	jsonParameterErrFormat = "JSON parameter %q: %w"
)

var (
	// two keys both claimed the one body
	multipleRequireError = errors.New("multiple handler parameters require a JSON object")
	// a handler asked to write through an unexpected writer type
	responseMismatch = errors.New("response writer type mismatch")
	// parameter type recognized as the raw request
	requestType = reflect.TypeFor[*http.Request]()
	// parameter type recognized as the injected context
	contextType = reflect.TypeFor[*Context]()
	// interface a handler declares to own the response
	writerType = reflect.TypeFor[http.ResponseWriter]()
	// concrete writer handed to owning handlers, checked for assignability
	pooledWriterType = reflect.TypeFor[*pooledResponseWriter]()
	// uploaded file, by value
	fileHeaderType = reflect.TypeFor[multipart.FileHeader]()
	// uploaded file, by pointer
	fileHeaderPtr = reflect.TypeFor[*multipart.FileHeader]()
	// uploaded files, slice of pointers
	fileHeaderPs = reflect.TypeFor[[]*multipart.FileHeader]()
	// uploaded files, slice of values
	fileHeaderSlice = reflect.TypeFor[[]multipart.FileHeader]()
	// interface checked before a bound value is injected
	validatableType = reflect.TypeFor[Validatable]()
)

// paramsPool recycles the reflection argument slice built per request. A pointer
// to the slice is stored so Get/Put do not box the slice header.
var paramsPool = sync.Pool{
	New: func() any { s := make([]reflect.Value, 0, 8); return &s },
}

// acquireParams returns a zeroed reflection argument slice of length n, reusing a
// pooled backing array when it is large enough.
func acquireParams(n int) (*[]reflect.Value, []reflect.Value) {
	bufPool := paramsPool.Get().(*[]reflect.Value)
	params := *bufPool
	if cap(params) < n {
		params = make([]reflect.Value, n)
	}
	params = params[:n]
	clear(params)
	return bufPool, params
}

// releaseParams zeroes the argument slice (so it pins no request value) and
// returns it to the pool.
func releaseParams(bufPool *[]reflect.Value, params []reflect.Value) {
	clear(params)
	*bufPool = params
	paramsPool.Put(bufPool)
}

// selfDescribing reports whether a business parameter can be bound without its
// handler parameter name. A struct — or any pointer chain leading to one — names
// its values by field, which reflection supplies even when the source file is
// unavailable at runtime; every other kind (scalar, slice, map, interface and
// pointer to those) is matched against the request by the parameter's own name.
func selfDescribing(t reflect.Type) bool {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.Kind() == reflect.Struct
}

// classifyParam decides a parameter's binding class once, at route-generation
// time. It mirrors the old per-request isSpecialParameter checks but runs the
// costlier interface-satisfaction probe (t.Implements) only during startup.
func classifyParam(t reflect.Type) paramClass {
	if t == contextType {
		return classContext
	}
	if t == requestType {
		return classRequest
	}
	if t == writerType || t.Implements(writerType) {
		return classWriter
	}
	switch t {
	case fileHeaderType, fileHeaderPtr, fileHeaderPs, fileHeaderSlice:
		return classFiles
	default:
		return classBind
	}
}

// buildBindPlan turns a handler signature into its immutable binding blueprint.
// It records each parameter's type and class, collects the business-parameter
// indexes, caches the top-level fields of struct parameters, and flags whether
// any parameter forces the request body/query to be parsed.
func buildBindPlan(rt reflect.Type) bindPlan {
	n := rt.NumIn()
	plan := bindPlan{
		types:        make([]reflect.Type, n),
		classes:      make([]paramClass, n),
		structFields: make([][]structField, n),
	}
	visited := make(map[reflect.Type]bool)
	for i := range n {
		t := rt.In(i)
		plan.types[i] = t

		class := classifyParam(t)
		plan.classes[i] = class
		switch class {
		case classBind:
			plan.businessIndexes = append(plan.businessIndexes, i)
			plan.needsParse = true
			if t.Kind() == reflect.Struct {
				plan.structFields[i] = structFieldsOf(t)
				prewarmNestedStructs(t, visited)
			}
			if !selfDescribing(t) {
				plan.needsNames = true
			}
			if t.Implements(validatableType) || reflect.PointerTo(t).Implements(validatableType) {
				plan.validatableIndexes = append(plan.validatableIndexes, i)
			}
		case classFiles:
			plan.needsParse = true
		case classWriter:
			if plan.writerOK == nil {
				plan.writerOK = make([]bool, n)
			}
			plan.writerOK[i] = pooledWriterType.AssignableTo(t)
		default:
		}
	}
	if len(plan.businessIndexes) > 1 {
		plan.needsNames = true
	}
	return plan
}

// prewarmNestedStructs recursively warms the structFieldsCache for every nested
// struct reachable from t, so request-time binding never pays the sync.Map miss.
func prewarmNestedStructs(t reflect.Type, visited map[reflect.Type]bool) {
	if visited[t] {
		return
	}
	visited[t] = true
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return
	}
	fields := structFieldsOf(t)
	for _, f := range fields {
		prewarmNestedStructs(f.typ, visited)
	}
}

// structFieldsCache memorizes field lists per struct type, so nested structs
// (bound via bindStruct at request time) skip reflection introspection just
// like top-level parameters. Concurrent first computes may both run; LoadOrStore
// keeps the first slice so every caller shares one instance. The duplicate
// compute is harmless and rare.
var structFieldsCache sync.Map // reflect.Type -> []structField

// structFieldsOf returns the cached bindable-field list of a struct type.
func structFieldsOf(t reflect.Type) []structField {
	if cached, ok := structFieldsCache.Load(t); ok {
		return cached.([]structField)
	}
	fields := computeStructFields(t)
	actual, _ := structFieldsCache.LoadOrStore(t, fields)
	return actual.([]structField)
}

// lowerFirst lowercases the first rune of s, leaving the rest verbatim. It
// bridges an exported struct field's PascalCase name (Name) to the lowercase
// key clients usually send (name), so no JSON tag is needed. Request keys are
// matched exactly and never transformed.
func lowerFirst(s string) string {
	if s == empty {
		return s
	}
	r, size := utf8.DecodeRuneInString(s)
	if lr := unicode.ToLower(r); lr != r {
		return string(lr) + s[size:]
	}
	return s
}

// computeStructFields lists a struct type's exported fields in declaration
// order. Every exported field of a freshly allocated value is settable, so the
// per-request CanSet/PkgPath checks in bindStruct collapse to this cached slice.
// The bind key is the JSON tag's name when present, else the Go field name with
// its first letter lowercased — so a lowercase request key binds to an exported
// PascalCase field without a tag. `json:"-"` drops the field entirely.
func computeStructFields(t reflect.Type) []structField {
	n := t.NumField()
	fields := make([]structField, 0, n)
	for i := range n {
		field := t.Field(i)
		if field.PkgPath != empty {
			continue // unexported
		}
		name := lowerFirst(field.Name)
		if tag, ok := field.Tag.Lookup("json"); ok {
			if tag == "-" {
				continue
			}
			// `json:"-,"` names the field "-" rather than skipping it.
			if tagName, _, _ := strings.Cut(tag, ","); tagName != "" {
				name = tagName
			}
		}
		fields = append(fields, structField{
			index: i,
			name:  name,
			typ:   field.Type,
		})
	}
	return fields
}

// injectedParameters fills the reflection argument list for a handler call, driven
// by the handler's precomputed bind plan. Each parameter is injected directly
// (request, writer, file headers) or bound by name from path parameters, a JSON
// body, or form values, in that precedence order. Type classification already
// happened in generateRoute, so nothing here re-inspects the signature with
// reflection. params is the caller's scratch slice, already sized to the
// parameter count; the caller recycles it.
func injectedParameters(
	c *Context,
	form url.Values,
	files filesMap,
	body []byte,
	jsonMedia bool,
	params []reflect.Value,
) error {
	rh := c.rh
	plan := &rh.plan
	names := rh.paramNames

	c.src = nameSources{path: c.params, form: form, files: files}
	src := &c.src

	var jsonFields map[string]json.RawMessage
	isJSON := jsonMedia && hasNonWhitespace(body)
	if isJSON && len(plan.businessIndexes) > 1 {
		if err := json.Unmarshal(body, &jsonFields); err != nil {
			return fmt.Errorf("invalid JSON body: %w", err)
		}
		if jsonFields == nil {
			return multipleRequireError
		}
	}

	for i := range plan.types {
		t := plan.types[i]
		name := paramNameAt(names, i)

		switch plan.classes[i] {
		case classBind:
			// The common case: a business parameter resolved from path/query/
			// body/form by the code below this switch, so nothing to inject
			// here. Breaking out (no continue) reaches that resolution path.
		case classContext:
			params[i] = reflect.ValueOf(c)
			continue
		case classRequest:
			params[i] = reflect.ValueOf(c.Request)
			continue
		case classWriter:
			if !plan.writerOK[i] {
				return fmt.Errorf(injectParameterErrFormat, name, responseMismatch)
			}
			params[i] = reflect.ValueOf(c.writer)
			continue
		case classFiles:
			headers, _ := src.filesGet(name)
			v, err := bindFileHeaders(t, headers)
			if err != nil {
				return fmt.Errorf(injectParameterErrFormat, name, err)
			}
			params[i] = v
			continue
		}

		// classBind: a business parameter, resolved from request data. Path
		// parameters take priority over query/body for scalar arguments too.
		if pathValue, ok := src.pathGet(name); ok {
			v, err := bindTextValue(t, []string{pathValue})
			if err != nil {
				return fmt.Errorf("path parameter %q: %w", name, err)
			}
			params[i] = v
			continue
		}

		if isJSON {
			// A lone business parameter decodes the whole body; several split
			// the JSON object by parameter name.
			raw := body
			if len(plan.businessIndexes) > 1 {
				var ok bool
				raw, ok = jsonFields[name]
				if !ok {
					return fmt.Errorf("JSON field %q not found", name)
				}
			}
			v, err := decodeJSON(t, raw)
			if err != nil {
				return fmt.Errorf(jsonParameterErrFormat, name, err)
			}
			params[i] = v
			continue
		}

		var (
			v   reflect.Value
			err error
		)
		if fields := plan.structFields[i]; fields != nil {
			v, err = bindStructFields(t, fields, src)
		} else {
			v, err = bindFormParameter(t, name, src)
		}
		if err != nil {
			return fmt.Errorf("parameter %q: %w", name, err)
		}
		params[i] = v
	}

	for i, v := range params {
		if !v.IsValid() {
			return fmt.Errorf("parameter %q was not initialized", paramNameAt(names, i))
		}
		if !v.Type().AssignableTo(plan.types[i]) {
			return fmt.Errorf("parameter %q type mismatch: got %s, want %s",
				paramNameAt(names, i), v.Type(), plan.types[i])
		}
	}
	return nil
}

// paramNameAt is a bounds-safe lookup of a handler parameter name.
//
// generateRoute pads the recovered names to exactly one per parameter, so this
// normally just reads the slice. The fallback keeps a name list of any length
// from turning into an index-out-of-range panic, which the recover path would
// otherwise hand back to the client as a 200 response.
func paramNameAt(names []string, index int) string {
	if index < len(names) {
		return names[index]
	}
	return unnamedParamName(index)
}

// hasNonWhitespace reports whether data holds at least one byte beyond
// spaces, tabs and newlines. The scan is allocation-free; TrimSpace would
// copy the whole (up to multi-MiB) body just to test emptiness.
func hasNonWhitespace(data []byte) bool {
	return bytes.ContainsFunc(data, func(r rune) bool {
		return r != ' ' && r != '\t' && r != '\n' && r != '\r'
	})
}

// isJSONMediaType reports whether a parsed media type is JSON — the exact
// application/json or a structured suffix such as application/vnd.api+json.
func isJSONMediaType(mediaType string) bool {
	return mediaType == applicationJson ||
		(strings.HasPrefix(mediaType, applicationPrefix) && strings.HasSuffix(mediaType, plusSignJson))
}

// nameSources bundles the three by-name request sources a binder reads. It
// lives inside the pooled Context, so priming it costs no allocation. Lookups
// are exact: the request key must equal the bind name byte-for-byte. Struct
// field names are pre-lowercased at registration so clients can send lowercase
// keys without tags; the request data itself is never transformed.
type nameSources struct {
	path  map[string]string
	form  url.Values
	files filesMap
}

func (s *nameSources) pathGet(name string) (string, bool) {
	v, ok := s.path[name]
	return v, ok
}

func (s *nameSources) formGet(name string) ([]string, bool) {
	v, ok := s.form[name]
	return v, ok
}

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
// when their source is absent, structs recurse over their fields, file header
// types pull from the uploaded files, slices/arrays consume repeated values,
// and scalars take the first value. name is matched against the request exactly.
func bindFormParameter(t reflect.Type, name string, src *nameSources) (reflect.Value, error) {
	if t.Kind() == reflect.Pointer {
		// A missing non-struct pointer stays nil; a missing *struct is still
		// allocated (zero-valued) so its fields can be bound individually.
		if _, ok := src.pathGet(name); !ok {
			if _, ok = src.formGet(name); !ok {
				if _, ok = src.filesGet(name); !ok && t.Elem().Kind() != reflect.Struct {
					return reflect.Zero(t), nil
				}
			}
		}
		value, err := bindFormParameter(t.Elem(), name, src)
		if err != nil {
			return reflect.Value{}, err
		}
		return wrapPointer(t, value), nil
	}

	if t.Kind() == reflect.Struct {
		return bindStruct(t, src)
	}

	if isFileHeaderType(t) {
		headers, _ := src.filesGet(name)
		return bindFileHeaders(t, headers)
	}

	values, _ := src.formGet(name)
	switch t.Kind() {
	case reflect.Array, reflect.Slice:
		// []byte in a form field is treated as a string's raw bytes.
		if t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8 {
			if len(values) == 0 {
				return reflect.Zero(t), nil
			}
			return reflect.ValueOf([]byte(values[0])).Convert(t), nil
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
			result = reflect.MakeSlice(t, len(values), len(values))
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

// bindStruct fills an exported-field struct from form and path values, matching
// each field's exact bind name (JSON tag, or the field name with its first
// letter lowercased). The field list is cached per type, so nested structs cost
// no per-request reflection introspection either.
func bindStruct(t reflect.Type, src *nameSources) (reflect.Value, error) {
	return bindStructFields(t, structFieldsOf(t), src)
}

// bindStructFields populates a fresh struct of type t from a precomputed field
// list, so the request path skips reflection-based field introspection entirely.
// The per-field precedence matches bindStruct: a path value wins over
// form/query, and everything else falls through to bindFormParameter.
func bindStructFields(t reflect.Type, fields []structField,
	src *nameSources) (reflect.Value, error) {
	result := reflect.New(t).Elem()

	for _, field := range fields {
		dst := result.Field(field.index)

		// Bind names come from the JSON tag when present, else the Go field
		// name with its first letter lowercased; matching is exact. Path wins
		// over query/form values.
		if pathValue, ok := src.pathGet(field.name); ok {
			value, err := bindTextValue(field.typ, []string{pathValue})
			if err != nil {
				return reflect.Value{}, fmt.Errorf("path field %q: %w", field.name, err)
			}
			dst.Set(value)
			continue
		}

		value, err := bindFormParameter(field.typ, field.name, src)
		if err != nil {
			return reflect.Value{}, fmt.Errorf("field %q: %w", field.name, err)
		}
		dst.Set(value)
	}
	return result, nil
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
		return reflect.ValueOf(headers), nil
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

// wrapPointer boxes a bound element value into a fresh pointer of type t,
// the shared tail of the pointer branches in the form and text binders.
func wrapPointer(t reflect.Type, elem reflect.Value) reflect.Value {
	result := reflect.New(t.Elem())
	result.Elem().Set(elem)
	return result
}

// bindTextValue converts raw request strings into a scalar (or pointer to
// scalar) of type t. Only the first value is used; repeated values should be
// bound to a slice or array parameter instead.
func bindTextValue(t reflect.Type, values []string) (reflect.Value, error) {
	if t.Kind() == reflect.Pointer {
		if len(values) == 0 {
			return reflect.Zero(t), nil
		}
		value, err := bindTextValue(t.Elem(), values)
		if err != nil {
			return reflect.Value{}, err
		}
		return wrapPointer(t, value), nil
	}

	if len(values) == 0 {
		return reflect.Zero(t), nil
	}
	result := reflect.New(t).Elem()

	// Scalar parameters use the first value. Repeated values should use slices/arrays.
	value := values[0]
	switch t.Kind() {
	case reflect.String:
		result.SetString(value)
	case reflect.Bool:
		v, err := strconv.ParseBool(value)
		if err != nil {
			return reflect.Value{}, err
		}
		result.SetBool(v)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v, err := strconv.ParseInt(value, 10, t.Bits())
		if err != nil {
			return reflect.Value{}, err
		}
		result.SetInt(v)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		v, err := strconv.ParseUint(value, 10, t.Bits())
		if err != nil {
			return reflect.Value{}, err
		}
		result.SetUint(v)
	case reflect.Float32, reflect.Float64:
		v, err := strconv.ParseFloat(value, t.Bits())
		if err != nil {
			return reflect.Value{}, err
		}
		result.SetFloat(v)
	case reflect.Complex64, reflect.Complex128:
		v, err := strconv.ParseComplex(value, t.Bits())
		if err != nil {
			return reflect.Value{}, err
		}
		result.SetComplex(v)
	default:
		return reflect.Value{}, fmt.Errorf("unsupported parameter type: %s", t)
	}
	return result, nil
}
