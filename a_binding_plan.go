// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"reflect"
)

// This file holds binding plans, type classification, and request-key preparation.

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
	// present, else the Go field name with its first letter lowercased. Thus, a
	// lowercase request key binds to an exported PascalCase field without a tag.
	structField struct {
		// index is the field path through any promoted anonymous structs.
		index []int
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
		// pathBindable is parallel to types and records whether a whole path value
		// can bind the parameter. Structs bind their fields instead of their outer name.
		pathBindable []bool
		// jsonExact records whether JSON decoding must enforce exact struct member
		// names, including inside top-level containers.
		jsonExact []bool
		// structFields is parallel to types and caches top-level struct fields
		structFields [][]structField
		// businessIndexes identifies parameters bound from request data
		businessIndexes []int
		// writerOK is parallel to types, non-nil only when a parameter is a writer
		// (classWriter): it records whether the pooled writer satisfies that
		// parameter type, decided once here so the request path injects without
		// re-running the reflection package interface-satisfaction probe.
		writerOK []bool
		// validatableIndexes identifies business parameters that implement
		// Validatable. validatablePointers is parallel and records whether the
		// addressable value must be used to avoid boxing a struct copy per request.
		validatableIndexes  []int
		validatablePointers []bool
		// queryKeys is the set of exact query names consumed by this handler.
		queryKeys requestKeySet
		// jsonKeys lists the top-level members needed by multi-parameter JSON handlers.
		jsonKeys []string
		// jsonKeySet tracks accepted member names and their count.
		jsonKeySet requestKeySet
		// needsParse records whether request data must be parsed
		needsParse bool
		// hasSSE is true when any parameter declares gbl.SSEWriter.
		hasSSE bool
		// needsNames is true when at least one business or file parameter must be
		// matched against the request by its handler parameter name. A lone struct
		// or pointer-to-struct is self-describing: its fields come from reflection,
		// so it binds correctly even when DWARF names are unavailable.
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

// maxTypePointerDepth bounds walks through recursive named pointer types.
const maxTypePointerDepth = 64

// maxFormPointerExpansion bounds the nodes a non-recursive struct pointer tree
// can allocate for an empty form request.
const maxFormPointerExpansion = 4096

// maxFormPointerExpansionSteps bounds startup graph traversal independently of node count.
var maxFormPointerExpansionSteps = maxFormPointerExpansion * 8

// validateFormPointerExpansion rejects pointer-to-struct trees whose empty-form
// allocation would grow exponentially. Recursive back-edges contribute no eager
// nodes, while non-recursive branches inside a recursive root still count.
func validateFormPointerExpansion(t reflect.Type) error {
	memo := make(map[reflect.Type]int)
	active := make(map[reflect.Type]bool)
	steps := 0
	var expansion func(reflect.Type) (int, bool)
	expansion = func(current reflect.Type) (int, bool) {
		steps++
		if steps > maxFormPointerExpansionSteps {
			return maxFormPointerExpansion + 1, true
		}
		pointers := 0
		for current.Kind() == reflect.Pointer {
			if pointers >= maxTypePointerDepth {
				return maxFormPointerExpansion + 1, true
			}
			pointers++
			current = current.Elem()
		}
		if current.Kind() != reflect.Struct {
			return 0, false
		}
		if active[current] {
			return 0, true
		}
		if count, ok := memo[current]; ok {
			return count, false
		}
		active[current] = true
		total := pointers
		cyclic := false
		for _, field := range structFieldsOf(current) {
			count, childCyclic := expansion(field.typ)
			cyclic = cyclic || childCyclic
			if count > maxFormPointerExpansion || total > maxFormPointerExpansion-count {
				total = maxFormPointerExpansion + 1
				break
			}
			total += count
		}
		delete(active, current)
		if !cyclic {
			memo[current] = total
		}
		return total, cyclic
	}
	if count, _ := expansion(t); count > maxFormPointerExpansion {
		return fmt.Errorf("form pointer expansion for %s exceeds %d nodes", t, maxFormPointerExpansion)
	}
	return nil
}

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
// its values by field, which reflection supplies even when DWARF is unavailable
// at runtime; every other kind (scalar, slice, map, interface and
// pointer to those) is matched against the request by the parameter name.
func selfDescribing(t reflect.Type) bool {
	terminal, ok := derefType(t)
	return ok && terminal.Kind() == reflect.Struct
}

// jsonTypeNeedsExactFilter reports whether a JSON target contains a struct whose
// exact member names must be enforced. The result is computed once per bind plan.
func jsonTypeNeedsExactFilter(t reflect.Type) bool {
	return jsonTypeNeedsExactFilterSeen(t, make(map[reflect.Type]bool))
}

func jsonTypeNeedsExactFilterSeen(t reflect.Type, seen map[reflect.Type]bool) bool {
	terminal, ok := derefType(t)
	if !ok || seen[terminal] || hasCustomJSONDecoder(terminal) {
		return false
	}
	seen[terminal] = true
	switch terminal.Kind() {
	case reflect.Struct:
		return true
	case reflect.Array, reflect.Slice:
		if terminal.Elem().Kind() == reflect.Uint8 {
			return false
		}
		return jsonTypeNeedsExactFilterSeen(terminal.Elem(), seen)
	case reflect.Map:
		return jsonTypeNeedsExactFilterSeen(terminal.Elem(), seen)
	default:
		return false
	}
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
// indexes, rejects ambiguous struct fields in every reachable business type,
// caches the struct field lookups, and flags whether request parsing is needed.
func buildBindPlan(rt reflect.Type) (bindPlan, error) {
	n := rt.NumIn()
	plan := bindPlan{
		types:        make([]reflect.Type, n),
		classes:      make([]paramClass, n),
		pathBindable: make([]bool, n),
		jsonExact:    make([]bool, n),
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
			plan.pathBindable[i] = !selfDescribing(t)
			plan.jsonExact[i] = jsonTypeNeedsExactFilter(t)
			plan.needsParse = true
			if err := validateFormPointerExpansion(t); err != nil {
				return plan, err
			}
			if err := prewarmNestedStructs(t, visited); err != nil {
				return plan, err
			}
			if t.Kind() == reflect.Struct {
				plan.structFields[i] = structFieldsOf(t)
			}
			if !selfDescribing(t) {
				plan.needsNames = true
			}
			if t.Kind() != reflect.Pointer && reflect.PointerTo(t).Implements(validatableType) {
				plan.validatableIndexes = append(plan.validatableIndexes, i)
				plan.validatablePointers = append(plan.validatablePointers, true)
			} else if t.Implements(validatableType) {
				plan.validatableIndexes = append(plan.validatableIndexes, i)
				plan.validatablePointers = append(plan.validatablePointers, false)
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
