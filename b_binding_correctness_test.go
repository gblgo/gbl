// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// CorrectnessEmbedded is an exported embedding fixture so promoted fields are
// settable through reflection just as they are for application handlers.
type CorrectnessEmbedded struct {
	Value   int    `json:"value"`
	Default string `json:",omitempty"`
	Ignored string `json:"-"`
}

// CorrectnessEmbedding exercises promotion through an embedded struct value.
type CorrectnessEmbedding struct {
	CorrectnessEmbedded
}

// CorrectnessNoTagEmbedded supplies default bind names without JSON tags.
type CorrectnessNoTagEmbedded struct {
	Value int
	Label string
}

// CorrectnessNoTagEmbedding promotes untagged embedded fields.
type CorrectnessNoTagEmbedding struct {
	CorrectnessNoTagEmbedded
}

// CorrectnessPointerEmbedding exercises promotion through an embedded pointer.
type CorrectnessPointerEmbedding struct {
	*CorrectnessEmbedded
}

// CorrectnessNamedEmbedding gives an anonymous struct its own object member.
type CorrectnessNamedEmbedding struct {
	CorrectnessEmbedded `json:"boxed"`
}

// CorrectnessIgnoredEmbedding suppresses an anonymous struct and its fields.
type CorrectnessIgnoredEmbedding struct {
	CorrectnessEmbedded `json:"-"`
}

// CorrectnessLeft and CorrectnessRight claim the same JSON member at one depth.
type CorrectnessLeft struct {
	Left   int    `json:"left"`
	Shared string `json:"shared"`
}

// CorrectnessRight conflicts with CorrectnessLeft.Shared.
type CorrectnessRight struct {
	Right  int `json:"right"`
	Shared int `json:"shared"`
}

// CorrectnessBindCaseConflict has distinct exact names in one SimpleFold cycle.
type CorrectnessBindCaseConflict struct {
	Name string
	NAME string
}

// CorrectnessBindUnicodeFoldConflict uses the Kelvin sign's ASCII fold cycle.
type CorrectnessBindUnicodeFoldConflict struct {
	K      string `json:"k"`
	Kelvin string `json:"K"`
}

// CorrectnessBindDirectTagConflict is ambiguous in the flat form namespace even
// though JSON alone would give the explicit tag priority.
type CorrectnessBindDirectTagConflict struct {
	Value int
	Other string `json:"value"`
}

// CorrectnessBindFoldLeft contributes one member of a Unicode fold cycle.
type CorrectnessBindFoldLeft struct {
	K int `json:"k"`
}

// CorrectnessBindFoldRight contributes the other member of that fold cycle.
type CorrectnessBindFoldRight struct {
	Kelvin int `json:"K"`
}

// CorrectnessBindEmbeddedFoldConflict has equal-depth promoted fold collisions.
type CorrectnessBindEmbeddedFoldConflict struct {
	CorrectnessBindFoldLeft
	CorrectnessBindFoldRight
}

// CorrectnessBindRecursiveEmbedding exercises cycle detection in promotion.
type CorrectnessBindRecursiveEmbedding struct {
	Value int
	*CorrectnessBindRecursiveEmbedding
}

// correctnessBindDuplicateType builds duplicate exact tags dynamically so vet
// does not reject the intentionally ambiguous fixture.
func correctnessBindDuplicateType() reflect.Type {
	return reflect.StructOf([]reflect.StructField{
		{Name: "First", Type: reflect.TypeFor[int](), Tag: `json:"shared"`},
		{Name: "Second", Type: reflect.TypeFor[string](), Tag: `json:"shared"`},
	})
}

// correctnessConflictType builds two equally shallow tagged claims dynamically
// so the fixture does not trip the structtag vet analyzer.
func correctnessConflictType() reflect.Type {
	return reflect.StructOf([]reflect.StructField{
		{Name: "CorrectnessLeft", Type: reflect.TypeFor[CorrectnessLeft](), Anonymous: true},
		{Name: "CorrectnessRight", Type: reflect.TypeFor[CorrectnessRight](), Anonymous: true},
	})
}

// CorrectnessShallow lets its direct member dominate an embedded claim.
type CorrectnessShallow struct {
	Shared bool `json:"shared"`
	CorrectnessLeft
}

// CorrectnessPromotedEmbedded contributes fields without claiming its own form key.
type CorrectnessPromotedEmbedded struct {
	Value int `json:"value"`
}

// CorrectnessEmbeddedNameCollision has a direct tag matching the embedded type name.
type CorrectnessEmbeddedNameCollision struct {
	CorrectnessPromotedEmbedded
	Other string `json:"correctnessPromotedEmbedded"`
}

// CorrectnessDeep carries the embedded value through another struct level.
type CorrectnessDeep struct {
	CorrectnessEmbedding
}

// CorrectnessMiddle adds one more promotion level.
type CorrectnessMiddle struct {
	CorrectnessDeep
}

// CorrectnessShallowDeep lets a direct member dominate a deeper promoted one.
type CorrectnessShallowDeep struct {
	Value int `json:"value"`
	CorrectnessMiddle
}

// CorrectnessUntaggedLeft contributes an untagged value member.
type CorrectnessUntaggedLeft struct {
	Value int
}

// CorrectnessTaggedRight contributes a tagged value member at the same depth.
type CorrectnessTaggedRight struct {
	Other string `json:"value"`
}

// CorrectnessTagPriority gives the tagged same-depth claim priority.
type CorrectnessTagPriority struct {
	CorrectnessUntaggedLeft
	CorrectnessTaggedRight
}

// correctnessHiddenInt is an unexported anonymous non-struct embedding.
type correctnessHiddenInt int

type correctnessHiddenEmbedded struct {
	Value int `json:"value"`
}

// CorrectnessHiddenPointerEmbedding cannot allocate its unexported embedded pointer.
type CorrectnessHiddenPointerEmbedding struct {
	*correctnessHiddenEmbedded
}

type correctnessEndlessPointer *correctnessEndlessPointer

type correctnessEndlessPointerHolder struct {
	Next correctnessEndlessPointer
}

// CorrectnessHiddenFields mixes unexported fields with one visible member.
type CorrectnessHiddenFields struct {
	correctnessHiddenInt
	hidden  int
	Visible int `json:"visible"`
}

// CorrectnessPromotionShared is reached at two different promotion depths.
type CorrectnessPromotionShared struct {
	Value int `json:"value"`
}

// CorrectnessPromotionLeft reaches the shared type at the shallower depth.
type CorrectnessPromotionLeft struct {
	CorrectnessPromotionShared
}

// CorrectnessPromotionMiddle reaches the same type through another level.
type CorrectnessPromotionMiddle struct {
	CorrectnessPromotionShared
}

// CorrectnessPromotionRight delays the second path to the shared type.
type CorrectnessPromotionRight struct {
	CorrectnessPromotionMiddle
}

// correctnessPromotionRevisitType builds two promotion paths dynamically so the
// intentional duplicate JSON tag does not trip the structtag vet analyzer.
func correctnessPromotionRevisitType() reflect.Type {
	return reflect.StructOf([]reflect.StructField{
		{Name: "CorrectnessPromotionLeft", Type: reflect.TypeFor[CorrectnessPromotionLeft](), Anonymous: true},
		{Name: "CorrectnessPromotionRight", Type: reflect.TypeFor[CorrectnessPromotionRight](), Anonymous: true},
	})
}

// CorrectnessRepeatedShared is embedded through two equally shallow paths.
type CorrectnessRepeatedShared struct {
	Value int `json:"value"`
}

// CorrectnessRepeatedLeft supplies the first path to the repeated type.
type CorrectnessRepeatedLeft struct {
	CorrectnessRepeatedShared
}

// CorrectnessRepeatedRight supplies the second path to the repeated type.
type CorrectnessRepeatedRight struct {
	CorrectnessRepeatedShared
}

// correctnessRepeatedEmbeddingType builds the intentionally ambiguous repeated
// paths dynamically so vet does not reject the fixture's duplicate JSON tag.
func correctnessRepeatedEmbeddingType() reflect.Type {
	return reflect.StructOf([]reflect.StructField{
		{Name: "CorrectnessRepeatedLeft", Type: reflect.TypeFor[CorrectnessRepeatedLeft](), Anonymous: true},
		{Name: "CorrectnessRepeatedRight", Type: reflect.TypeFor[CorrectnessRepeatedRight](), Anonymous: true},
	})
}

// CorrectnessJSONItem is the leaf shape used by container filtering tests.
type CorrectnessJSONItem struct {
	Value int `json:"value"`
}

// CorrectnessJSONCustom owns its case-sensitive JSON representation.
type CorrectnessJSONCustom struct {
	Seen map[string]json.RawMessage
}

// UnmarshalJSON records the original member names for the custom-decoder check.
func (v *CorrectnessJSONCustom) UnmarshalJSON(data []byte) error {
	return json.Unmarshal(data, &v.Seen)
}

type correctnessEndlessJSONPointer *correctnessEndlessJSONPointer

type correctnessRecursiveJSONSlice []correctnessRecursiveJSONSlice

// CorrectnessJSONContainers covers every recursive container binder shape.
type CorrectnessJSONContainers struct {
	Map    map[string]CorrectnessJSONItem `json:"map"`
	Slice  []CorrectnessJSONItem          `json:"slice"`
	Array  [2]CorrectnessJSONItem         `json:"array"`
	Custom CorrectnessJSONCustom          `json:"custom"`
	Bytes  []byte                         `json:"bytes"`
}

// CorrectnessRecursiveNode is a recursive form-binding target with a leaf.
type CorrectnessRecursiveNode struct {
	Value int                       `json:"value"`
	Next  *CorrectnessRecursiveNode `json:"next"`
}

// CorrectnessRecursiveFileNode has an uploaded-file leaf and a recursive edge.
type CorrectnessRecursiveFileNode struct {
	Upload multipart.FileHeader          `json:"upload"`
	Next   *CorrectnessRecursiveFileNode `json:"next"`
}

// CorrectnessLeafBranch supplies a non-recursive nested leaf for allocation probes.
type CorrectnessLeafBranch struct {
	Value int `json:"value"`
}

// CorrectnessNestedRecursiveNode reaches its leaf through another struct.
type CorrectnessNestedRecursiveNode struct {
	Branch CorrectnessLeafBranch           `json:"branch"`
	Next   *CorrectnessNestedRecursiveNode `json:"next"`
}

// CorrectnessDeepNode is recursively nested for filter correctness and benchmarks.
type CorrectnessDeepNode struct {
	Value int                  `json:"value"`
	Next  *CorrectnessDeepNode `json:"next"`
}

// correctnessFilteredSink keeps benchmark rewrite results observable.
var correctnessFilteredSink []byte

// correctnessNeedsExactSink keeps allocation-test detection results observable.
var correctnessNeedsExactSink bool

// TestJSONVisitorsLeadingWhitespaceAndEmptyContainers guards the scanner start
// offsets, including empty containers surrounded by JSON whitespace.
func TestJSONVisitorsLeadingWhitespaceAndEmptyContainers(t *testing.T) {
	data := []byte(" \t\r\n { \"value\" : 1, \"nested\" : { \"flag\": true }, \"empty\": {} } \n")
	var names []string
	var values []string
	stopped := visitJSONObject(data, func(name string, _, value []byte) bool {
		names = append(names, name)
		values = append(values, string(value))
		return false
	})
	if stopped {
		t.Fatal("complete object visit reported an early stop")
	}
	if want := []string{"value", "nested", "empty"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("visited names = %#v, want %#v", names, want)
	}
	if want := []string{"1", `{ "flag": true }`, `{}`}; !reflect.DeepEqual(values, want) {
		t.Fatalf("visited values = %#v, want %#v", values, want)
	}

	visits := 0
	stopped = visitJSONObject(data, func(string, []byte, []byte) bool {
		visits++
		return visits == 2
	})
	if !stopped || visits != 2 {
		t.Fatalf("object early stop = (%v, %d), want (true, 2)", stopped, visits)
	}

	for _, tc := range []struct {
		input  string
		object bool
	}{
		{input: " \n\t {} \r", object: true},
		{input: " \t\r\n[] \n"},
	} {
		visits = 0
		if tc.object {
			stopped = visitJSONObject([]byte(tc.input), func(string, []byte, []byte) bool {
				visits++
				return false
			})
		} else {
			stopped = visitJSONArray([]byte(tc.input), func([]byte) bool {
				visits++
				return false
			})
		}
		if stopped || visits != 0 {
			t.Fatalf("empty container %q = (%v, %d), want (false, 0)", tc.input, stopped, visits)
		}
	}

	if got := string(filterExactJSONValue([]byte(" \n {} \t"), reflect.TypeFor[CorrectnessJSONItem]())); got != `{}` {
		t.Fatalf("filtered empty object = %q, want {}", got)
	}
	if got := string(filterExactJSONValue([]byte(" \n [ ] \t"), reflect.TypeFor[[]CorrectnessJSONItem]())); got != `[]` {
		t.Fatalf("filtered empty array = %q, want []", got)
	}
	if !jsonNeedsExactFilter(data, reflect.TypeFor[CorrectnessJSONItem]()) {
		t.Fatal("leading-whitespace object with unknown members did not request filtering")
	}
}

// TestJSONWalkersRejectOtherShapes checks empty, scalar and opposite-container
// inputs without invoking either visitor.
func TestJSONWalkersRejectOtherShapes(t *testing.T) {
	visits := 0
	for _, input := range []string{"", " \t\r\n", "null", "[]"} {
		if walkJSONObject([]byte(input), func([]byte, []byte) bool {
			visits++
			return false
		}) {
			t.Fatalf("walkJSONObject(%q) accepted a non-object", input)
		}
	}
	for _, input := range []string{"", " \t\r\n", "null", "{}"} {
		if walkJSONArray([]byte(input), func([]byte) bool {
			visits++
			return false
		}) {
			t.Fatalf("walkJSONArray(%q) accepted a non-array", input)
		}
	}
	if visits != 0 {
		t.Fatalf("rejected inputs invoked visitors %d times", visits)
	}
	for _, tc := range []struct {
		input string
		want  byte
	}{
		{input: " \n\t", want: 0},
		{input: " \n {", want: '{'},
		{input: "\r [", want: '['},
		{input: " null", want: 'n'},
	} {
		if got := jsonFirstNonSpace([]byte(tc.input)); got != tc.want {
			t.Errorf("jsonFirstNonSpace(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// TestJSONExactParserScalarsAndShapeMismatch checks raw-value retention and the
// parser offset after scalars, mismatched containers and trailing whitespace.
func TestJSONExactParserScalarsAndShapeMismatch(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		typ   reflect.Type
		want  string
	}{
		{name: "number", input: " \n -12.5e+2 \t\r\n", typ: reflect.TypeFor[int](), want: "-12.5e+2"},
		{name: "string", input: " \n \"a,b:}]\" \t", typ: reflect.TypeFor[string](), want: `"a,b:}]"`},
		{name: "null", input: " null \r\n", typ: reflect.TypeFor[*int](), want: "null"},
		{name: "object for slice", input: " \t { \"Value\" : 1 } \r\n", typ: reflect.TypeFor[[]CorrectnessJSONItem](), want: `{ "Value" : 1 }`},
		{name: "array for struct", input: " [ {\"Value\":1} ] ", typ: reflect.TypeFor[CorrectnessJSONItem](), want: `[ {"Value":1} ]`},
		{name: "custom decoder", input: " {\"Value\":1} ", typ: reflect.TypeFor[CorrectnessJSONCustom](), want: `{"Value":1}`},
		{name: "byte slice", input: " [1,2] ", typ: reflect.TypeFor[[]byte](), want: `[1,2]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := []byte(tc.input)
			var out bytes.Buffer
			parser := jsonExactParser{data: data}
			if parser.parse(tc.typ, &out) {
				t.Fatal("raw value requested exact filtering")
			}
			if parser.index != len(data) {
				t.Fatalf("parser index = %d, want %d", parser.index, len(data))
			}
			if got := out.String(); got != tc.want {
				t.Fatalf("parsed value = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestJSONExactParserDefensiveBoundaries checks that empty input and a nil target
// return without advancing or writing, rather than indexing past the input.
func TestJSONExactParserDefensiveBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []byte
		typ  reflect.Type
	}{
		{name: "empty input", typ: reflect.TypeFor[int]()},
		{name: "nil target", data: []byte(`{"value":1}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			parser := jsonExactParser{data: tc.data}
			if parser.value(tc.typ, &out) {
				t.Fatal("defensive boundary requested filtering")
			}
			if parser.index != 0 || out.Len() != 0 {
				t.Fatalf("defensive boundary advanced to %d and wrote %q", parser.index, out.String())
			}
		})
	}
}

// TestJSONExactParserUnterminatedKey covers the scanner's direct-call fallback;
// validated request input always takes the closing-quote path instead.
func TestJSONExactParserUnterminatedKey(t *testing.T) {
	data := []byte(`"unterminated`)
	parser := jsonExactParser{data: data}
	rawKey, escaped := parser.scanKey()
	if escaped || string(rawKey) != string(data) || parser.index != len(data) {
		t.Fatalf("unterminated key = (%q, %t, %d), want (%q, false, %d)",
			rawKey, escaped, parser.index, data, len(data))
	}
}

// TestJSONExactParserContainerBoundaries checks whitespace around commas and
// colons, including commas after discarded members and shape-mismatched elements.
func TestJSONTypeNeedsExactFilterBoundaries(t *testing.T) {
	tests := []struct {
		name string
		typ  reflect.Type
		want bool
	}{
		{name: "struct", typ: reflect.TypeFor[CorrectnessJSONItem](), want: true},
		{name: "slice of structs", typ: reflect.TypeFor[[]CorrectnessJSONItem](), want: true},
		{name: "map of structs", typ: reflect.TypeFor[map[string]CorrectnessJSONItem](), want: true},
		{name: "bytes", typ: reflect.TypeFor[[]byte](), want: false},
		{name: "scalar", typ: reflect.TypeFor[int](), want: false},
		{name: "custom decoder", typ: reflect.TypeFor[CorrectnessJSONCustom](), want: false},
		{name: "endless pointer", typ: reflect.TypeFor[correctnessEndlessJSONPointer](), want: false},
		{name: "recursive slice", typ: reflect.TypeFor[correctnessRecursiveJSONSlice](), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := jsonTypeNeedsExactFilter(tt.typ); got != tt.want {
				t.Fatalf("jsonTypeNeedsExactFilter(%s) = %t, want %t", tt.typ, got, tt.want)
			}
		})
	}
	if got := jsonKeyIndexString([]string{"name"}, "missing"); got != -1 {
		t.Fatalf("jsonKeyIndexString miss = %d, want -1", got)
	}
}

func TestJSONExactParserContainerBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		typ   reflect.Type
		want  string
	}{
		{
			name:  "object",
			input: " \n { \"value\" \t : \r\n 1 , \"unknown\" : { \"items\" : [1, 2] } , \"next\" : { \"Value\" : 9 , \"value\" : 2 } } \t",
			typ:   reflect.TypeFor[CorrectnessDeepNode](),
			want:  `{"value":1,"next":{"value":2}}`,
		},
		{
			name:  "array",
			input: " \n [ {\"value\":1,\"Value\":9} , 2 , {\"value\":3} ] \t",
			typ:   reflect.TypeFor[[]CorrectnessJSONItem](),
			want:  `[{"value":1},2,{"value":3}]`,
		},
		{
			name:  "map",
			input: ` {"a" : {"value":1,"Value":2},"b":[{"value":3}]} `,
			typ:   reflect.TypeFor[map[string]CorrectnessJSONItem](),
			want:  `{"a":{"value":1},"b":[{"value":3}]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := []byte(tc.input)
			var out bytes.Buffer
			parser := jsonExactParser{data: data}
			if !parser.parse(tc.typ, &out) {
				t.Fatal("nested wrong-case member did not request filtering")
			}
			if parser.index != len(data) {
				t.Fatalf("parser index = %d, want %d", parser.index, len(data))
			}
			if got := out.String(); got != tc.want {
				t.Fatalf("parsed containers = %s, want %s", got, tc.want)
			}
		})
	}
}

// TestJSONStructFieldsPromotionAndConflicts checks the exact-member lookup used
// before encoding/json applies its broader case-insensitive matching.
func TestJSONStructFieldsPromotionAndConflicts(t *testing.T) {
	for _, typ := range []reflect.Type{
		reflect.TypeFor[CorrectnessEmbedding](),
		reflect.TypeFor[CorrectnessPointerEmbedding](),
	} {
		fields := jsonStructFieldsOf(typ)
		if len(fields) != 2 {
			t.Fatalf("promoted fields for %v = %#v, want value and default", typ, fields)
		}
		if fields["value"] != reflect.TypeFor[int]() || fields["default"] != reflect.TypeFor[string]() {
			t.Fatalf("promoted field types for %v = %#v", typ, fields)
		}
		if _, exists := fields["Ignored"]; exists {
			t.Fatalf("ignored field survived for %v", typ)
		}

		value, err := decodeJSONExactStruct(typ, []byte(` {"Value":99,"value":1,"default":"bound","Ignored":"no"} `))
		if err != nil {
			t.Fatalf("decode promoted %v: %v", typ, err)
		}
		embedded := value.FieldByName("CorrectnessEmbedded")
		if typ == reflect.TypeFor[CorrectnessPointerEmbedding]() {
			if embedded.IsNil() {
				t.Fatal("embedded pointer was not allocated")
			}
			embedded = embedded.Elem()
		}
		if embedded.FieldByName("Value").Int() != 1 || embedded.FieldByName("Default").String() != "bound" {
			t.Fatalf("bound promoted %v = %#v", typ, embedded.Interface())
		}
	}

	named := jsonStructFieldsOf(reflect.TypeFor[CorrectnessNamedEmbedding]())
	if len(named) != 1 || named["boxed"] != reflect.TypeFor[CorrectnessEmbedded]() {
		t.Fatalf("named embedding fields = %#v, want only boxed", named)
	}
	if fields := jsonStructFieldsOf(reflect.TypeFor[CorrectnessIgnoredEmbedding]()); len(fields) != 0 {
		t.Fatalf("ignored embedding fields = %#v, want none", fields)
	}

	conflict := jsonStructFieldsOf(correctnessConflictType())
	if _, exists := conflict["shared"]; exists {
		t.Fatalf("equal-depth conflict survived: %#v", conflict)
	}
	if conflict["left"] != reflect.TypeFor[int]() || conflict["right"] != reflect.TypeFor[int]() {
		t.Fatalf("non-conflicting promoted fields = %#v", conflict)
	}
	if fields := jsonStructFieldsOf(reflect.TypeFor[CorrectnessShallow]()); fields["shared"] != reflect.TypeFor[bool]() {
		t.Fatalf("shallow dominance fields = %#v, want shared bool", fields)
	}
	if fields := jsonStructFieldsOf(reflect.TypeFor[CorrectnessShallowDeep]()); fields["value"] != reflect.TypeFor[int]() {
		t.Fatalf("shallow-over-deep fields = %#v, want value int", fields)
	}
	if fields := jsonStructFieldsOf(reflect.TypeFor[CorrectnessTagPriority]()); fields["value"] != reflect.TypeFor[string]() {
		t.Fatalf("tagged same-depth priority fields = %#v, want value string", fields)
	}
}

// TestJSONStructFieldsEmbeddingEdgeCases checks skipped fields, repeated types
// and a type that is queued again after its first breadth-first visit.
func TestJSONStructFieldsEmbeddingEdgeCases(t *testing.T) {
	hidden := jsonStructFieldsOf(reflect.TypeFor[CorrectnessHiddenFields]())
	if len(hidden) != 1 || hidden["visible"] != reflect.TypeFor[int]() {
		t.Fatalf("hidden embedding fields = %#v, want only visible", hidden)
	}

	revisited := jsonStructFieldsOf(correctnessPromotionRevisitType())
	if len(revisited) != 1 || revisited["value"] != reflect.TypeFor[int]() {
		t.Fatalf("revisited embedding fields = %#v, want one value", revisited)
	}

	repeated := jsonStructFieldsOf(correctnessRepeatedEmbeddingType())
	if len(repeated) != 0 {
		t.Fatalf("repeated embedding fields = %#v, want all members ambiguous", repeated)
	}

	invalidTag := reflect.StructOf([]reflect.StructField{{
		Name: "Value",
		Type: reflect.TypeFor[int](),
		Tag:  reflect.StructTag(`json:"bad\name"`),
	}})
	fallback := jsonStructFieldsOf(invalidTag)
	if len(fallback) != 1 || fallback["value"] != reflect.TypeFor[int]() {
		t.Fatalf("invalid-tag fallback fields = %#v, want value", fallback)
	}
}

// TestBuildBindPlanRejectsAmbiguousStructFields checks startup validation for
// direct fields, promoted fields and structs reached through every container.
func TestBuildBindPlanRejectsAmbiguousStructFields(t *testing.T) {
	duplicate := correctnessBindDuplicateType()
	tests := []struct {
		name string
		typ  reflect.Type
		want []string
	}{
		{
			name: "top-level case fold",
			typ:  reflect.TypeFor[CorrectnessBindCaseConflict](),
			want: []string{"gbl.CorrectnessBindCaseConflict", "Name", "NAME", "case-folding"},
		},
		{
			name: "duplicate exact tag",
			typ:  duplicate,
			want: []string{`"shared"`, "First", "Second", "duplicate exact bind name"},
		},
		{
			name: "direct tagged and untagged claims",
			typ:  reflect.TypeFor[CorrectnessBindDirectTagConflict](),
			want: []string{"gbl.CorrectnessBindDirectTagConflict", "Value", "Other", "duplicate exact"},
		},
		{
			name: "top-level Unicode fold",
			typ:  reflect.TypeFor[CorrectnessBindUnicodeFoldConflict](),
			want: []string{"gbl.CorrectnessBindUnicodeFoldConflict", "K", "Kelvin", "case-folding"},
		},
		{
			name: "nested pointer",
			typ:  reflect.PointerTo(duplicate),
			want: []string{duplicate.String(), "First", "Second"},
		},
		{
			name: "nested slice",
			typ:  reflect.SliceOf(duplicate),
			want: []string{duplicate.String(), "First", "Second"},
		},
		{
			name: "nested map",
			typ:  reflect.MapOf(reflect.TypeFor[string](), duplicate),
			want: []string{duplicate.String(), "First", "Second"},
		},
		{
			name: "nested array",
			typ:  reflect.ArrayOf(2, duplicate),
			want: []string{duplicate.String(), "First", "Second"},
		},
		{
			name: "anonymous embedding",
			typ:  correctnessConflictType(),
			want: []string{"shared", "CorrectnessLeft.Shared", "CorrectnessRight.Shared"},
		},
		{
			name: "repeated embedding paths",
			typ:  correctnessRepeatedEmbeddingType(),
			want: []string{
				"value",
				"CorrectnessRepeatedLeft.CorrectnessRepeatedShared.Value",
				"CorrectnessRepeatedRight.CorrectnessRepeatedShared.Value",
			},
		},
		{
			name: "embedded Unicode fold",
			typ:  reflect.TypeFor[CorrectnessBindEmbeddedFoldConflict](),
			want: []string{
				"gbl.CorrectnessBindEmbeddedFoldConflict",
				"CorrectnessBindFoldLeft.K",
				"CorrectnessBindFoldRight.Kelvin",
				"case-folding",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fnType := reflect.FuncOf([]reflect.Type{tt.typ}, nil, false)
			_, err := buildBindPlan(fnType)
			if err == nil {
				t.Fatalf("buildBindPlan(%s) accepted an ambiguous struct", tt.typ)
			}
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %v, want substring %q", err, want)
				}
			}
		})
	}
}

func TestBindStructEmbeddedTypeNameDoesNotConflictWithDirectTag(t *testing.T) {
	typ := reflect.TypeFor[CorrectnessEmbeddedNameCollision]()
	if _, err := buildBindPlan(reflect.FuncOf([]reflect.Type{typ}, nil, false)); err != nil {
		t.Fatalf("embedded type name caused a false conflict: %v", err)
	}
	value, err := bindStruct(typ, &nameSources{form: url.Values{
		"value":                       {"9"},
		"correctnessPromotedEmbedded": {"label"},
	}})
	if err != nil {
		t.Fatalf("bind embedded name collision: %v", err)
	}
	got := value.Interface().(CorrectnessEmbeddedNameCollision)
	if got.Value != 9 || got.Other != "label" {
		t.Fatalf("bound embedded name collision = %#v", got)
	}
}

func TestBindStructPointerEmbeddingAllocatesPromotedPath(t *testing.T) {
	value, err := bindStruct(reflect.TypeFor[CorrectnessPointerEmbedding](), &nameSources{
		form: url.Values{"value": {"9"}, "default": {"bound"}},
	})
	if err != nil {
		t.Fatalf("bind pointer embedding: %v", err)
	}
	got := value.Interface().(CorrectnessPointerEmbedding)
	if got.CorrectnessEmbedded == nil || got.Value != 9 || got.Default != "bound" {
		t.Fatalf("bound pointer embedding = %#v", got)
	}
}

func TestBindStructShallowFieldDominatesEmbeddedMember(t *testing.T) {
	value, err := bindStruct(reflect.TypeFor[CorrectnessShallow](), &nameSources{
		form: url.Values{"shared": {"true"}, "left": {"7"}},
	})
	if err != nil {
		t.Fatalf("bind shallow dominance: %v", err)
	}
	got := value.Interface().(CorrectnessShallow)
	if !got.Shared || got.Left != 7 {
		t.Fatalf("bound shallow struct = %#v, want Shared=true Left=7", got)
	}
}

// TestBuildBindPlanAllowsUnambiguousStructs checks normal name derivation,
// recursive types and the existing shallow/tagged JSON dominance rules.
func TestBuildBindPlanAllowsUnambiguousStructs(t *testing.T) {
	defaults := reflect.TypeFor[CorrectnessJSONItem]()
	defaultPlan, err := buildBindPlan(reflect.FuncOf([]reflect.Type{defaults}, nil, false))
	if err != nil {
		t.Fatalf("build default field name: %v", err)
	}
	if got := defaultPlan.structFields[0][0].name; got != "value" {
		t.Fatalf("Value bind name = %q, want value", got)
	}

	nameType := reflect.StructOf([]reflect.StructField{
		{Name: "Name", Type: reflect.TypeFor[string]()},
		{Name: "CODE", Type: reflect.TypeFor[string]()},
	})
	namePlan, err := buildBindPlan(reflect.FuncOf([]reflect.Type{nameType}, nil, false))
	if err != nil {
		t.Fatalf("build ordinary field names: %v", err)
	}
	var names []string
	for _, field := range namePlan.structFields[0] {
		names = append(names, field.name)
	}
	if want := []string{"name", "cODE"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("ordinary bind names = %#v, want %#v", names, want)
	}

	acronymType := reflect.StructOf([]reflect.StructField{{Name: "NAME", Type: reflect.TypeFor[int]()}})
	acronymPlan, err := buildBindPlan(reflect.FuncOf([]reflect.Type{acronymType}, nil, false))
	if err != nil {
		t.Fatalf("build acronym field name: %v", err)
	}
	if got := acronymPlan.structFields[0][0].name; got != "nAME" {
		t.Fatalf("NAME bind name = %q, want nAME", got)
	}

	explicitType := reflect.StructOf([]reflect.StructField{
		{Name: "Value", Type: reflect.TypeFor[int](), Tag: `json:"NAME"`},
	})
	explicitPlan, err := buildBindPlan(reflect.FuncOf([]reflect.Type{explicitType}, nil, false))
	if err != nil {
		t.Fatalf("build explicit uppercase name: %v", err)
	}
	if got := explicitPlan.structFields[0][0].name; got != "NAME" {
		t.Fatalf("explicit bind name = %q, want NAME", got)
	}

	for _, typ := range []reflect.Type{
		reflect.TypeFor[CorrectnessEmbedding](),
		reflect.TypeFor[CorrectnessPointerEmbedding](),
		reflect.TypeFor[CorrectnessNamedEmbedding](),
		reflect.TypeFor[CorrectnessIgnoredEmbedding](),
		reflect.TypeFor[CorrectnessHiddenFields](),
		reflect.TypeFor[CorrectnessShallow](),
		reflect.TypeFor[CorrectnessShallowDeep](),
		reflect.TypeFor[CorrectnessTagPriority](),
		reflect.TypeFor[CorrectnessRecursiveNode](),
		reflect.TypeFor[CorrectnessBindRecursiveEmbedding](),
		reflect.TypeFor[[]CorrectnessJSONItem](),
		reflect.TypeFor[map[string]CorrectnessJSONItem](),
		reflect.TypeFor[[2]CorrectnessJSONItem](),
	} {
		t.Run(typ.String(), func(t *testing.T) {
			if _, err := buildBindPlan(reflect.FuncOf([]reflect.Type{typ}, nil, false)); err != nil {
				t.Fatalf("buildBindPlan(%s) rejected a legal struct: %v", typ, err)
			}
		})
	}
}

// TestBuildBindPlanCachesStructValidation checks that both accepted and rejected
// struct results are reused by later startup plans.
func TestBuildBindPlanCachesStructValidation(t *testing.T) {
	for _, tt := range []struct {
		name    string
		typ     reflect.Type
		wantErr bool
	}{
		{name: "valid", typ: reflect.TypeFor[CorrectnessJSONItem]()},
		{name: "invalid", typ: correctnessBindDuplicateType(), wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			structBindNameCache.Delete(tt.typ)
			fnType := reflect.FuncOf([]reflect.Type{tt.typ}, nil, false)
			_, firstErr := buildBindPlan(fnType)
			_, secondErr := buildBindPlan(fnType)
			if (firstErr != nil) != tt.wantErr || (secondErr != nil) != tt.wantErr {
				t.Fatalf("cached build errors = (%v, %v), want error = %t", firstErr, secondErr, tt.wantErr)
			}
			if firstErr != secondErr {
				t.Fatalf("validation was not reused: first %v, second %v", firstErr, secondErr)
			}
			cached, ok := structBindNameCache.Load(tt.typ)
			if !ok {
				t.Fatal("validation result was not cached")
			}
			if got := cached.(structBindValidation).err; got != firstErr {
				t.Fatalf("cached error = %v, want %v", got, firstErr)
			}
		})
	}
}

// TestFoldBindNameSimpleFoldCycles checks complete Unicode cycles rather than
// relying on lowercase or upper case alone.
func TestFoldBindNameSimpleFoldCycles(t *testing.T) {
	for _, tt := range []struct {
		input string
		want  string
	}{
		{input: "", want: ""},
		{input: "Name", want: "NAME"},
		{input: "nAME", want: "NAME"},
		{input: "kK", want: "KK"},
		{input: "Σςσ", want: "ΣΣΣ"},
		{input: "ſ", want: "S"},
		{input: "é", want: "É"},
		{input: "-_12", want: "-_12"},
	} {
		if got := foldBindName(tt.input); got != tt.want {
			t.Errorf("foldBindName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// TestCompareJSONFieldCandidates exercises both comparison directions directly,
// independently of the sorting algorithm's choice of argument order.
func TestCompareJSONFieldCandidates(t *testing.T) {
	for _, tc := range []struct {
		name string
		a    jsonFieldCandidate
		b    jsonFieldCandidate
		want int
	}{
		{
			name: "different names",
			a:    jsonFieldCandidate{name: "alpha"},
			b:    jsonFieldCandidate{name: "beta"},
			want: -1,
		},
		{
			name: "shallower index",
			a:    jsonFieldCandidate{name: "value", index: []int{0}},
			b:    jsonFieldCandidate{name: "value", index: []int{0, 0}},
			want: -1,
		},
		{
			name: "deeper index",
			a:    jsonFieldCandidate{name: "value", index: []int{0, 0}},
			b:    jsonFieldCandidate{name: "value", index: []int{0}},
			want: 1,
		},
		{
			name: "tagged first",
			a:    jsonFieldCandidate{name: "value", index: []int{0}, tagged: true},
			b:    jsonFieldCandidate{name: "value", index: []int{0}},
			want: -1,
		},
		{
			name: "untagged after tagged",
			a:    jsonFieldCandidate{name: "value", index: []int{0}},
			b:    jsonFieldCandidate{name: "value", index: []int{0}, tagged: true},
			want: 1,
		},
		{
			name: "field index",
			a:    jsonFieldCandidate{name: "value", index: []int{0, 1}},
			b:    jsonFieldCandidate{name: "value", index: []int{0, 2}},
			want: -1,
		},
		{
			name: "identical candidates",
			a:    jsonFieldCandidate{name: "value", index: []int{0}, tagged: true},
			b:    jsonFieldCandidate{name: "value", index: []int{0}, tagged: true},
			want: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := compareJSONFieldCandidates(tc.a, tc.b); got != tc.want {
				t.Fatalf("candidate comparison = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestValidJSONFieldName covers the accepted punctuation set and invalid runes.
func TestValidJSONFieldName(t *testing.T) {
	for _, name := range []string{
		"value", "Value2", "éclair", "two words", "!#$%&()*+-./:;<=>?@[]^_{|}~",
	} {
		if !validJSONFieldName(name) {
			t.Errorf("validJSONFieldName(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"", `bad\name`, `bad"name`, "bad\nname"} {
		if validJSONFieldName(name) {
			t.Errorf("validJSONFieldName(%q) = true, want false", name)
		}
	}
}

// TestFilterExactJSONValueContainersAndOrder verifies one-pass rewriting keeps
// duplicate order and the standard decoder's map, slice, array and custom rules.
func TestJSONStructFieldsWithoutTags(t *testing.T) {
	typ := reflect.TypeFor[CorrectnessNoTagEmbedding]()
	fields := jsonStructFieldsOf(typ)
	if len(fields) != 2 || fields["value"] != reflect.TypeFor[int]() ||
		fields["label"] != reflect.TypeFor[string]() {
		t.Fatalf("untagged promoted fields = %#v", fields)
	}
	value, err := decodeJSONExactStruct(typ, []byte(`{"value":7,"label":"ok","Value":99}`))
	if err != nil {
		t.Fatalf("decode untagged embedded struct: %v", err)
	}
	got := value.Interface().(CorrectnessNoTagEmbedding)
	if got.Value != 7 || got.Label != "ok" {
		t.Fatalf("untagged embedded struct = %#v", got)
	}
	if _, err := buildBindPlan(reflect.FuncOf([]reflect.Type{typ}, nil, false)); err != nil {
		t.Fatalf("buildBindPlan rejected untagged embedded struct: %v", err)
	}
}

func TestFilterExactJSONValueContainersAndOrder(t *testing.T) {
	input := []byte(` {"map":{"Key":{"Value":9,"value":1}},"slice":[{"Value":8,"value":2},null],"array":[{"value":3},{"Value":7,"value":4}],"custom":{"Value":5},"bytes":"YQ==","unknown":true} `)
	typ := reflect.TypeFor[CorrectnessJSONContainers]()
	if !jsonNeedsExactFilter(input, typ) {
		t.Fatal("container payload did not request exact filtering")
	}
	filtered := filterExactJSONValue(input, typ)
	want := `{"map":{"Key":{"value":1}},"slice":[{"value":2},null],"array":[{"value":3},{"value":4}],"custom":{"Value":5},"bytes":"YQ=="}`
	if string(filtered) != want {
		t.Fatalf("filtered containers = %s, want %s", filtered, want)
	}

	value, err := decodeJSONExactStruct(typ, filtered)
	if err != nil {
		t.Fatalf("decode filtered containers: %v", err)
	}
	got := value.Interface().(CorrectnessJSONContainers)
	if got.Map["Key"].Value != 1 || len(got.Slice) != 2 || got.Slice[0].Value != 2 || got.Slice[1].Value != 0 {
		t.Fatalf("decoded map/slice = %#v", got)
	}
	if got.Array != [2]CorrectnessJSONItem{{Value: 3}, {Value: 4}} {
		t.Fatalf("decoded array = %#v", got.Array)
	}
	if string(got.Custom.Seen["Value"]) != "5" || string(got.Bytes) != "a" {
		t.Fatalf("decoded custom/bytes = %#v", got)
	}

	duplicates := []byte(` {"value":"bad","value":1,"value":2} `)
	if got := string(filterExactJSONValue(duplicates, reflect.TypeFor[CorrectnessJSONItem]())); got != `{"value":"bad","value":1,"value":2}` {
		t.Fatalf("filtered duplicates = %s", got)
	}
	if _, err := decodeJSONExactStruct(reflect.TypeFor[CorrectnessJSONItem](), duplicates); err == nil {
		t.Fatal("earlier duplicate type error was lost")
	}

	escaped := []byte(`{"\u0076alue":1,"Value":9}`)
	if got := string(filterExactJSONValue(escaped, reflect.TypeFor[CorrectnessJSONItem]())); got != `{"\u0076alue":1}` {
		t.Fatalf("filtered escaped exact key = %s", got)
	}
	value, err = decodeJSONExactStruct(reflect.TypeFor[CorrectnessJSONItem](), escaped)
	if err != nil || value.Interface().(CorrectnessJSONItem).Value != 1 {
		t.Fatalf("decoded escaped exact key = (%v, %v)", value, err)
	}
}

// TestRecursivePointerAllocatesForLeafSources distinguishes a missing recursive
// pointer from one whose struct has a bindable leaf in any request source.
func TestRecursivePointerAllocatesForLeafSources(t *testing.T) {
	typ := reflect.TypeFor[*CorrectnessRecursiveNode]()
	for _, tc := range []struct {
		name string
		src  nameSources
		want int
	}{
		{name: "form leaf", src: nameSources{form: url.Values{"value": {"7"}}}, want: 7},
		{name: "path leaf", src: nameSources{path: map[string]string{"value": "8"}}, want: 8},
		{name: "no leaf"},
		{name: "recursive edge is not a leaf", src: nameSources{form: url.Values{"next": {"present"}}}},
		{name: "cycle is stopped", src: nameSources{form: url.Values{"value": {"9"}, "next": {"present"}}}, want: 9},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, err := bindFormParameter(typ, "node", &tc.src)
			if err != nil {
				t.Fatalf("bind recursive pointer: %v", err)
			}
			if tc.want == 0 {
				if !value.IsNil() {
					t.Fatalf("recursive pointer without a leaf = %#v, want nil", value.Interface())
				}
				return
			}
			if value.IsNil() {
				t.Fatal("recursive pointer with a leaf was not allocated")
			}
			got := value.Interface().(*CorrectnessRecursiveNode)
			if got.Value != tc.want || got.Next != nil {
				t.Fatalf("recursive pointer = %#v, want Value=%d and nil Next", got, tc.want)
			}
		})
	}

	header := &multipart.FileHeader{Filename: "leaf.txt"}
	fileValue, err := bindFormParameter(reflect.TypeFor[*CorrectnessRecursiveFileNode](), "node", &nameSources{
		files: filesMap{"upload": {header}},
	})
	if err != nil {
		t.Fatalf("bind recursive file pointer: %v", err)
	}
	if fileValue.IsNil() {
		t.Fatal("recursive pointer with a file leaf was not allocated")
	}
	gotFile := fileValue.Interface().(*CorrectnessRecursiveFileNode)
	if gotFile.Upload.Filename != "leaf.txt" || gotFile.Next != nil {
		t.Fatalf("recursive file pointer = %#v", gotFile)
	}

	missingFile, err := bindFormParameter(reflect.TypeFor[*CorrectnessRecursiveFileNode](), "node",
		&nameSources{})
	if err != nil || !missingFile.IsNil() {
		t.Fatalf("recursive file pointer without upload = (%v, %v), want nil", missingFile, err)
	}

	nested, err := bindFormParameter(reflect.TypeFor[*CorrectnessNestedRecursiveNode](), "node",
		&nameSources{form: url.Values{"value": {"11"}}})
	if err != nil {
		t.Fatalf("bind nested recursive pointer: %v", err)
	}
	if nested.IsNil() {
		t.Fatal("recursive pointer with a nested leaf was not allocated")
	}
	gotNested := nested.Interface().(*CorrectnessNestedRecursiveNode)
	if gotNested.Branch.Value != 11 || gotNested.Next != nil {
		t.Fatalf("nested recursive pointer = %#v, want Branch.Value=11 and nil Next", gotNested)
	}
}

// TestHasStructLeafSourceEdgeBranches directly checks file, pointer-depth,
// nested-success and nested-error paths in the recursive allocation probe.
func TestHasStructLeafSourceEdgeBranches(t *testing.T) {
	t.Run("missing file is not a leaf", func(t *testing.T) {
		present, err := hasStructLeafSource(reflect.TypeFor[CorrectnessRecursiveFileNode](),
			&nameSources{}, &formBindState{})
		if err != nil || present {
			t.Fatalf("missing file leaf = (%v, %v), want (false, nil)", present, err)
		}
	})

	t.Run("over-deep field pointer is skipped", func(t *testing.T) {
		deep := reflect.TypeFor[int]()
		for range maxTypePointerDepth + 1 {
			deep = reflect.PointerTo(deep)
		}
		typ := reflect.StructOf([]reflect.StructField{{Name: "Deep", Type: deep}})
		present, err := hasStructLeafSource(typ, &nameSources{}, &formBindState{})
		if err != nil || present {
			t.Fatalf("over-deep field leaf = (%v, %v), want (false, nil)", present, err)
		}
	})

	t.Run("nested leaf is present", func(t *testing.T) {
		src := &nameSources{form: url.Values{"value": {"12"}}}
		present, err := hasStructLeafSource(reflect.TypeFor[CorrectnessNestedRecursiveNode](),
			src, &formBindState{})
		if err != nil || !present {
			t.Fatalf("nested leaf = (%v, %v), want (true, nil)", present, err)
		}
	})

	t.Run("nested depth error propagates", func(t *testing.T) {
		src := &nameSources{form: url.Values{"value": {"13"}}}
		state := formBindState{size: maxTypePointerDepth - 1}
		present, err := hasStructLeafSource(reflect.TypeFor[CorrectnessNestedRecursiveNode](),
			src, &state)
		wantErr := "struct nesting exceeds " + strconv.Itoa(maxTypePointerDepth) + " levels"
		if present || err == nil || err.Error() != wantErr {
			t.Fatalf("nested depth error = (%v, %v), want (false, %q)", present, err, wantErr)
		}
		if state.size != maxTypePointerDepth-1 {
			t.Fatalf("state size after nested error = %d, want %d", state.size, maxTypePointerDepth-1)
		}
	})
}

// TestRecursivePointerPropagatesLeafDepthError checks the binder's error path
// when its allocation probe cannot push another struct onto the active branch.
func TestRecursivePointerPropagatesLeafDepthError(t *testing.T) {
	state := formBindState{size: maxTypePointerDepth}
	_, err := bindFormParameterDepth(reflect.TypeFor[*CorrectnessNestedRecursiveNode](), "node",
		&nameSources{form: url.Values{"value": {"14"}}}, 0, &state)
	wantErr := "struct nesting exceeds " + strconv.Itoa(maxTypePointerDepth) + " levels"
	if err == nil || err.Error() != wantErr {
		t.Fatalf("recursive leaf depth error = %v, want %q", err, wantErr)
	}
	if state.size != maxTypePointerDepth {
		t.Fatalf("state size after binder error = %d, want %d", state.size, maxTypePointerDepth)
	}
}

// correctnessDeepJSON builds equal-width nested objects with one wrong-case
// member per level, so both the input and rewrite result grow linearly.
func correctnessDeepJSON(depth int) ([]byte, []byte) {
	const prefix = `{"Value":0,"next":`
	body := make([]byte, 0, depth*len(prefix)+32)
	want := make([]byte, 0, depth*len(`{"next":`)+16)
	body = append(body, " \n"...)
	for range depth - 1 {
		body = append(body, prefix...)
		want = append(want, `{"next":`...)
	}
	body = append(body, `{"Value":0,"value":7}`...)
	want = append(want, `{"value":7}`...)
	for range depth - 1 {
		body = append(body, '}')
		want = append(want, '}')
	}
	body = append(body, '\n', ' ')
	return body, want
}

// TestFilterExactJSONValueDeep checks recursive filtering well beyond the
// pointer-type guard while retaining the exact leaf at the bottom.
func TestFilterExactJSONValueDeep(t *testing.T) {
	const depth = 256
	body, want := correctnessDeepJSON(depth)
	filtered := filterExactJSONValue(body, reflect.TypeFor[*CorrectnessDeepNode]())
	if string(filtered) != string(want) {
		index := 0
		for index < len(filtered) && index < len(want) && filtered[index] == want[index] {
			index++
		}
		t.Fatalf("deep filtered JSON differs at %d: got %d bytes, want %d", index, len(filtered), len(want))
	}
	value, err := decodeJSONExactStruct(reflect.TypeFor[*CorrectnessDeepNode](), filtered)
	if err != nil {
		t.Fatalf("decode deep filtered JSON: %v", err)
	}
	current := value.Interface().(*CorrectnessDeepNode)
	for level := 0; level < depth; level++ {
		if current == nil {
			t.Fatalf("deep JSON ended at level %d, want %d", level, depth)
		}
		if level == depth-1 {
			if current.Value != 7 || current.Next != nil {
				t.Fatalf("deep leaf = %#v", current)
			}
		} else if current.Value != 0 {
			t.Fatalf("deep level %d Value = %d, want 0", level, current.Value)
		}
		current = current.Next
	}
	if current != nil {
		t.Fatal("deep JSON has an extra level")
	}
}

// BenchmarkFilterExactJSONValueDeep reports rewrite allocations and throughput
// for increasing depths; allocation count should stay flat after the fix.
func BenchmarkFilterExactJSONValueDeep(b *testing.B) {
	typ := reflect.TypeFor[*CorrectnessDeepNode]()
	_ = jsonStructFieldsOf(reflect.TypeFor[CorrectnessDeepNode]())
	for _, depth := range []int{16, 64, 256, 1024} {
		b.Run(strconv.Itoa(depth), func(b *testing.B) {
			body, _ := correctnessDeepJSON(depth)
			b.ReportAllocs()
			b.SetBytes(int64(len(body)))
			b.ResetTimer()
			for b.Loop() {
				correctnessFilteredSink = filterExactJSONValue(body, typ)
			}
		})
	}
}
