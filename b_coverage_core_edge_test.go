// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

type coreEdgeQueryCycle struct {
	Value string
	Next  *coreEdgeQueryCycle
}

type coreEdgeRecursiveA struct {
	B *coreEdgeRecursiveB
}

type coreEdgeRecursiveB struct {
	A *coreEdgeRecursiveA
}

type coreEdgePlainStruct struct {
	Value string
}

type coreEdgeMethodReceiver struct{}

func (coreEdgeMethodReceiver) Method() {}

func coreEdgePlainFunction(int) {}

type coreEdgeNarrowWriter interface {
	CoreEdgeNarrowWriter()
}

func coreEdgePointerType(depth int) reflect.Type {
	t := reflect.TypeFor[int]()
	for range depth {
		t = reflect.PointerTo(t)
	}
	return t
}

func TestCoreEdgeAcquireParamsGrowsAndClears(t *testing.T) {
	originalNew := paramsPool.New
	t.Cleanup(func() {
		paramsPool = sync.Pool{New: originalNew}
	})

	paramsPool = sync.Pool{New: func() any {
		params := make([]reflect.Value, 1, 1)
		params[0] = reflect.ValueOf("stale")
		return &params
	}}

	buf, params := acquireParams(16)
	if len(params) != 16 || cap(params) < 16 {
		t.Fatalf("acquireParams(16) length/capacity = %d/%d, want 16/at least 16", len(params), cap(params))
	}
	for i, value := range params {
		if value.IsValid() {
			t.Fatalf("params[%d] retained a value: %v", i, value)
		}
	}
	for i := range params {
		params[i] = reflect.ValueOf(i)
	}
	releaseParams(buf, params)
	if len(*buf) != len(params) {
		t.Fatalf("released slice length = %d, want %d", len(*buf), len(params))
	}
	for i, value := range *buf {
		if value.IsValid() {
			t.Fatalf("released params[%d] retained a value: %v", i, value)
		}
	}

	const pooledCapacity = 4
	const requestSize = 2
	paramsPool = sync.Pool{New: func() any {
		params := make([]reflect.Value, pooledCapacity, pooledCapacity)
		for i := range params {
			params[i] = reflect.ValueOf("stale")
		}
		return &params
	}}

	buf, params = acquireParams(requestSize)
	if len(params) != requestSize || cap(params) != pooledCapacity {
		t.Fatalf("acquireParams(%d) length/capacity = %d/%d, want %d/%d",
			requestSize, len(params), cap(params), requestSize, pooledCapacity)
	}
	for i, value := range params {
		if value.IsValid() {
			t.Fatalf("acquired params[%d] retained a value: %v", i, value)
		}
	}
	for i := range params {
		params[i] = reflect.ValueOf(i)
	}
	releaseParams(buf, params)
	if len(*buf) != len(params) {
		t.Fatalf("released slice length = %d, want %d", len(*buf), len(params))
	}
	for i, value := range *buf {
		if value.IsValid() {
			t.Fatalf("released backing[%d] retained a value: %v", i, value)
		}
	}
}

func TestCoreEdgeAppendStructQueryKeysVisitedAndDeref(t *testing.T) {
	keys := make(requestKeySet)
	visited := make(map[reflect.Type]bool)
	cycleType := reflect.TypeFor[coreEdgeQueryCycle]()
	appendStructQueryKeys(keys, cycleType, visited)
	appendStructQueryKeys(keys, cycleType, visited)
	if _, ok := keys["value"]; !ok || len(keys) != 1 {
		t.Fatalf("cycle query keys = %v, want only value", keys)
	}

	appendStructQueryKeys(keys, coreEdgePointerType(maxTypePointerDepth+1), visited)
	if len(keys) != 1 {
		t.Fatalf("over-deep type changed query keys: %v", keys)
	}

	deepFieldType := reflect.StructOf([]reflect.StructField{{
		Name: "Deep",
		Type: coreEdgePointerType(maxTypePointerDepth + 1),
	}})
	appendStructQueryKeys(keys, deepFieldType, visited)
	if _, ok := keys["deep"]; ok {
		t.Fatalf("over-deep field was added to query keys: %v", keys)
	}
}

func TestCoreEdgeIsRecursiveStructCachesResult(t *testing.T) {
	if isRecursiveStruct(reflect.TypeFor[int]()) {
		t.Fatal("non-struct type was reported as recursive")
	}
	if isRecursiveStruct(coreEdgePointerType(maxTypePointerDepth + 1)) {
		t.Fatal("over-deep pointer type was reported as recursive")
	}

	typ := reflect.TypeFor[coreEdgeRecursiveA]()
	recursiveStructCache.Delete(typ)
	if !isRecursiveStruct(typ) {
		t.Fatal("indirectly recursive struct was reported as non-recursive")
	}
	cached, ok := recursiveStructCache.Load(typ)
	if !ok || cached.(bool) != true {
		t.Fatalf("recursive cache entry = (%v, %t), want (true, true)", cached, ok)
	}
	if !isRecursiveStruct(typ) {
		t.Fatal("cached recursive result changed")
	}

	plainType := reflect.TypeFor[coreEdgePlainStruct]()
	recursiveStructCache.Delete(plainType)
	if isRecursiveStruct(plainType) {
		t.Fatal("plain struct was reported as recursive")
	}
	cached, ok = recursiveStructCache.Load(plainType)
	if !ok || cached.(bool) != false {
		t.Fatalf("non-recursive cache entry = (%v, %t), want (false, true)", cached, ok)
	}
	if isRecursiveStruct(plainType) {
		t.Fatal("cached non-recursive result changed")
	}
}

func TestCoreEdgeLowerFirstBoundaries(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{input: "", want: ""},
		{input: "Name", want: "name"},
		{input: "name", want: "name"},
		{input: "1Name", want: "1Name"},
		{input: "Éclair", want: "éclair"},
		{input: "ǅTEST", want: "ǆTEST"},
		{input: string([]byte{0xff, 'A'}), want: string([]byte{0xff, 'A'})},
	}
	for _, tt := range tests {
		if got := lowerFirst(tt.input); got != tt.want {
			t.Errorf("lowerFirst(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestCoreEdgeInjectedParametersSpecialAndDefensiveBranches(t *testing.T) {
	t.Run("request", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		plan := bindPlan{
			types:   []reflect.Type{requestType},
			classes: []paramClass{classRequest},
		}
		c := &Context{
			Request: request,
			rh:      &routeHandler{plan: plan, paramNames: []string{"request"}},
		}
		params := make([]reflect.Value, 1)
		if err := injectedParameters(c, nil, nil, nil, false, params); err != nil {
			t.Fatalf("injectedParameters() error = %v", err)
		}
		if got := params[0].Interface(); got != request {
			t.Fatalf("injected request = %p, want %p", got, request)
		}
	})

	t.Run("writer mismatch", func(t *testing.T) {
		plan := bindPlan{
			types:    []reflect.Type{writerType},
			classes:  []paramClass{classWriter},
			writerOK: []bool{false},
		}
		c := &Context{rh: &routeHandler{plan: plan, paramNames: []string{"writer"}}}
		err := injectedParameters(c, nil, nil, nil, false, make([]reflect.Value, 1))
		if !errors.Is(err, responseMismatch) {
			t.Fatalf("injectedParameters() error = %v, want responseMismatch", err)
		}
	})

	t.Run("SSE", func(t *testing.T) {
		stream := &sseWriter{}
		plan := bindPlan{
			types:   []reflect.Type{sseWriterType},
			classes: []paramClass{classSSE},
		}
		c := &Context{sse: stream, rh: &routeHandler{plan: plan, paramNames: []string{"stream"}}}
		params := make([]reflect.Value, 1)
		if err := injectedParameters(c, nil, nil, nil, false, params); err != nil {
			t.Fatalf("injectedParameters() error = %v", err)
		}
		if got := params[0].Interface(); got != stream {
			t.Fatalf("injected SSE writer = %p, want %p", got, stream)
		}
	})

	t.Run("files error", func(t *testing.T) {
		plan := bindPlan{
			types:   []reflect.Type{reflect.TypeFor[string]()},
			classes: []paramClass{classFiles},
		}
		c := &Context{rh: &routeHandler{plan: plan, paramNames: []string{"upload"}}}
		err := injectedParameters(c, nil, nil, nil, false, make([]reflect.Value, 1))
		if err == nil || !strings.Contains(err.Error(), "unsupported file parameter type") {
			t.Fatalf("injectedParameters() error = %v, want unsupported file type", err)
		}
	})

}

func TestCoreEdgeBindFormParameterDepthAndMissingScalarPointer(t *testing.T) {
	src := &nameSources{}
	_, err := bindFormParameterDepth(reflect.TypeFor[*int](), "value", src,
		maxTypePointerDepth, &formBindState{})
	if err == nil || !strings.Contains(err.Error(), "recursive pointer type") {
		t.Fatalf("bindFormParameterDepth() error = %v, want pointer depth error", err)
	}

	got, err := bindFormParameterDepth(reflect.TypeFor[*int](), "missing", src,
		0, &formBindState{})
	if err != nil {
		t.Fatalf("bindFormParameterDepth(missing) error = %v", err)
	}
	if !got.IsValid() || got.Type() != reflect.TypeFor[*int]() || !got.IsNil() {
		t.Fatalf("missing scalar pointer = %v, want a valid nil *int", got)
	}

	fileSrc := &nameSources{files: filesMap{"upload": nil}}
	fileValue, err := bindFormParameterDepth(fileHeaderPtr, "upload", fileSrc,
		0, &formBindState{})
	if err != nil {
		t.Fatalf("bindFormParameterDepth(file) error = %v", err)
	}
	if !fileValue.IsValid() || fileValue.Type() != fileHeaderPtr || !fileValue.IsNil() {
		t.Fatalf("missing file pointer = %v, want a valid nil *multipart.FileHeader", fileValue)
	}
}

func TestCoreEdgeBindFileHeadersDefault(t *testing.T) {
	_, err := bindFileHeaders(reflect.TypeFor[string](), nil)
	if err == nil || !strings.Contains(err.Error(), "unsupported file parameter type") {
		t.Fatalf("bindFileHeaders() error = %v, want unsupported file type", err)
	}
}

func TestCoreEdgeBindTextValueDepthAndEmptyValues(t *testing.T) {
	_, err := bindTextValueDepth(reflect.TypeFor[**int](), []string{"1"},
		maxTypePointerDepth-1)
	if err == nil || !strings.Contains(err.Error(), "recursive pointer type") {
		t.Fatalf("bindTextValueDepth() error = %v, want pointer depth error", err)
	}

	tests := []struct {
		name   string
		typ    reflect.Type
		values []string
		want   any
	}{
		{name: "missing scalar", typ: reflect.TypeFor[int](), want: int(0)},
		{name: "missing scalar pointer", typ: reflect.TypeFor[*int](), want: (*int)(nil)},
		{name: "empty string", typ: reflect.TypeFor[string](), values: []string{""}, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := bindTextValueDepth(tt.typ, tt.values, 0)
			if err != nil {
				t.Fatalf("bindTextValueDepth() error = %v", err)
			}
			if value := got.Interface(); !reflect.DeepEqual(value, tt.want) {
				t.Fatalf("bindTextValueDepth() = %#v, want %#v", value, tt.want)
			}
		})
	}
}

func TestCoreEdgeHasNonWhitespaceUsesJSONWhitespaceOnly(t *testing.T) {
	if hasNonWhitespace([]byte(" \t\r\n")) {
		t.Fatal("JSON whitespace was reported as non-whitespace")
	}
	if !hasNonWhitespace([]byte("\u00a0")) {
		t.Fatal("Unicode whitespace was treated as JSON whitespace")
	}
}

func TestCoreEdgeEqualMethodNameRejectsDifferentLengths(t *testing.T) {
	if equalMethodName("GET", "GETS") {
		t.Fatal("equalMethodName accepted strings with different lengths")
	}
}

func TestCoreEdgeIsMethodExpressionBranches(t *testing.T) {
	var nilFunction func(int)
	if isMethodExpression(reflect.ValueOf(nilFunction), reflect.TypeOf(nilFunction)) {
		t.Fatal("nil function was reported as a method expression")
	}
	if isMethodExpressionName("len", reflect.TypeFor[int]()) {
		t.Fatal("unqualified builtin name was reported as a method expression")
	}
	if isMethodExpression(reflect.ValueOf(coreEdgePlainFunction), reflect.TypeOf(coreEdgePlainFunction)) {
		t.Fatal("ordinary package function was reported as a method expression")
	}
	if isMethodExpression(reflect.ValueOf(coreEdgeNoArg), reflect.TypeOf(coreEdgeNoArg)) {
		t.Fatal("zero-argument function was reported as a method expression")
	}

	method := reflect.ValueOf(coreEdgeMethodReceiver.Method)
	if !isMethodExpression(method, method.Type()) {
		t.Fatal("method expression was not recognized")
	}
	deepReceiverType := reflect.FuncOf([]reflect.Type{
		coreEdgePointerType(maxTypePointerDepth + 1),
	}, nil, false)
	if isMethodExpression(method, deepReceiverType) {
		t.Fatal("method expression with an over-deep receiver was recognized")
	}
}

func coreEdgeNoArg() {}

func TestCoreEdgeParamNameLookupNilFunction(t *testing.T) {
	var nilFunction func(int)
	value := reflect.ValueOf(nilFunction)
	e := &engine{dwarfIndex: map[string]dwarfParamNames{
		"unused": {names: []string{"unused"}},
	}}
	names, resolved, ambiguous := e.lookupParamNamesDWARF(value)
	if names != nil || resolved || ambiguous != "" {
		t.Fatalf("lookupParamNamesDWARF(nil) = (%v, %t, %q), want (nil, false, empty)",
			names, resolved, ambiguous)
	}
}

func TestCoreEdgeFindOverlappingRouteNilAndVariableStatic(t *testing.T) {
	if found, path := findOverlappingRoute(nil, nil, 0); found || path != "" {
		t.Fatalf("findOverlappingRoute(nil) = (%t, %q), want (false, empty)", found, path)
	}

	root := newNode("/")
	terminal := newNode("users")
	terminal.exist = true
	terminal.path = "/users"
	root.staticNode["users"] = terminal
	found, path := findOverlappingRoute(root, []segmentMeta{{
		segment: ":id", isVar: true, paramName: "id",
	}}, 0)
	if !found || path != "/users" {
		t.Fatalf("variable/static overlap = (%t, %q), want (true, /users)", found, path)
	}
}

func TestCoreEdgeReleaseSegmentsOverCapacity(t *testing.T) {
	buf := new([]string)
	segments := make([]string, maxPooledPathSegments+1)
	for i := range segments {
		segments[i] = "retained"
	}
	releaseSegments(buf, segments)
	for i, segment := range segments {
		if segment != "" {
			t.Fatalf("released segments[%d] = %q, want empty", i, segment)
		}
	}
	if *buf != nil {
		t.Fatalf("over-capacity slice was pooled: %v", *buf)
	}
}

func TestCoreEdgeSearchNodeNilAndNonTerminal(t *testing.T) {
	if got := searchNode(nil, nil, 0, nil); got != nil {
		t.Fatalf("searchNode(nil) = %v, want nil", got)
	}
	root := newNode("root")
	root.handler = &routeHandler{}
	if got := searchNode(root, nil, 0, map[string]string{}); got != nil {
		t.Fatalf("searchNode(non-terminal) = %v, want nil", got)
	}
}

func TestCoreEdgeServeHTTPCommitsStagedStatusWhenHandlerSkipped(t *testing.T) {
	e := routeTestNewEngine(t)
	handlerCalled := false
	e.register(methodGet, "/core-edge-abort", func() { handlerCalled = true })
	e.use(func(c *Context) {
		c.writer.WriteHeader(http.StatusAccepted)
		c.Abort()
	})
	e.generateRoute()

	recorder := routeTestServe(e, http.MethodGet, "/core-edge-abort")
	if handlerCalled {
		t.Fatal("business handler ran after middleware aborted")
	}
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusAccepted)
	}
	if recorder.Body.Len() != 0 {
		t.Fatalf("body = %q, want empty", recorder.Body.String())
	}
}
