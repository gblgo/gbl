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
	"slices"
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

	// requestKeySet is the immutable lookup form of a handler's bind names. It is
	// built during route generation so request parsing never scans a key slice.
	requestKeySet map[string]struct{}

	// structField is one settable (exported) field of a struct parameter,
	// cached so bindStruct can skip reflection-based field introspection per
	// request. name is the exact bind key: the non-empty JSON tag name when
	// present, else the Go field name with its first letter lowercased. Thus a
	// lowercase request key binds to an exported PascalCase field without a tag.
	structField struct {
		// index is the field position in the reflected struct
		index int
		// name is the exact request key for this field
		name string
		// typ is the reflected field type
		typ reflect.Type
	}

	// bindPlan is an immutable handler binding blueprint, built in generateRoute
	// from the handler signature. It carries the per-parameter type and class, the
	// business-parameter indexes, cached top-level struct fields, and whether the
	// request body/query must be parsed at all, so serveHTTP can skip work for
	// handlers that bind nothing.
	bindPlan struct {
		// types is parallel to the handler parameter list
		types []reflect.Type
		// classes is parallel to types and records each injection strategy
		classes []paramClass
		// structFields is parallel to types and caches top-level struct fields
		structFields [][]structField
		// businessIndexes identifies parameters bound from request data
		businessIndexes []int
		// writerOK is parallel to types, non-nil only when a parameter is a writer
		// (classWriter): it records whether the pooled writer satisfies that
		// parameter type, decided once here so the request path injects without
		// re-running the reflection package interface-satisfaction probe.
		writerOK []bool
		// validatableIndexes is the subset of businessIndexes whose type (or its
		// pointer) implements Validatable, precomputed here so a handler that does
		// not validate pays no per-request reflection or interface boxing.
		validatableIndexes []int
		// queryKeys is the set of exact query names consumed by this handler.
		queryKeys requestKeySet
		// jsonKeys lists the top-level members needed by multi-parameter JSON handlers.
		jsonKeys []string
		// jsonKeySet provides constant-time lookup for jsonKeys.
		jsonKeySet requestKeySet
		// needsParse records whether request data must be parsed
		needsParse bool
		// hasSSE is true when any parameter declares gbl.SSEWriter.
		hasSSE bool
		// needsNames is true when at least one business or file parameter must be
		// matched against the request by its handler parameter name. A lone struct
		// or pointer-to-struct is self-describing: its fields come from reflection,
		// so it binds correctly even when source names are unavailable.
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
	// gbl.SSEWriter, injected as the request event stream
	classSSE
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
	// several handler parameters need one JSON object to share the body
	multipleRequireError = errors.New("multiple handler parameters require a JSON object")
	// a handler asked to write through an unexpected writer type
	responseMismatch = errors.New("response writer type mismatch")
	// parameter type recognized as the raw request
	requestType = reflect.TypeFor[*http.Request]()
	// type whose custom JSON decoder owns its exact field-matching semantics
	jsonUnmarshalerType = reflect.TypeFor[json.Unmarshaler]()
	// parameter type recognized as the injected context
	contextType = reflect.TypeFor[*Context]()
	// interface a handler declares to own the response
	writerType = reflect.TypeFor[http.ResponseWriter]()
	// interface a handler declares to stream server-sent events
	sseWriterType = reflect.TypeFor[SSEWriter]()
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

// maxTypePointerDepth bounds walks through recursive named pointer types.
const maxTypePointerDepth = 64

// derefType strips pointer chains and rejects chains deeper than the binding
// depth limit. Struct recursion is tracked separately by isRecursiveStruct.
func derefType(t reflect.Type) (reflect.Type, bool) {
	for depth := 0; t.Kind() == reflect.Pointer; depth++ {
		if depth >= maxTypePointerDepth {
			return nil, false
		}
		t = t.Elem()
	}
	return t, true
}

// selfDescribing reports whether a business parameter can be bound without its
// handler parameter name. A struct — or any pointer chain leading to one — names
// its values by field, which reflection supplies even when the source file is
// unavailable at runtime; every other kind (scalar, slice, map, interface and
// pointer to those) is matched against the request by the parameter name.
func selfDescribing(t reflect.Type) bool {
	terminal, ok := derefType(t)
	return ok && terminal.Kind() == reflect.Struct
}

// classifyParam decides a parameter binding class once, at route-generation
// time. It mirrors the old per-request isSpecialParameter checks but runs the
// costlier interface-satisfaction probe (t.Implements) only during startup.
func classifyParam(t reflect.Type) paramClass {
	if t == contextType {
		return classContext
	}
	if t == requestType {
		return classRequest
	}
	if t == sseWriterType {
		return classSSE
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
// It records each parameter type and class, collects the business-parameter
// indexes, caches the top-level fields of struct parameters, and flags whether
// any parameter forces the request body/query to be parsed.
func buildBindPlan(rt reflect.Type) (bindPlan, error) {
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
			if _, ok := derefType(t); !ok {
				return plan, fmt.Errorf("recursive pointer type: %s", t)
			}
			plan.businessIndexes = append(plan.businessIndexes, i)
			plan.needsParse = true
			if t.Kind() == reflect.Struct {
				plan.structFields[i] = structFieldsOf(t)
			}
			if selfDescribing(t) {
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
			plan.needsNames = true
		case classWriter:
			if plan.writerOK == nil {
				plan.writerOK = make([]bool, n)
			}
			plan.writerOK[i] = pooledWriterType.AssignableTo(t)
		case classSSE:
			plan.hasSSE = true
		default:
		}
	}
	if len(plan.businessIndexes) > 1 {
		plan.needsNames = true
	}
	return plan, nil
}

// prepareKeys records the exact query and JSON names this handler can consume.
// It runs after parameter-name recovery and never changes during serving.
func (p *bindPlan) prepareKeys(names []string) {
	if len(p.businessIndexes) > 0 && p.queryKeys == nil {
		p.queryKeys = make(requestKeySet)
	}
	clear(p.queryKeys)
	clear(p.jsonKeySet)
	p.jsonKeys = nil

	visited := make(map[reflect.Type]bool)
	for _, index := range p.businessIndexes {
		name := paramNameAt(names, index)
		if selfDescribing(p.types[index]) {
			appendStructQueryKeys(p.queryKeys, p.types[index], visited)
		} else {
			p.queryKeys[name] = struct{}{}
		}
		if len(p.businessIndexes) > 1 {
			if _, exists := p.jsonKeySet[name]; !exists {
				p.jsonKeys = append(p.jsonKeys, name)
				if p.jsonKeySet == nil {
					p.jsonKeySet = make(requestKeySet)
				}
				p.jsonKeySet[name] = struct{}{}
			}
		}
	}
}

// appendStructQueryKeys collects leaf field names because nested structs bind
// their own fields rather than a value for the outer struct field.
func appendStructQueryKeys(keys requestKeySet, t reflect.Type, visited map[reflect.Type]bool) {
	t, ok := derefType(t)
	if !ok || t.Kind() != reflect.Struct || visited[t] {
		return
	}
	visited[t] = true
	for _, field := range structFieldsOf(t) {
		fieldType, ok := derefType(field.typ)
		if !ok {
			continue
		}
		if fieldType.Kind() == reflect.Struct {
			appendStructQueryKeys(keys, fieldType, visited)
		} else {
			keys[field.name] = struct{}{}
		}
	}
}

// newRequestKeySet adapts a plain key slice for direct parser callers. Route
// handlers keep the set built by prepareKeys and do not take this path.
func newRequestKeySet(keys []string) requestKeySet {
	if len(keys) == 0 {
		return nil
	}
	set := make(requestKeySet, len(keys))
	for _, key := range keys {
		set[key] = struct{}{}
	}
	return set
}

// prewarmNestedStructs recursively warms the structFieldsCache for every nested
// struct reachable from t, including structs inside JSON slices and maps, so
// request-time binding never pays a concurrent-map miss.
func prewarmNestedStructs(t reflect.Type, visited map[reflect.Type]bool) {
	t, ok := derefType(t)
	if !ok || visited[t] {
		return
	}
	visited[t] = true
	switch t.Kind() {
	case reflect.Struct:
		_ = isRecursiveStruct(t)
		fields := structFieldsOf(t)
		_ = jsonStructFieldsOf(t)
		for _, f := range fields {
			prewarmNestedStructs(f.typ, visited)
		}
	case reflect.Array, reflect.Slice, reflect.Map:
		prewarmNestedStructs(t.Elem(), visited)
	}
}

// recursiveStructCache records whether a struct type occurs in its own nested
// struct/pointer field graph. The answer is a property of the type, not of a
// particular request, and is therefore cached across requests.
var recursiveStructCache sync.Map

// isRecursiveStruct reports whether t can reach itself through nested struct
// fields and pointer chains. A struct that merely contains a different recursive
// type is not itself recursive and keeps the ordinary allocation rule.
func isRecursiveStruct(t reflect.Type) bool {
	t, ok := derefType(t)
	if !ok || t.Kind() != reflect.Struct {
		return false
	}
	if cached, ok := recursiveStructCache.Load(t); ok {
		return cached.(bool)
	}
	visited := make(map[reflect.Type]bool)
	var reachesSelf func(reflect.Type) bool
	reachesSelf = func(current reflect.Type) bool {
		for _, field := range structFieldsOf(current) {
			fieldType, ok := derefType(field.typ)
			if !ok || fieldType.Kind() != reflect.Struct {
				continue
			}
			if fieldType == t {
				return true
			}
			if !visited[fieldType] {
				visited[fieldType] = true
				if reachesSelf(fieldType) {
					return true
				}
			}
		}
		return false
	}
	recursive := reachesSelf(t)
	recursiveStructCache.Store(t, recursive)
	return recursive
}

// structFieldsCache stores field lists per struct type so nested structs bound
// through bindStruct skip request-time reflection introspection. Concurrent first
// computations may both run; LoadOrStore keeps the first slice. The duplicate
// computation is harmless and rare.
var structFieldsCache sync.Map

// structFieldsOf returns the cached bindable-field list of a struct type.
func structFieldsOf(t reflect.Type) []structField {
	if cached, ok := structFieldsCache.Load(t); ok {
		return cached.([]structField)
	}
	fields := computeStructFields(t)
	actual, _ := structFieldsCache.LoadOrStore(t, fields)
	return actual.([]structField)
}

// jsonStructFieldsCache stores exact JSON member names and their field types.
// encoding/json otherwise matches struct fields case-insensitively, so the
// single-struct binder filters objects against this map before unmarshalling.
// The lookup follows JSON promotion and dominance rules rather than the flat
// form-binding field list.
var jsonStructFieldsCache sync.Map

// jsonStructFieldsOf returns the cached exact-member lookup for t, including
// promoted fields and excluding names hidden by equal-depth conflicts.
func jsonStructFieldsOf(t reflect.Type) map[string]reflect.Type {
	if cached, ok := jsonStructFieldsCache.Load(t); ok {
		return cached.(map[string]reflect.Type)
	}
	lookup := computeJSONStructFields(t)
	actual, _ := jsonStructFieldsCache.LoadOrStore(t, lookup)
	return actual.(map[string]reflect.Type)
}

// jsonFieldCandidate is one possible exact member before dominance resolution.
type jsonFieldCandidate struct {
	// name is the exact request key after applying the binding name rule.
	name string
	// index identifies the promoted field path.
	index []int
	// typ is the value type used for recursive filtering.
	typ reflect.Type
	// tagged records whether name came from an explicit JSON tag.
	tagged bool
}

// computeJSONStructFields mirrors encoding/json's breadth-first field
// promotion, then applies exact bind names. Shallower members dominate deeper
// ones; equal-depth members conflict unless one name came from a JSON tag.
func computeJSONStructFields(t reflect.Type) map[string]reflect.Type {
	// jsonEmbeddedType is one anonymous struct queued for the next breadth level.
	type jsonEmbeddedType struct {
		// typ is the embedded struct type to scan.
		typ reflect.Type
		// index is the field path from the root to typ.
		index []int
	}

	current := []jsonEmbeddedType{}
	next := []jsonEmbeddedType{{typ: t}}
	var count, nextCount map[reflect.Type]int
	visited := make(map[reflect.Type]bool)
	var candidates []jsonFieldCandidate

	for len(next) > 0 {
		current, next = next, current[:0]
		count, nextCount = nextCount, make(map[reflect.Type]int)
		for _, embedded := range current {
			if visited[embedded.typ] {
				continue
			}
			visited[embedded.typ] = true
			for i := range embedded.typ.NumField() {
				field := embedded.typ.Field(i)
				fieldType := field.Type
				if field.Anonymous && fieldType.Kind() == reflect.Pointer {
					fieldType = fieldType.Elem()
				}
				if field.Anonymous {
					// Unexported embedded structs may still promote exported fields.
					if !field.IsExported() && fieldType.Kind() != reflect.Struct {
						continue
					}
				} else if !field.IsExported() {
					continue
				}

				tag := field.Tag.Get("json")
				if tag == "-" {
					continue
				}
				name, _, _ := strings.Cut(tag, ",")
				if !validJSONFieldName(name) {
					name = ""
				}
				index := make([]int, len(embedded.index)+1)
				copy(index, embedded.index)
				index[len(embedded.index)] = i

				if name == "" && field.Anonymous && fieldType.Kind() == reflect.Struct {
					nextCount[fieldType]++
					if nextCount[fieldType] == 1 {
						next = append(next, jsonEmbeddedType{typ: fieldType, index: index})
					}
					continue
				}
				tagged := name != ""
				if name == "" {
					name = lowerFirst(field.Name)
				}
				candidate := jsonFieldCandidate{
					name:   name,
					index:  index,
					typ:    field.Type,
					tagged: tagged,
				}
				candidates = append(candidates, candidate)
				if count[embedded.typ] > 1 {
					// Repeated embedding paths make this name ambiguous even when
					// the shared embedded type itself was queued only once.
					candidates = append(candidates, candidate)
				}
			}
		}
	}

	slices.SortFunc(candidates, compareJSONFieldCandidates)

	lookup := make(map[string]reflect.Type, len(candidates))
	for start := 0; start < len(candidates); {
		end := start + 1
		for end < len(candidates) && candidates[end].name == candidates[start].name {
			end++
		}
		first := candidates[start]
		if end-start == 1 || len(first.index) != len(candidates[start+1].index) ||
			first.tagged != candidates[start+1].tagged {
			lookup[first.name] = first.typ
		}
		start = end
	}
	return lookup
}

// compareJSONFieldCandidates orders members by name, depth, tag priority and
// field index so the first candidate in each name group wins dominance resolution.
func compareJSONFieldCandidates(a, b jsonFieldCandidate) int {
	if order := strings.Compare(a.name, b.name); order != 0 {
		return order
	}
	if len(a.index) != len(b.index) {
		if len(a.index) < len(b.index) {
			return -1
		}
		return 1
	}
	if a.tagged != b.tagged {
		if a.tagged {
			return -1
		}
		return 1
	}
	return slices.Compare(a.index, b.index)
}

// validJSONFieldName reports whether encoding/json accepts name as an explicit
// tag. Empty and invalid names fall back to the normal Go field name.
func validJSONFieldName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			continue
		}
		if !strings.ContainsRune("!#$%&()*+-./:;<=>?@[]^_{|}~ ", r) {
			return false
		}
	}
	return true
}

// lowerFirst lowercases the first rune of s, leaving the rest verbatim. It
// bridges an exported struct field PascalCase name (Name) to the lowercase
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

// computeStructFields lists a struct type exported fields in declaration
// order. Every exported field of a freshly allocated value is settable, so the
// per-request CanSet/PkgPath checks in bindStruct collapse to this cached slice.
// The bind key is the JSON tag name when present, else the Go field name with
// its first letter lowercased — so a lowercase request key binds to an exported
// PascalCase field without a tag. `json:"-"` drops the field entirely.
func computeStructFields(t reflect.Type) []structField {
	n := t.NumField()
	fields := make([]structField, 0, n)
	for i := range n {
		field := t.Field(i)
		if field.PkgPath != empty {
			continue
		}
		name := lowerFirst(field.Name)
		if tag, ok := field.Tag.Lookup("json"); ok {
			if tag == "-" {
				continue
			}
			// A trailing comma makes the dash a literal field name.
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

// decodeJSONFields validates one JSON object and retains only the raw fields
// named by keys. Duplicate names keep the last value, matching encoding/json map
// semantics, and the returned slices alias body until binding completes.
func decodeJSONFields(body []byte, keys []string) (map[string]json.RawMessage, error) {
	return decodeJSONFieldsSet(body, newRequestKeySet(keys))
}

// decodeJSONFieldsSet is the request-path variant; keys was built at route
// generation time, so member matching is a constant-time map lookup.
func decodeJSONFieldsSet(body []byte, keys requestKeySet) (map[string]json.RawMessage, error) {
	if !json.Valid(body) {
		return nil, fmt.Errorf("invalid JSON body: %w", json.Unmarshal(body, &json.RawMessage{}))
	}

	index := skipJSONSpace(body, 0)
	if index+4 <= len(body) && bytes.Equal(body[index:index+4], []byte("null")) {
		return nil, multipleRequireError
	}
	if body[index] != '{' {
		return nil, errors.New("invalid JSON body: expected an object")
	}

	fields := make(map[string]json.RawMessage, len(keys))
	index = skipJSONSpace(body, index+1)
	for body[index] != '}' {
		keyStart := index
		index = scanJSONStringEnd(body, index)
		var name string
		// The whole object is already valid, so unmarshalling a JSON string into
		// a Go string cannot fail.
		_ = json.Unmarshal(body[keyStart:index], &name)
		index = skipJSONSpace(body, index)
		index = skipJSONSpace(body, index+1)
		valueStart := index
		index = skipJSONValueEnd(body, index)
		if _, ok := keys[name]; ok {
			fields[name] = body[valueStart:index]
		}
		index = skipJSONSpace(body, index)
		switch body[index] {
		case ',':
			index = skipJSONSpace(body, index+1)
		case '}':
			return fields, nil
		}
	}
	return fields, nil
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

// injectedParameters fills the reflection argument list for a handler call, driven
// by the handler precomputed bind plan. Each parameter is injected directly
// (request, writer, file headers) or bound by name from path parameters, a JSON
// body, or form values, in that precedence order. Type classification already
// happened in generateRoute, so nothing here re-inspects the signature with
// reflection. params is the scratch slice owned by the caller, already sized to
// parameter count; the caller recycles it. The jsonMedia flag already carries
// the empty-body check, so the body is never re-scanned here.
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
		case classSSE:
			params[i] = reflect.ValueOf(c.sse)
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

		if jsonMedia {
			// A lone business parameter decodes the whole body; several split
			// the JSON object by parameter name. Decode only after path precedence
			// has failed for the first parameter that actually needs JSON.
			if len(plan.businessIndexes) > 1 && jsonFields == nil {
				var err error
				jsonFields, err = decodeJSONFieldsSet(body, plan.jsonKeySet)
				if err != nil {
					return err
				}
			}
			raw := body
			found := true
			if len(plan.businessIndexes) > 1 {
				raw, found = jsonFields[name]
			}
			var v reflect.Value
			if found {
				var decoded reflect.Value
				var err error
				if len(plan.businessIndexes) == 1 && selfDescribing(t) {
					decoded, err = decodeJSONExactStruct(t, raw)
				} else {
					decoded, err = decodeJSON(t, raw)
				}
				if err != nil {
					return fmt.Errorf(jsonParameterErrFormat, name, err)
				}
				v = decoded
			} else {
				v = reflect.New(t).Elem()
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

// hasNonWhitespace reports whether data holds at least one byte beyond JSON
// whitespace (space, tab, carriage return or newline). Like TrimSpace, the scan
// is allocation-free and does not copy data; the difference is that TrimSpace
// also treats Unicode whitespace as blank, while JSON does not.
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
}

// contains reports whether t is already active on the current branch.
func (s *formBindState) contains(t reflect.Type) bool {
	for _, active := range s.active[:s.size] {
		if active == t {
			return true
		}
	}
	return false
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

// bindStructFieldsState binds one branch of a nested struct. Re-entering a type
// already on this branch is a recursion guard, not a reason to reject repeated
// sibling fields of the same type.
func bindStructFieldsState(t reflect.Type, fields []structField, src *nameSources,
	state *formBindState) (reflect.Value, error) {
	if state.contains(t) {
		return reflect.Zero(t), nil
	}
	if err := state.push(t); err != nil {
		return reflect.Value{}, err
	}
	defer state.pop()

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

		value, err := bindFormParameterDepth(field.typ, field.name, src, 0, state)
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
// lone struct parameter. Valid objects are filtered recursively before the
// standard decoder runs, preserving its pointer, slice, map, optional-field and
// UnmarshalTypeError behavior. Types with their own JSON decoder keep ownership
// of their representation.
func decodeJSONExactStruct(t reflect.Type, data []byte) (reflect.Value, error) {
	if t == nil {
		return reflect.Value{}, errors.New("nil JSON target type")
	}
	terminal, ok := derefType(t)
	if ok && terminal.Kind() == reflect.Struct && !hasCustomJSONDecoder(terminal) && json.Valid(data) {
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

// bindTextValue converts raw request strings into a scalar (or pointer to
// scalar) of type t. Only the first value is used; repeated values should be
// bound to a slice or array parameter instead.
func bindTextValue(t reflect.Type, values []string) (reflect.Value, error) {
	return bindTextValueDepth(t, values, 0)
}

// bindTextValueDepth bounds pointer recursion for path and scalar binding.
func bindTextValueDepth(t reflect.Type, values []string, depth int) (reflect.Value, error) {
	if t.Kind() == reflect.Pointer {
		if depth >= maxTypePointerDepth {
			return reflect.Value{}, fmt.Errorf("recursive pointer type: %s", t)
		}
		if len(values) == 0 {
			return reflect.Zero(t), nil
		}
		value, err := bindTextValueDepth(t.Elem(), values, depth+1)
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
