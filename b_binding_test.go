// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"reflect"
	"runtime/debug"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type bindTestLogger struct{}

func (bindTestLogger) Info(string, ...any)  {}
func (bindTestLogger) Warn(string, ...any)  {}
func (bindTestLogger) Error(string, ...any) {}

type bindTestNamedInt int

type bindTestOctet byte

type bindTestNamedBytes []byte

type bindTestOctetSlice []bindTestOctet

type bindTestNested struct {
	Count   int `json:"count"`
	Default string
	Ignored string `json:"-"`
	Exact   string `json:"exact_name"`
	hidden  string
}

type bindTestFormStruct struct {
	Name      string
	ID        string `json:"id"`
	Nested    bindTestNested
	NestedPtr *bindTestNested
	Tags      []string
	Pair      [2]int
	Raw       []byte
	Optional  *int
}

type bindTestJSONNested struct {
	Value int `json:"value"`
}

type bindTestJSONStruct struct {
	Name     string             `json:"name"`
	Count    int                `json:"count"`
	Nested   bindTestJSONNested `json:"nested"`
	Tags     []string           `json:"tags"`
	Optional *bool              `json:"optional"`
}

type bindTestJSONNoTagNested struct {
	Value int
}

type bindTestJSONNoTag struct {
	Name     string
	Count    int
	Nested   bindTestJSONNoTagNested
	Tags     []string
	Optional *bool
}

type bindTestRecursiveNode struct {
	Value int                    `json:"value"`
	Next  *bindTestRecursiveNode `json:"next"`
}

type bindTestValidated struct {
	Name string
}

var (
	bindTestValidationError  = errors.New("name is required")
	bindTestValidationCalls  atomic.Int64
	bindTestRecursiveEnvName = "GBL_BIND_TEST_RECURSIVE_CHILD"
)

func (v *bindTestValidated) Validate() error {
	bindTestValidationCalls.Add(1)
	if v.Name == "" {
		return bindTestValidationError
	}
	return nil
}

// bindTestEngine installs a single already-planned route, avoiding global route
// registration while still exercising matching, dispatch, recovery and cleanup.
func bindTestEngine(t *testing.T, method, path string, fn any, names []string) *engine {
	t.Helper()
	e := newEngine()
	e.logger = bindTestLogger{}
	t.Cleanup(e.cancelStreams)

	m, ok := methodOf(method)
	if !ok {
		t.Fatalf("unsupported test method %q", method)
	}
	v := reflect.ValueOf(fn)
	if !v.IsValid() || v.Kind() != reflect.Func {
		t.Fatal("test route handler must be a function")
	}
	typ := v.Type()
	plan, err := buildBindPlan(typ)
	if err != nil {
		t.Fatalf("build bind plan: %v", err)
	}
	paramNames := normalizeParamNames(names, typ.NumIn())
	plan.prepareKeys(paramNames)
	rh := &routeHandler{
		v:            v,
		t:            typ,
		paramNames:   paramNames,
		plan:         plan,
		handlers:     []HandlerFunc{routeEndpoint},
		contentType:  textPlainCharset,
		streamCtx:    e.streamCtx,
		writeTimeout: e.cfg.WriteTimeout,
	}
	segments, normalized := e.dealPath(path)
	isStatic := true
	for _, segment := range segments {
		if segment.isVar {
			isStatic = false
			break
		}
	}
	if isStatic {
		e.staticRoutes[m] = map[string]*routeHandler{normalized: rh}
	} else {
		e.insertNode(segments, normalized, m, rh)
	}
	return e
}

func bindTestInject(t *testing.T, fn any, names []string, src nameSources, body []byte, jsonMedia bool) ([]reflect.Value, error) {
	t.Helper()
	typ := reflect.TypeOf(fn)
	plan, err := buildBindPlan(typ)
	if err != nil {
		t.Fatalf("build bind plan: %v", err)
	}
	paramNames := normalizeParamNames(names, typ.NumIn())
	plan.prepareKeys(paramNames)
	c := &Context{
		rh:     &routeHandler{plan: plan, paramNames: paramNames},
		params: src.path,
	}
	paramsBuf, params := acquireParams(typ.NumIn())
	defer releaseParams(paramsBuf, params)
	err = injectedParameters(c, src.form, src.files, body, jsonMedia, params)
	result := append([]reflect.Value(nil), params...)
	return result, err
}

func bindTestInterface(t *testing.T, value reflect.Value) any {
	t.Helper()
	if !value.IsValid() {
		t.Fatal("binding returned an invalid value")
	}
	return value.Interface()
}

func TestBindTextValues(t *testing.T) {
	tests := []struct {
		name   string
		typ    reflect.Type
		values []string
		want   any
	}{
		{name: "string first value", typ: reflect.TypeFor[string](), values: []string{"first", "second"}, want: "first"},
		{name: "bool", typ: reflect.TypeFor[bool](), values: []string{"true"}, want: true},
		{name: "int", typ: reflect.TypeFor[int](), values: []string{"-42"}, want: int(-42)},
		{name: "int8", typ: reflect.TypeFor[int8](), values: []string{"-8"}, want: int8(-8)},
		{name: "int16", typ: reflect.TypeFor[int16](), values: []string{"-16"}, want: int16(-16)},
		{name: "int32", typ: reflect.TypeFor[int32](), values: []string{"-32"}, want: int32(-32)},
		{name: "int64", typ: reflect.TypeFor[int64](), values: []string{"-64"}, want: int64(-64)},
		{name: "uint", typ: reflect.TypeFor[uint](), values: []string{"42"}, want: uint(42)},
		{name: "uint8", typ: reflect.TypeFor[uint8](), values: []string{"8"}, want: uint8(8)},
		{name: "uint16", typ: reflect.TypeFor[uint16](), values: []string{"16"}, want: uint16(16)},
		{name: "uint32", typ: reflect.TypeFor[uint32](), values: []string{"32"}, want: uint32(32)},
		{name: "uint64", typ: reflect.TypeFor[uint64](), values: []string{"64"}, want: uint64(64)},
		{name: "uintptr", typ: reflect.TypeFor[uintptr](), values: []string{"64"}, want: uintptr(64)},
		{name: "float32", typ: reflect.TypeFor[float32](), values: []string{"1.5"}, want: float32(1.5)},
		{name: "float64", typ: reflect.TypeFor[float64](), values: []string{"2.5"}, want: float64(2.5)},
		{name: "complex64", typ: reflect.TypeFor[complex64](), values: []string{"1+2i"}, want: complex64(1 + 2i)},
		{name: "complex128", typ: reflect.TypeFor[complex128](), values: []string{"3+4i"}, want: complex128(3 + 4i)},
		{name: "named integer", typ: reflect.TypeFor[bindTestNamedInt](), values: []string{"12"}, want: bindTestNamedInt(12)},
		{name: "missing scalar", typ: reflect.TypeFor[int](), want: int(0)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := bindTextValue(tt.typ, tt.values)
			if err != nil {
				t.Fatalf("bindTextValue() error = %v", err)
			}
			if value := bindTestInterface(t, got); !reflect.DeepEqual(value, tt.want) {
				t.Fatalf("bindTextValue() = %#v, want %#v", value, tt.want)
			}
		})
	}
}

func TestBindTextValueErrors(t *testing.T) {
	tests := []struct {
		name   string
		typ    reflect.Type
		values []string
	}{
		{name: "invalid bool", typ: reflect.TypeFor[bool](), values: []string{"maybe"}},
		{name: "int8 overflow", typ: reflect.TypeFor[int8](), values: []string{"128"}},
		{name: "negative uint", typ: reflect.TypeFor[uint](), values: []string{"-1"}},
		{name: "invalid float", typ: reflect.TypeFor[float64](), values: []string{"1.2.3"}},
		{name: "invalid complex", typ: reflect.TypeFor[complex128](), values: []string{"1+xi"}},
		{name: "unsupported map", typ: reflect.TypeFor[map[string]int](), values: []string{"x"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := bindTextValue(tt.typ, tt.values)
			if err == nil {
				t.Fatal("bindTextValue() succeeded, want an error")
			}
			if tt.typ.Kind() != reflect.Map {
				var numberError *strconv.NumError
				if !errors.As(err, &numberError) {
					t.Fatalf("error = %T %v, want *strconv.NumError", err, err)
				}
			}
		})
	}
}

func TestBindTextPointers(t *testing.T) {
	got, err := bindTextValue(reflect.TypeFor[**int](), []string{"23"})
	if err != nil {
		t.Fatalf("bind pointer chain: %v", err)
	}
	value := bindTestInterface(t, got).(**int)
	if value == nil || *value == nil || **value != 23 {
		t.Fatalf("bound pointer chain = %#v, want **int(23)", value)
	}

	missing, err := bindTextValue(reflect.TypeFor[*int](), nil)
	if err != nil {
		t.Fatalf("bind missing pointer: %v", err)
	}
	if !missing.IsNil() {
		t.Fatalf("missing pointer = %v, want nil", missing.Interface())
	}
}

func TestBindFormCollections(t *testing.T) {
	src := &nameSources{form: url.Values{
		"pair":    {"1", "2"},
		"numbers": {"3", "4", "5"},
		"names":   {"a", "b"},
		"raw":     {"hello"},
	}}
	tests := []struct {
		name string
		typ  reflect.Type
		key  string
		want any
	}{
		{name: "array", typ: reflect.TypeFor[[2]int](), key: "pair", want: [2]int{1, 2}},
		{name: "slice", typ: reflect.TypeFor[[]int](), key: "numbers", want: []int{3, 4, 5}},
		{name: "string slice", typ: reflect.TypeFor[[]string](), key: "names", want: []string{"a", "b"}},
		{name: "byte slice", typ: reflect.TypeFor[[]byte](), key: "raw", want: []byte("hello")},
		{name: "missing array", typ: reflect.TypeFor[[2]int](), key: "absent", want: [2]int{}},
		{name: "missing bytes", typ: reflect.TypeFor[[]byte](), key: "absent", want: []byte(nil)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := bindFormParameter(tt.typ, tt.key, src)
			if err != nil {
				t.Fatalf("bindFormParameter() error = %v", err)
			}
			if value := bindTestInterface(t, got); !reflect.DeepEqual(value, tt.want) {
				t.Fatalf("bindFormParameter() = %#v, want %#v", value, tt.want)
			}
		})
	}

	_, err := bindFormParameter(reflect.TypeFor[[2]int](), "numbers", src)
	if err == nil || !strings.Contains(err.Error(), "array requires 2 values, got 3") {
		t.Fatalf("array length error = %v", err)
	}
	_, err = bindFormParameter(reflect.TypeFor[[]int](), "pair", &nameSources{form: url.Values{"pair": {"1", "bad"}}})
	if err == nil || !strings.Contains(err.Error(), "slice index 1") {
		t.Fatalf("slice element error = %v", err)
	}
}

func TestBindNamedByteSlice(t *testing.T) {
	tests := []struct {
		name string
		typ  reflect.Type
		want any
	}{
		{name: "named slice", typ: reflect.TypeFor[bindTestNamedBytes](), want: bindTestNamedBytes("octets")},
		{name: "named byte element", typ: reflect.TypeFor[[]bindTestOctet](), want: []bindTestOctet("octets")},
		{name: "named slice and element", typ: reflect.TypeFor[bindTestOctetSlice](), want: bindTestOctetSlice("octets")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got reflect.Value
			var err error
			func() {
				defer func() {
					if recovered := recover(); recovered != nil {
						err = fmt.Errorf("bindFormParameter panicked: %v", recovered)
					}
				}()
				got, err = bindFormParameter(tt.typ, "raw", &nameSources{
					form: url.Values{"raw": {"octets"}},
				})
			}()
			if err != nil {
				t.Fatalf("named byte slice must bind raw bytes without panicking: %v", err)
			}
			if value := bindTestInterface(t, got); !reflect.DeepEqual(value, tt.want) {
				t.Fatalf("named byte slice = %#v, want %#v", value, tt.want)
			}
		})
	}
}

func TestBindStructNestingTagsAndExactCase(t *testing.T) {
	src := &nameSources{form: url.Values{
		"name":         {"alice"},
		"Name":         {"wrong-name"},
		"id":           {"lower-id"},
		"ID":           {"wrong-id"},
		"count":        {"7"},
		"Count":        {"99"},
		"default":      {"default-value"},
		"exact_name":   {"exact-value"},
		"Exact_Name":   {"wrong-exact"},
		"ignored":      {"must-not-bind"},
		"hidden":       {"must-not-bind"},
		"nested.count": {"must-not-bind"},
		"tags":         {"go", "http"},
		"pair":         {"8", "9"},
		"raw":          {"bytes"},
		"optional":     {"11"},
	}}
	got, err := bindStruct(reflect.TypeFor[bindTestFormStruct](), src)
	if err != nil {
		t.Fatalf("bindStruct() error = %v", err)
	}
	nested := bindTestNested{
		Count:   7,
		Default: "default-value",
		Exact:   "exact-value",
	}
	want := bindTestFormStruct{
		Name:      "alice",
		ID:        "lower-id",
		Nested:    nested,
		NestedPtr: bindTestPointer(nested),
		Tags:      []string{"go", "http"},
		Pair:      [2]int{8, 9},
		Raw:       []byte("bytes"),
		Optional:  bindTestPointer(11),
	}
	if value := bindTestInterface(t, got); !reflect.DeepEqual(value, want) {
		t.Fatalf("bound struct = %#v, want %#v", value, want)
	}
}

func bindTestPointer[T any](value T) *T {
	return &value
}

func TestBindStructRejectsWrongCaseKeys(t *testing.T) {
	got, err := bindStruct(reflect.TypeFor[bindTestFormStruct](), &nameSources{form: url.Values{
		"Name":       {"alice"},
		"ID":         {"id"},
		"Count":      {"4"},
		"Exact_Name": {"value"},
	}})
	if err != nil {
		t.Fatalf("bindStruct() error = %v", err)
	}
	value := bindTestInterface(t, got).(bindTestFormStruct)
	if value.Name != "" || value.ID != "" || value.Nested.Count != 0 || value.Nested.Exact != "" {
		t.Fatalf("wrong-case keys bound unexpectedly: %#v", value)
	}
}

func TestValidateFormPointerExpansionBoundsTraversal(t *testing.T) {
	old := maxFormPointerExpansionSteps
	maxFormPointerExpansionSteps = 1
	t.Cleanup(func() { maxFormPointerExpansionSteps = old })
	if err := validateFormPointerExpansion(reflect.TypeFor[bindTestNested]()); err == nil {
		t.Fatal("pointer expansion traversal exceeded its step budget without an error")
	}
}

func TestValidateFormPointerExpansionRejectsWideAcyclicTree(t *testing.T) {
	leaf := reflect.TypeFor[struct{}]()
	for range 16 {
		leaf = reflect.StructOf([]reflect.StructField{
			{Name: "Left", Type: reflect.PointerTo(leaf)},
			{Name: "Right", Type: reflect.PointerTo(leaf)},
		})
	}
	if err := validateFormPointerExpansion(leaf); err == nil {
		t.Fatal("wide acyclic pointer tree was accepted")
	}
	if err := validateFormPointerExpansion(reflect.TypeFor[bindTestNested]()); err != nil {
		t.Fatalf("ordinary struct pointer expansion: %v", err)
	}
}

func TestBindStructPointers(t *testing.T) {
	present, err := bindFormParameter(reflect.TypeFor[*bindTestNested](), "count", &nameSources{
		form: url.Values{"count": {"5"}},
	})
	if err != nil {
		t.Fatalf("bind nested pointer: %v", err)
	}
	presentValue := bindTestInterface(t, present).(*bindTestNested)
	if presentValue == nil || presentValue.Count != 5 {
		t.Fatalf("nested pointer = %#v, want Count=5", presentValue)
	}

	missingStruct, err := bindFormParameter(reflect.TypeFor[*bindTestNested](), "absent", &nameSources{})
	if err != nil {
		t.Fatalf("bind missing struct pointer: %v", err)
	}
	if missingStruct.IsNil() {
		t.Fatal("missing pointer-to-struct must be allocated so its zero fields can be validated")
	}
	if value := bindTestInterface(t, missingStruct).(*bindTestNested); !reflect.DeepEqual(value, &bindTestNested{}) {
		t.Fatalf("missing struct pointer = %#v, want zero struct", value)
	}

	missingScalar, err := bindFormParameter(reflect.TypeFor[*int](), "absent", &nameSources{})
	if err != nil {
		t.Fatalf("bind missing scalar pointer: %v", err)
	}
	if !missingScalar.IsNil() {
		t.Fatalf("missing scalar pointer = %v, want nil", missingScalar.Interface())
	}
}

func TestBindPathPrecedence(t *testing.T) {
	src := nameSources{
		path: map[string]string{"id": "7", "name": "path-name"},
		form: url.Values{"id": {"9"}, "name": {"form-name"}},
	}
	values, err := bindTestInject(t, func(id int) {}, []string{"id"}, src, []byte(`{"id":11}`), true)
	if err != nil {
		t.Fatalf("inject path scalar: %v", err)
	}
	if got := bindTestInterface(t, values[0]); got != 7 {
		t.Fatalf("path scalar = %v, want 7", got)
	}

	values, err = bindTestInject(t, func(payload bindTestJSONStruct) {}, []string{"payload"}, src, nil, false)
	if err != nil {
		t.Fatalf("inject path struct field: %v", err)
	}
	if got := bindTestInterface(t, values[0]).(bindTestJSONStruct).Name; got != "path-name" {
		t.Fatalf("path struct field = %q, want path-name", got)
	}

	_, err = bindTestInject(t, func(id int) {}, []string{"id"}, nameSources{
		path: map[string]string{"id": "not-an-int"},
		form: url.Values{"id": {"9"}},
	}, nil, false)
	if err == nil || !strings.Contains(err.Error(), `path parameter "id"`) {
		t.Fatalf("malformed path value must fail instead of falling back to form: %v", err)
	}
}

func TestBindJSONSingleStruct(t *testing.T) {
	body := []byte(`{"name":"alice","count":3,"nested":{"value":4},"tags":["go","http"],"optional":true}`)
	values, err := bindTestInject(t, func(payload bindTestJSONStruct) {}, []string{"payload"}, nameSources{
		form: url.Values{"name": {"form-name"}, "count": {"99"}},
	}, body, true)
	if err != nil {
		t.Fatalf("inject JSON struct: %v", err)
	}
	want := bindTestJSONStruct{
		Name:     "alice",
		Count:    3,
		Nested:   bindTestJSONNested{Value: 4},
		Tags:     []string{"go", "http"},
		Optional: bindTestPointer(true),
	}
	if got := bindTestInterface(t, values[0]); !reflect.DeepEqual(got, want) {
		t.Fatalf("JSON struct = %#v, want %#v", got, want)
	}

	values, err = bindTestInject(t, func(payload *bindTestJSONStruct) {}, []string{"payload"}, nameSources{}, body, true)
	if err != nil {
		t.Fatalf("inject JSON struct pointer: %v", err)
	}
	if got := bindTestInterface(t, values[0]); !reflect.DeepEqual(got, bindTestPointer(want)) {
		t.Fatalf("JSON struct pointer = %#v, want %#v", got, bindTestPointer(want))
	}
}

func TestBindJSONWithoutTags(t *testing.T) {
	body := []byte(`{"name":"alice","count":3,"nested":{"value":4},"tags":["go","http"],"optional":true}`)
	values, err := bindTestInject(t, func(payload bindTestJSONNoTag) {}, []string{"payload"}, nameSources{}, body, true)
	if err != nil {
		t.Fatalf("inject untagged JSON struct: %v", err)
	}
	want := bindTestJSONNoTag{
		Name:     "alice",
		Count:    3,
		Nested:   bindTestJSONNoTagNested{Value: 4},
		Tags:     []string{"go", "http"},
		Optional: bindTestPointer(true),
	}
	if got := bindTestInterface(t, values[0]); !reflect.DeepEqual(got, want) {
		t.Fatalf("untagged JSON struct = %#v, want %#v", got, want)
	}

	values, err = bindTestInject(t, func(payload *bindTestJSONNoTag) {}, []string{"payload"}, nameSources{}, body, true)
	if err != nil {
		t.Fatalf("inject untagged JSON struct pointer: %v", err)
	}
	if got := bindTestInterface(t, values[0]); !reflect.DeepEqual(got, bindTestPointer(want)) {
		t.Fatalf("untagged JSON struct pointer = %#v, want %#v", got, bindTestPointer(want))
	}

	values, err = bindTestInject(t, func(payload bindTestJSONNoTag) {}, []string{"payload"}, nameSources{},
		[]byte(`{"Name":"alice","Count":3,"Nested":{"Value":4}}`), true)
	if err != nil {
		t.Fatalf("inject wrong-case untagged JSON struct: %v", err)
	}
	if got := bindTestInterface(t, values[0]); !reflect.DeepEqual(got, bindTestJSONNoTag{}) {
		t.Fatalf("wrong-case untagged JSON struct = %#v, want zero value", got)
	}

	values, err = bindTestInject(t,
		func(first bindTestJSONNoTagNested, second bindTestJSONNoTagNested) {},
		[]string{"first", "second"}, nameSources{},
		[]byte(`{"first":{"value":1},"second":{"value":2}}`), true)
	if err != nil {
		t.Fatalf("inject multiple untagged JSON structs: %v", err)
	}
	if got := bindTestInterface(t, values[0]); got != (bindTestJSONNoTagNested{Value: 1}) {
		t.Fatalf("first untagged JSON struct = %#v", got)
	}
	if got := bindTestInterface(t, values[1]); got != (bindTestJSONNoTagNested{Value: 2}) {
		t.Fatalf("second untagged JSON struct = %#v", got)
	}
}

func TestBindJSONSingleStructUsesExactFieldCase(t *testing.T) {
	body := []byte(`{"Name":"alice","Count":3,"NESTED":{"Value":4},"TAGS":["go"]}`)
	values, err := bindTestInject(t, func(payload bindTestJSONStruct) {}, []string{"payload"}, nameSources{}, body, true)
	if err != nil {
		t.Fatalf("inject JSON struct: %v", err)
	}
	got := bindTestInterface(t, values[0]).(bindTestJSONStruct)
	if !reflect.DeepEqual(got, bindTestJSONStruct{}) {
		t.Fatalf("JSON struct field names must match exact bind names; got %#v, want zero value", got)
	}
}

func TestBindJSONMultipleParametersUseExactStructFieldCase(t *testing.T) {
	body := []byte(`{"payload":{"Name":"alice","Count":3},"id":1}`)
	values, err := bindTestInject(t, func(payload bindTestJSONStruct, id int) {},
		[]string{"payload", "id"}, nameSources{}, body, true)
	if err != nil {
		t.Fatalf("inject multi-parameter JSON struct: %v", err)
	}
	if got := bindTestInterface(t, values[0]); !reflect.DeepEqual(got, bindTestJSONStruct{}) {
		t.Fatalf("multi-parameter JSON struct = %#v, want exact-case zero value", got)
	}
}

func TestBindJSONMultipleParameters(t *testing.T) {
	body := []byte(`{"id":42,"name":"alice","enabled":true,"payload":{"name":"nested","count":5},"unknown":"ignored"}`)
	values, err := bindTestInject(t, func(id int, name string, enabled *bool, payload bindTestJSONStruct, missing int) {},
		[]string{"id", "name", "enabled", "payload", "missing"}, nameSources{}, body, true)
	if err != nil {
		t.Fatalf("inject JSON parameters: %v", err)
	}
	want := []any{
		42,
		"alice",
		bindTestPointer(true),
		bindTestJSONStruct{Name: "nested", Count: 5},
		0,
	}
	for i, expected := range want {
		if got := bindTestInterface(t, values[i]); !reflect.DeepEqual(got, expected) {
			t.Errorf("parameter %d = %#v, want %#v", i, got, expected)
		}
	}
}

func TestBindJSONMultipleParameterErrors(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "invalid JSON", body: `{"id":`},
		{name: "array is not an object", body: `[]`},
		{name: "null is not an object", body: `null`},
		{name: "wrong member type", body: `{"id":"not-an-int"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := bindTestInject(t, func(id int, name string) {}, []string{"id", "name"}, nameSources{}, []byte(tt.body), true)
			if err == nil {
				t.Fatal("injectedParameters() succeeded, want an error")
			}
			if tt.name == "null is not an object" && !errors.Is(err, multipleRequireError) {
				t.Fatalf("null error = %v, want multipleRequireError", err)
			}
			if tt.name == "wrong member type" {
				var typeError *json.UnmarshalTypeError
				if !errors.As(err, &typeError) || !strings.Contains(err.Error(), `JSON parameter "id"`) {
					t.Fatalf("type error = %T %v, want wrapped JSON parameter error", err, err)
				}
			}
		})
	}
}

func TestBindJSONDuplicateAndEscapedKeys(t *testing.T) {
	body := []byte(`{"i\u0064":1,"id":2,"name":"a\"b"}`)
	values, err := bindTestInject(t, func(id int, name string) {}, []string{"id", "name"}, nameSources{}, body, true)
	if err != nil {
		t.Fatalf("inject escaped JSON keys: %v", err)
	}
	if got := bindTestInterface(t, values[0]); got != 2 {
		t.Fatalf("duplicate id = %v, want last value 2", got)
	}
	if got := bindTestInterface(t, values[1]); got != `a"b` {
		t.Fatalf("escaped name = %q, want %q", got, `a"b`)
	}
}

func TestBindMissingJSONFieldsValidateZeroValues(t *testing.T) {
	t.Run("single struct", func(t *testing.T) {
		bindTestValidationCalls.Store(0)
		called := false
		e := bindTestEngine(t, http.MethodPost, "/validate", func(value bindTestValidated) string {
			called = true
			return value.Name
		}, nil)
		r := httptest.NewRequest(http.MethodPost, "/validate", strings.NewReader(`{}`))
		r.Header.Set(contentType, applicationJson)
		recorder := httptest.NewRecorder()
		e.ServeHTTP(recorder, r)
		if recorder.Code != http.StatusBadRequest || recorder.Body.String() != bindTestValidationError.Error() {
			t.Fatalf("response = %d %q, want 400 %q", recorder.Code, recorder.Body.String(), bindTestValidationError)
		}
		if called {
			t.Fatal("handler ran after validation failed")
		}
		if calls := bindTestValidationCalls.Load(); calls != 1 {
			t.Fatalf("Validate calls = %d, want 1", calls)
		}
	})

	t.Run("multiple parameters", func(t *testing.T) {
		bindTestValidationCalls.Store(0)
		called := false
		e := bindTestEngine(t, http.MethodPost, "/validate-multi", func(value bindTestValidated, id int) string {
			called = true
			return value.Name
		}, []string{"value", "id"})
		r := httptest.NewRequest(http.MethodPost, "/validate-multi", strings.NewReader(`{"id":7}`))
		r.Header.Set(contentType, applicationJson)
		recorder := httptest.NewRecorder()
		e.ServeHTTP(recorder, r)
		if recorder.Code != http.StatusBadRequest || recorder.Body.String() != bindTestValidationError.Error() {
			t.Fatalf("response = %d %q, want 400 %q", recorder.Code, recorder.Body.String(), bindTestValidationError)
		}
		if called {
			t.Fatal("handler ran after validation failed")
		}
		if calls := bindTestValidationCalls.Load(); calls != 1 {
			t.Fatalf("Validate calls = %d, want 1", calls)
		}
	})
}

func TestBindNilPointerSkipsValidation(t *testing.T) {
	bindTestValidationCalls.Store(0)
	called := false
	e := bindTestEngine(t, http.MethodPost, "/nil", func(value *bindTestValidated, id int) string {
		called = true
		if value != nil {
			t.Fatalf("missing JSON member produced %#v, want nil", value)
		}
		return "nil"
	}, []string{"value", "id"})
	r := httptest.NewRequest(http.MethodPost, "/nil", strings.NewReader(`{"id":7}`))
	r.Header.Set(contentType, applicationJson)
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, r)
	if recorder.Code != http.StatusOK || recorder.Body.String() != "nil" || !called {
		t.Fatalf("response = %d %q, called=%v; want 200 nil, called=true", recorder.Code, recorder.Body.String(), called)
	}
	if calls := bindTestValidationCalls.Load(); calls != 0 {
		t.Fatalf("Validate calls = %d, want 0 for nil pointer", calls)
	}
}

func TestBindValidationRunsAfterPresentValues(t *testing.T) {
	bindTestValidationCalls.Store(0)
	e := bindTestEngine(t, http.MethodPost, "/validate", func(value bindTestValidated) string {
		return value.Name
	}, nil)
	r := httptest.NewRequest(http.MethodPost, "/validate", strings.NewReader("name=alice"))
	r.Header.Set(contentType, xWwwFormUrlencoded)
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, r)
	if recorder.Code != http.StatusOK || recorder.Body.String() != "alice" {
		t.Fatalf("response = %d %q, want 200 alice", recorder.Code, recorder.Body.String())
	}
	if calls := bindTestValidationCalls.Load(); calls != 1 {
		t.Fatalf("Validate calls = %d, want 1", calls)
	}
}

func TestBindFileHeaderShapes(t *testing.T) {
	headers := []*multipart.FileHeader{
		{Filename: "first.txt", Size: 3},
		{Filename: "second.txt", Size: 4},
	}
	tests := []struct {
		name string
		typ  reflect.Type
		want any
	}{
		{name: "value takes first", typ: reflect.TypeFor[multipart.FileHeader](), want: *headers[0]},
		{name: "pointer takes first", typ: reflect.TypeFor[*multipart.FileHeader](), want: headers[0]},
		{name: "pointer slice preserves order", typ: reflect.TypeFor[[]*multipart.FileHeader](), want: headers},
		{name: "value slice preserves order", typ: reflect.TypeFor[[]multipart.FileHeader](), want: []multipart.FileHeader{*headers[0], *headers[1]}},
		{name: "missing value", typ: reflect.TypeFor[multipart.FileHeader](), want: multipart.FileHeader{}},
		{name: "missing pointer", typ: reflect.TypeFor[*multipart.FileHeader](), want: (*multipart.FileHeader)(nil)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source := headers
			if strings.HasPrefix(tt.name, "missing") {
				source = nil
			}
			got, err := bindFileHeaders(tt.typ, source)
			if err != nil {
				t.Fatalf("bindFileHeaders() error = %v", err)
			}
			if value := bindTestInterface(t, got); !reflect.DeepEqual(value, tt.want) {
				t.Fatalf("bindFileHeaders() = %#v, want %#v", value, tt.want)
			}
		})
	}
}

func TestBindRecursiveStructForm(t *testing.T) {
	if os.Getenv(bindTestRecursiveEnvName) != "1" {
		executable, err := os.Executable()
		if err != nil {
			t.Fatalf("find test executable: %v", err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, executable, "-test.run=^TestBindRecursiveStructFormSubprocess$", "-test.count=1", "-test.timeout=15s")
		cmd.Env = append(os.Environ(), bindTestRecursiveEnvName+"=1")
		output, runErr := cmd.CombinedOutput()
		if runErr != nil {
			var diagnostics []string
			for _, line := range strings.Split(string(output), "\n") {
				if strings.Contains(line, "stack overflow") || strings.Contains(line, "TestBindRecursiveStructFormSubprocess") {
					diagnostics = append(diagnostics, line)
				}
			}
			if len(diagnostics) > 4 {
				diagnostics = diagnostics[:4]
			}
			if len(diagnostics) == 0 {
				diagnostics = append(diagnostics, "child exited without a recognizable stack-overflow diagnostic")
			}
			t.Fatalf("recursive struct form subprocess failed (stack overflow must be fixed, not ignored): %v\nchild diagnostics:\n%s", runErr, strings.Join(diagnostics, "\n"))
		}
		return
	}
	t.Fatal("parent-only recursive struct test ran in the child process")
}

func TestBindRecursiveStructFormSubprocess(t *testing.T) {
	if os.Getenv(bindTestRecursiveEnvName) != "1" {
		t.Skip("subprocess helper; run by TestBindRecursiveStructForm")
	}
	// Keep a regression failure fast and bounded. A fatal runtime stack overflow
	// cannot be recovered, which is why this code runs only in the child process.
	debug.SetMaxStack(8 << 20)
	got, err := bindFormParameter(reflect.TypeFor[bindTestRecursiveNode](), "node", &nameSources{})
	if err != nil {
		t.Fatalf("missing recursive struct must bind to its zero value: %v", err)
	}
	want := reflect.Zero(reflect.TypeFor[bindTestRecursiveNode]()).Interface()
	if value := bindTestInterface(t, got); !reflect.DeepEqual(value, want) {
		t.Fatalf("missing recursive struct = %#v, want %#v", value, want)
	}
}
