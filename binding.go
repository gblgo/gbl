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
	// cached so bindStruct can skip reflect.Type field introspection per request.
	// name is the bind name: the JSON tag's name when present, else the Go field
	// name; lower is its precomputed lowercase form for fold lookups.
	structField struct {
		index int
		name  string
		lower string
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
	classBind    paramClass = iota // resolved from path/query/body by name
	classRequest                   // *http.Request, injected directly
	classWriter                    // http.ResponseWriter (or a satisfying interface)
	classFiles                     // one of the multipart.FileHeader shapes
	classContext                   // *Context, injected directly
)

var (
	multipleRequireError = errors.New("multiple handler parameters require a JSON object")
	responseMismatch     = errors.New("response writer type mismatch")
	requestType          = reflect.TypeFor[*http.Request]()
	contextType          = reflect.TypeFor[*Context]()
	writerType           = reflect.TypeFor[http.ResponseWriter]()
	fileHeaderType       = reflect.TypeFor[multipart.FileHeader]()
	fileHeaderPtr        = reflect.TypeFor[*multipart.FileHeader]()
	fileHeaderPs         = reflect.TypeFor[[]*multipart.FileHeader]()
	fileHeaderSlice      = reflect.TypeFor[[]multipart.FileHeader]()
	validatableType      = reflect.TypeFor[Validatable]()
)

var paramsPool = sync.Pool{
	New: func() any { return new(make([]reflect.Value, 0, 8)) },
}

// acquireParams returns a zeroed reflect argument slice of length n, reusing a
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
			}
			if !selfDescribing(t) {
				plan.needsNames = true
			}
			// Decide validation opt-in once, by type, so the request path never
			// boxes a value just to discover it does not implement Validatable.
			if t.Implements(validatableType) || reflect.PointerTo(t).Implements(validatableType) {
				plan.validatableIndexes = append(plan.validatableIndexes, i)
			}
		case classFiles:
			// File headers arrive only through a parsed multipart body.
			plan.needsParse = true
		default:
			// classRequest, classWriter and classContext are injected directly and
			// never force the body/query to be parsed.
		}
	}
	// More than one business argument is name-dependent even when every one is a
	// struct: a JSON object body is distributed across them by parameter name.
	if len(plan.businessIndexes) > 1 {
		plan.needsNames = true
	}
	return plan
}

// structFieldsCache memorizes field lists per struct type, so nested structs
// (bound via bindStruct at request time) skip reflection introspection just
// like top-level parameters. Concurrent first computes store identical slices;
// the duplicate work is harmless and rare.
var structFieldsCache sync.Map // reflect.Type -> []structField

// structFieldsOf returns the cached bindable-field list of a struct type.
func structFieldsOf(t reflect.Type) []structField {
	if cached, ok := structFieldsCache.Load(t); ok {
		return cached.([]structField)
	}
	fields := computeStructFields(t)
	structFieldsCache.Store(t, fields)
	return fields
}

// computeStructFields lists a struct type's exported fields in declaration
// order. Every exported field of a freshly allocated value is settable, so the
// per-request CanSet/PkgPath checks in bindStruct collapse to this cached slice.
// A JSON tag's name replaces the Go field name (matching encoding/json, so form
// and JSON binding agree on names); `json:"-"` drops the field entirely.
func computeStructFields(t reflect.Type) []structField {
	n := t.NumField()
	fields := make([]structField, 0, n)
	for i := range n {
		field := t.Field(i)
		if field.PkgPath != null {
			continue // unexported
		}
		name := field.Name
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
			lower: strings.ToLower(name),
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
	mediaType string,
	params []reflect.Value,
) error {
	rh := c.rh
	plan := &rh.plan
	names := rh.paramNames

	c.src = nameSources{path: c.params, form: form, files: files}
	src := &c.src

	var jsonFields map[string]json.RawMessage
	var jf foldLookup[json.RawMessage]
	isJSON := isJSONMediaType(mediaType) && hasNonWhitespace(body)
	if isJSON && len(plan.businessIndexes) > 1 {
		if err := json.Unmarshal(body, &jsonFields); err != nil {
			return fmt.Errorf("invalid JSON body: %w", err)
		}
		if jsonFields == nil {
			return multipleRequireError
		}
		jf.exact = jsonFields
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
			v, err := injectSpecialParameter(t, c.writer, c.Request, src, name)
			if err != nil {
				return fmt.Errorf(injectParameterErrFormat, name, err)
			}
			params[i] = v
			continue
		case classFiles:
			headers, _ := src.filesGet(name, null)
			v, err := bindFileHeaders(t, headers)
			if err != nil {
				return fmt.Errorf(injectParameterErrFormat, name, err)
			}
			params[i] = v
			continue
		}

		// classBind: a business parameter, resolved from request data. Path
		// parameters take priority over query/body for scalar arguments too.
		if pathValue, ok := src.pathGet(name, null); ok {
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
				raw, ok = jf.get(name)
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
			v, err = bindFormParameter(t, name, null, src)
		}
		if err != nil {
			return fmt.Errorf("parameter %q: %w", name, err)
		}
		params[i] = v
	}

	for i, v := range params {
		name := paramNameAt(names, i)
		if !v.IsValid() {
			return fmt.Errorf("parameter %q was not initialized", name)
		}
		if !v.Type().AssignableTo(plan.types[i]) {
			return fmt.Errorf("parameter %q type mismatch: got %s, want %s", name, v.Type(), plan.types[i])
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
		(strings.HasPrefix(mediaType, applicationNoJson) && strings.HasSuffix(mediaType, plusSignJson))
}

// foldThreshold is the source size where a folded index starts to beat
// repeated EqualFold scans. Below it a miss scans linearly — building a map
// for a handful of keys costs more than the scans it saves. At or above it the
// first miss builds a lowercase index that every later lookup reuses, turning
// O(lookups × keys) into O(lookups + keys).
const foldThreshold = 16

// foldIndex builds the lowercase-keyed copy of a source map. First key wins on
// a fold collision, mirroring the arbitrary pick of a linear scan.
func foldIndex[V any](m map[string]V) map[string]V {
	out := make(map[string]V, len(m))
	for key, v := range m {
		lk := strings.ToLower(key)
		if _, seen := out[lk]; !seen {
			out[lk] = v
		}
	}
	return out
}

// foldName returns the precomputed lowercase name when one is cached (struct
// fields), else lowers on demand — reached only on the indexed miss path.
func foldName(name, lower string) string {
	if lower != null {
		return lower
	}
	return strings.ToLower(name)
}

// nameSources bundles the three by-name request sources a binder reads. It
// lives inside the pooled Context, so priming it costs no allocation. Each
// source resolves exact-first through sourceGet; the per-source methods stay
// as thin typed wrappers so call sites keep their concrete value types.
type nameSources struct {
	path  map[string]string
	form  url.Values
	files filesMap

	pathFold  map[string]string
	formFold  map[string][]string
	filesFold map[string][]*multipart.FileHeader
}

// sourceGet is the shared exact-then-fold lookup behind the three sources.
// Dictionary indirection from the generic form only reaches the cold paths
// (EqualFold scan, index build); the exact map hit runs direct.
func sourceGet[V any](m map[string]V, fold *map[string]V, name, lower string) (V, bool) {
	if v, ok := m[name]; ok {
		return v, true
	}
	if len(m) < foldThreshold {
		for key, v := range m {
			if strings.EqualFold(key, name) {
				return v, true
			}
		}
		var zero V
		return zero, false
	}
	if *fold == nil {
		*fold = foldIndex(m)
	}
	v, ok := (*fold)[foldName(name, lower)]
	return v, ok
}

func (s *nameSources) pathGet(name, lower string) (string, bool) {
	return sourceGet(s.path, &s.pathFold, name, lower)
}

func (s *nameSources) formGet(name, lower string) ([]string, bool) {
	return sourceGet(s.form, &s.formFold, name, lower)
}

func (s *nameSources) filesGet(name, lower string) ([]*multipart.FileHeader, bool) {
	return sourceGet(s.files, &s.filesFold, name, lower)
}

// foldLookup serves the multi-parameter JSON split, a colder path where the
// generic form is fine: exact hit first, lazy folded index on miss.
type foldLookup[V any] struct {
	exact map[string]V
	fold  map[string]V
}

func (f *foldLookup[V]) get(name string) (V, bool) {
	if v, ok := f.exact[name]; ok {
		return v, true
	}
	if f.fold == nil {
		f.fold = foldIndex(f.exact)
	}
	v, ok := f.fold[strings.ToLower(name)]
	return v, ok
}

// lookupString finds a map value by exact key first, then case-insensitively.
// It serves one-off lookups outside the binding path (Context.Param); binders
// use foldLookup so the scan amortizes across a whole request.
func lookupString(values map[string]string, name string) (string, bool) {
	if value, ok := values[name]; ok {
		return value, true
	}
	for key, value := range values {
		if strings.EqualFold(key, name) {
			return value, true
		}
	}
	return null, false
}

// bindFormParameter binds one non-JSON parameter of any kind: pointers stay nil
// when their source is absent, structs recurse over their fields, file header
// types pull from the uploaded files, slices/arrays consume repeated values,
// and scalars take the first value. lower is name's precomputed lowercase form,
// or empty to fold lazily only when an exact lookup misses.
func bindFormParameter(t reflect.Type, name, lower string, src *nameSources) (reflect.Value, error) {
	if t.Kind() == reflect.Pointer {
		// A missing non-struct pointer stays nil; a missing *struct is still
		// allocated (zero-valued) so its fields can be bound individually.
		if _, ok := src.pathGet(name, lower); !ok {
			if _, ok = src.formGet(name, lower); !ok {
				if _, ok = src.filesGet(name, lower); !ok && t.Elem().Kind() != reflect.Struct {
					return reflect.Zero(t), nil
				}
			}
		}
		value, err := bindFormParameter(t.Elem(), name, lower, src)
		if err != nil {
			return reflect.Value{}, err
		}
		return wrapPointer(t, value), nil
	}

	if t.Kind() == reflect.Struct {
		return bindStruct(t, src)
	}

	if isFileHeaderType(t) {
		headers, _ := src.filesGet(name, lower)
		return bindFileHeaders(t, headers)
	}

	values, _ := src.formGet(name, lower)
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
// bind names (JSON tag or Go field name) case-insensitively. The field list is
// cached per type, so nested structs cost no per-request reflection
// introspection either.
func bindStruct(t reflect.Type, src *nameSources) (reflect.Value, error) {
	return bindStructFields(t, structFieldsOf(t), src)
}

// bindStructFields populates a fresh struct of type t from a precomputed field
// list, so the request path skips reflect.Type field introspection entirely. The
// per-field precedence matches bindStruct: a path value wins over form/query, and
// everything else falls through to bindFormParameter.
func bindStructFields(t reflect.Type, fields []structField, src *nameSources) (reflect.Value, error) {
	result := reflect.New(t).Elem()

	for _, field := range fields {
		dst := result.Field(field.index)

		// Bind names come from the JSON tag when present, else the Go field
		// name; matching is case-insensitive. Path wins over query/form values.
		if pathValue, ok := src.pathGet(field.name, field.lower); ok {
			value, err := bindTextValue(field.typ, []string{pathValue})
			if err != nil {
				return reflect.Value{}, fmt.Errorf("path field %q: %w", field.name, err)
			}
			dst.Set(value)
			continue
		}

		value, err := bindFormParameter(field.typ, field.name, field.lower, src)
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

// injectSpecialParameter supplies the framework-owned arguments a handler can
// declare: *http.Request, any http.ResponseWriter implementation, and the file
// header types, which are matched by parameter name against the uploads.
func injectSpecialParameter(
	t reflect.Type,
	w http.ResponseWriter,
	r *http.Request,
	src *nameSources,
	name string,
) (reflect.Value, error) {
	if t == requestType {
		return reflect.ValueOf(r), nil
	}

	if t == writerType || t.Implements(writerType) {
		value := reflect.ValueOf(w)
		if value.Type().AssignableTo(t) {
			return value, nil
		}
		if t.Kind() == reflect.Interface && value.Type().Implements(t) {
			return value, nil
		}
		return reflect.Value{}, responseMismatch
	}

	headers, _ := src.filesGet(name, null)
	return bindFileHeaders(t, headers)
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
