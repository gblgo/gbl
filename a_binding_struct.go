// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// This file holds struct field caches, promoted-field validation, and bind-name conflict checks.

// bindFieldCandidate is one visible member in a struct's promoted binding view.
type bindFieldCandidate struct {
	// name is the exact request key after applying the binding name rule.
	name string
	// path identifies the field from the validated struct root.
	path string
	// depth counts field and anonymous-embedding edges from that root.
	depth int
	// tagged records whether an explicit JSON name supplied the bind key.
	tagged bool
}

// structBindValidation is the immutable validation result cached for one struct.
type structBindValidation struct {
	// err is nil exactly when the struct has unambiguous bind names.
	err error
}

// structBindNameCache stores startup validation results by struct type. Keeping
// valid results too avoids rewalking shared request models across routes.
var structBindNameCache sync.Map

// prewarmNestedStructs validates every reachable struct and warms both field
// caches, including structs inside pointers, arrays, slices and maps. The visited
// set is local to one bind plan; validation results remain cached across plans.
func prewarmNestedStructs(t reflect.Type, visited map[reflect.Type]bool) error {
	pending := []reflect.Type{t}
	for len(pending) > 0 {
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		current, ok := derefType(current)
		if !ok || visited[current] {
			continue
		}
		visited[current] = true
		switch current.Kind() {
		case reflect.Struct:
			if err := validateStructBindNames(current); err != nil {
				return err
			}
			_ = isRecursiveStruct(current)
			_ = jsonStructFieldsOf(current)
			for field := range current.Fields() {
				if bindableStructField(field) {
					pending = append(pending, field.Type)
				}
			}
		case reflect.Array, reflect.Slice, reflect.Map:
			pending = append(pending, current.Elem())
		default:
			// Other kinds cannot contain statically known nested structs.
		}
	}
	return nil
}

// validateStructBindNames returns the cached ambiguity result for a struct type.
func validateStructBindNames(t reflect.Type) error {
	if cached, ok := structBindNameCache.Load(t); ok {
		return cached.(structBindValidation).err
	}
	err := computeStructBindNameError(t)
	actual, _ := structBindNameCache.LoadOrStore(t, structBindValidation{err: err})
	return actual.(structBindValidation).err
}

// computeStructBindNameError checks the promoted form/JSON binding view.
// Shallower fields dominate deeper embedded members; equal-depth conflicts fail
// unless one exact name has a single explicit-tag claimant.
func computeStructBindNameError(t reflect.Type) error {
	seen := make(map[string]bindFieldCandidate, t.NumField())
	for field := range t.Fields() {
		if !bindableStructField(field) {
			continue
		}
		if field.Anonymous && !field.IsExported() && field.Type.Kind() == reflect.Pointer {
			if terminal, ok := derefType(field.Type); ok && terminal.Kind() == reflect.Struct {
				return fmt.Errorf("struct %s has an unexported anonymous pointer %s that cannot be allocated", t, field.Name)
			}
		}
		name, tagged := structFieldBindName(field)
		if field.Anonymous && !tagged {
			if terminal, ok := derefType(field.Type); ok && terminal.Kind() == reflect.Struct {
				continue
			}
		}
		candidate := bindFieldCandidate{name: name, path: field.Name, depth: 1}
		folded := foldBindName(name)
		if previous, exists := seen[folded]; exists {
			return bindFieldConflictError(t, []bindFieldCandidate{previous, candidate})
		}
		seen[folded] = candidate
	}
	return resolvePromotedBindFields(t, collectPromotedBindFields(t))
}

// collectPromotedBindFields walks anonymous structs breadth-first. Active types
// are tracked per path, which stops recursive embedding without hiding repeated
// sibling paths that can make a promoted member ambiguous.
func collectPromotedBindFields(t reflect.Type) []bindFieldCandidate {
	// bindEmbeddedType is one anonymous-struct path queued for promotion.
	type bindEmbeddedType struct {
		// typ is the embedded struct to scan.
		typ reflect.Type
		// path identifies this embedding from the validated root.
		path string
		// depth is the number of embedding edges already followed.
		depth int
		// active holds the struct types on this embedding path.
		active []reflect.Type
	}

	queue := []bindEmbeddedType{{typ: t, active: []reflect.Type{t}}}
	var candidates []bindFieldCandidate
	for head := 0; head < len(queue); head++ {
		embedded := queue[head]
		for field := range embedded.typ.Fields() {
			if !bindableStructField(field) {
				continue
			}
			fieldType := field.Type
			if field.Anonymous && fieldType.Kind() == reflect.Pointer {
				fieldType = fieldType.Elem()
			}
			name, tagged := jsonStructFieldName(field)
			path := field.Name
			if embedded.path != empty {
				path = embedded.path + "." + field.Name
			}
			if field.Anonymous && !tagged && fieldType.Kind() == reflect.Struct {
				if slices.Contains(embedded.active, fieldType) {
					continue
				}
				active := append(slices.Clone(embedded.active), fieldType)
				queue = append(queue, bindEmbeddedType{
					typ:    fieldType,
					path:   path,
					depth:  embedded.depth + 1,
					active: active,
				})
				continue
			}
			candidates = append(candidates, bindFieldCandidate{
				name:   name,
				path:   path,
				depth:  embedded.depth + 1,
				tagged: tagged,
			})
		}
	}
	return candidates
}

// resolvePromotedBindFields rejects folded-name collisions that remain after
// shallow-member dominance. At equal depth, one explicit JSON name may dominate
// untagged claims only when every contender has that same exact name.
func resolvePromotedBindFields(t reflect.Type, candidates []bindFieldCandidate) error {
	groups := make(map[string]int)
	grouped := make([][]bindFieldCandidate, 0)
	for _, candidate := range candidates {
		folded := foldBindName(candidate.name)
		index, exists := groups[folded]
		if !exists {
			index = len(grouped)
			groups[folded] = index
			grouped = append(grouped, nil)
		}
		grouped[index] = append(grouped[index], candidate)
	}
	for _, group := range grouped {
		shallowest := group[0].depth
		contenders := make([]bindFieldCandidate, 0, len(group))
		for _, candidate := range group {
			if candidate.depth == shallowest {
				contenders = append(contenders, candidate)
			}
		}
		if len(contenders) == 1 || uniqueTaggedBindName(contenders) {
			continue
		}
		return bindFieldConflictError(t, contenders)
	}
	return nil
}

// uniqueTaggedBindName reports whether equal-depth claims have one exact name
// and one explicitly tagged claimant, matching JSON's existing tag dominance.
func uniqueTaggedBindName(candidates []bindFieldCandidate) bool {
	tagged := -1
	for i, candidate := range candidates {
		if candidate.name != candidates[0].name {
			return false
		}
		if candidate.tagged {
			if tagged >= 0 {
				return false
			}
			tagged = i
		}
	}
	return tagged >= 0
}

// bindFieldConflictError identifies the struct, bind keys and competing paths.
func bindFieldConflictError(t reflect.Type, candidates []bindFieldCandidate) error {
	details := make([]string, len(candidates))
	for i, candidate := range candidates {
		details[i] = fmt.Sprintf("%s (%q)", candidate.path, candidate.name)
	}
	duplicate := true
	for _, candidate := range candidates[1:] {
		if candidate.name != candidates[0].name {
			duplicate = false
			break
		}
	}
	if duplicate {
		return fmt.Errorf(
			"ambiguous bind name %q in struct %s: duplicate exact bind name on fields %s",
			candidates[0].name, t, strings.Join(details, ", "),
		)
	}
	return fmt.Errorf(
		"ambiguous bind names in struct %s: Unicode case-folding conflict between fields %s",
		t, strings.Join(details, ", "),
	)
}

// foldBindName returns a deterministic key for Unicode simple case folding.
// Each rune is represented by the smallest member of its complete fold cycle.
func foldBindName(name string) string {
	var folded strings.Builder
	folded.Grow(len(name))
	for _, r := range name {
		canonical := r
		for next := unicode.SimpleFold(r); next != r; next = unicode.SimpleFold(next) {
			if next < canonical {
				canonical = next
			}
		}
		folded.WriteRune(canonical)
	}
	return folded.String()
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

// bindableStructField reports whether a field participates in struct binding.
// Unexported anonymous structs may still promote their exported members.
func bindableStructField(field reflect.StructField) bool {
	if field.Tag.Get("json") == "-" {
		return false
	}
	fieldType := field.Type
	if field.Anonymous && fieldType.Kind() == reflect.Pointer {
		fieldType = fieldType.Elem()
	}
	if field.IsExported() {
		return true
	}
	return field.Anonymous && fieldType.Kind() == reflect.Struct
}

// jsonStructFieldName applies encoding/json's explicit-name validation and the
// binding fallback for an empty or invalid tag name.
func jsonStructFieldName(field reflect.StructField) (string, bool) {
	name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
	if !validJSONFieldName(name) {
		return lowerFirst(field.Name), false
	}
	return name, true
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

	var current []jsonEmbeddedType
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
				if !bindableStructField(field) {
					continue
				}
				fieldType := field.Type
				if field.Anonymous && fieldType.Kind() == reflect.Pointer {
					fieldType = fieldType.Elem()
				}

				name, tagged := jsonStructFieldName(field)
				index := make([]int, len(embedded.index)+1)
				copy(index, embedded.index)
				index[len(embedded.index)] = i

				if field.Anonymous && !tagged && fieldType.Kind() == reflect.Struct {
					nextCount[fieldType]++
					if nextCount[fieldType] == 1 {
						next = append(next, jsonEmbeddedType{typ: fieldType, index: index})
					}
					continue
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

// structFieldBindName returns the exact form/JSON bind key for a direct field.
// Unlike encoding/json validation, form binding preserves any non-empty tag name.
func structFieldBindName(field reflect.StructField) (string, bool) {
	if tag, ok := field.Tag.Lookup("json"); ok {
		if name, _, _ := strings.Cut(tag, ","); name != "" {
			return name, true
		}
	}
	return lowerFirst(field.Name), false
}

// computeStructFields flattens the effective form-binding view once at startup.
// Anonymous structs promote their members, shallow fields dominate deeper ones,
// and an explicit tag wins an equal-depth untagged claim with the same name.
func computeStructFields(t reflect.Type) []structField {
	type embeddedType struct {
		typ   reflect.Type
		index []int
		depth int
	}
	type candidate struct {
		name   string
		index  []int
		typ    reflect.Type
		depth  int
		tagged bool
	}

	queue := []embeddedType{{typ: t}}
	visited := make(map[reflect.Type]bool)
	var candidates []candidate
	for len(queue) > 0 {
		embedded := queue[0]
		queue = queue[1:]
		if visited[embedded.typ] {
			continue
		}
		visited[embedded.typ] = true
		for i := range embedded.typ.NumField() {
			field := embedded.typ.Field(i)
			if !bindableStructField(field) {
				continue
			}
			fieldType := field.Type
			if field.Anonymous && fieldType.Kind() == reflect.Pointer {
				fieldType = fieldType.Elem()
			}
			name, tagged := structFieldBindName(field)
			index := append(slices.Clone(embedded.index), i)
			if field.Anonymous && !tagged && fieldType.Kind() == reflect.Struct {
				queue = append(queue, embeddedType{typ: fieldType, index: index, depth: embedded.depth + 1})
				continue
			}
			candidates = append(candidates, candidate{
				name: name, index: index, typ: field.Type,
				depth: embedded.depth + 1, tagged: tagged,
			})
		}
	}

	winners := make(map[string]candidate, len(candidates))
	for _, field := range candidates {
		key := foldBindName(field.name)
		previous, exists := winners[key]
		if !exists || field.depth < previous.depth ||
			(field.depth == previous.depth && field.tagged && !previous.tagged &&
				field.name == previous.name) {
			winners[key] = field
		}
	}
	fields := make([]structField, 0, len(winners))
	for _, field := range winners {
		fields = append(fields, structField{index: field.index, name: field.name, typ: field.typ})
	}
	slices.SortFunc(fields, func(a, b structField) int {
		return slices.Compare(a.index, b.index)
	})
	return fields
}
