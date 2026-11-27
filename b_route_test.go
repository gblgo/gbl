// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"debug/dwarf"
	"fmt"
	"go/ast"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

type routeTestDiscardLogger struct{}

func (routeTestDiscardLogger) Info(string, ...any)  {}
func (routeTestDiscardLogger) Warn(string, ...any)  {}
func (routeTestDiscardLogger) Error(string, ...any) {}

func routeTestNewEngine(t *testing.T) *engine {
	t.Helper()
	e := newEngine()
	e.logger = routeTestDiscardLogger{}
	t.Cleanup(e.cancelStreams)
	t.Cleanup(e.hijacks.closeAll)
	return e
}

func routeTestCapturePanic(fn func()) (recovered any) {
	defer func() { recovered = recover() }()
	fn()
	return nil
}

func routeTestRequirePanic(t *testing.T, fn func()) any {
	t.Helper()
	value := routeTestCapturePanic(fn)
	if value == nil {
		t.Fatal("expected a panic")
	}
	return value
}

func routeTestServe(e *engine, method, target string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, httptest.NewRequest(method, target, nil))
	return recorder
}

func routeTestAllowSet(t *testing.T, header string) map[string]bool {
	t.Helper()
	set := make(map[string]bool)
	if header == "" {
		return set
	}
	for _, method := range strings.Split(header, ",") {
		method = strings.TrimSpace(method)
		if method == "" {
			t.Fatalf("Allow contains an empty method: %q", header)
		}
		if set[method] {
			t.Fatalf("Allow contains duplicate method %q: %q", method, header)
		}
		set[method] = true
	}
	return set
}

func routeTestRequireAllow(t *testing.T, got string, want ...string) {
	t.Helper()
	gotSet := routeTestAllowSet(t, got)
	wantSet := make(map[string]bool, len(want))
	for _, method := range want {
		wantSet[method] = true
	}
	if !reflect.DeepEqual(gotSet, wantSet) {
		t.Fatalf("Allow = %q (%v), want methods %v", got, gotSet, want)
	}
}

func routeTestNoArgHandler() string { return "ok" }

func routeTestEchoID(id string) string { return id }

func routeTestEchoPostID(id, postID string) string { return id + "/" + postID }

func routeTestStaticMe() string { return "static-me" }

func routeTestExplicitHead() string { return "explicit-head" }

func routeTestCustomOptions() string { return "custom-options" }

func TestRouteMethodOf(t *testing.T) {
	tests := []struct {
		input string
		want  httpMethod
		ok    bool
	}{
		{"GET", methodGet, true},
		{"get", methodGet, true},
		{"HeAd", methodHead, true},
		{"POST", methodPost, true},
		{"put", methodPut, true},
		{"PATCH", methodPatch, true},
		{"delete", methodDelete, true},
		{"OPTIONS", methodOptions, true},
		{"", 0, false},
		{" TRACE", 0, false},
		{"TRACE", 0, false},
		{"CONNECT", 0, false},
		{"CUSTOM", 0, false},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%q", tt.input), func(t *testing.T) {
			got, ok := methodOf(tt.input)
			if got != tt.want || ok != tt.ok {
				t.Fatalf("methodOf(%q) = (%d, %v), want (%d, %v)", tt.input, got, ok, tt.want, tt.ok)
			}
		})
	}
	for method, name := range methodNames {
		got, ok := methodOf(strings.ToLower(name))
		if !ok || got != httpMethod(method) {
			t.Errorf("method table entry %d (%q) did not round-trip: (%d, %v)", method, name, got, ok)
		}
	}
}

func TestRouteDealPathNormalizes(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantPath  string
		wantMetas []segmentMeta
	}{
		{
			name:     "leading trailing and repeated separators",
			input:    "  //api///users//:id/  ",
			wantPath: "/api/users/:id",
			wantMetas: []segmentMeta{
				{segment: "api"},
				{segment: "users"},
				{segment: ":id", isVar: true, paramName: "id"},
			},
		},
		{
			name:     "brace parameter",
			input:    "/teams/{teamID}/members/{memberID}",
			wantPath: "/teams/{teamID}/members/{memberID}",
			wantMetas: []segmentMeta{
				{segment: "teams"},
				{segment: "{teamID}", isVar: true, paramName: "teamID"},
				{segment: "members"},
				{segment: "{memberID}", isVar: true, paramName: "memberID"},
			},
		},
		{
			name:      "unicode identifier",
			input:     "/用户/:编号",
			wantPath:  "/用户/:编号",
			wantMetas: []segmentMeta{{segment: "用户"}, {segment: ":编号", isVar: true, paramName: "编号"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := routeTestNewEngine(t)
			metas, path := e.dealPath(tt.input)
			if path != tt.wantPath {
				t.Errorf("normalized path = %q, want %q", path, tt.wantPath)
			}
			if !reflect.DeepEqual(metas, tt.wantMetas) {
				t.Errorf("segment metadata = %#v, want %#v", metas, tt.wantMetas)
			}
		})
	}
}

func TestRouteDealPathRejectsMalformedSegments(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{"empty", ""},
		{"whitespace", " \t\n "},
		{"root", "/"},
		{"separators only", "////"},
		{"empty colon name", "/users/:"},
		{"nested colon marker", "/users/::id"},
		{"colon in literal", "/users/id:value"},
		{"empty brace name", "/users/{}"},
		{"unclosed brace", "/users/{id"},
		{"nested opening brace", "/users/{{id}}"},
		{"opening brace in literal", "/users/id{value}"},
		{"closing brace in literal", "/users/id}value"},
		{"unpaired trailing closing brace", "/users/id}"},
		{"invalid identifier character", "/users/{user-id}"},
		{"keyword colon parameter", "/users/:for"},
		{"keyword brace parameter", "/users/{range}"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := routeTestNewEngine(t)
			routeTestRequirePanic(t, func() { e.dealPath(tt.path) })
		})
	}
}

func TestRouteStaticAndDynamicPriority(t *testing.T) {
	for _, staticFirst := range []bool{false, true} {
		name := "dynamic registered first"
		if staticFirst {
			name = "static registered first"
		}
		t.Run(name, func(t *testing.T) {
			e := routeTestNewEngine(t)
			// Registration order must not decide the winner: both patterns are
			// valid, and the literal branch must win for /users/me.
			if staticFirst {
				e.register(methodGet, "/users/me", routeTestStaticMe)
				e.register(methodGet, "/users/{id}", routeTestEchoID)
			} else {
				e.register(methodGet, "/users/{id}", routeTestEchoID)
				e.register(methodGet, "/users/me", routeTestStaticMe)
			}
			e.generateRoute()

			for _, tt := range []struct {
				path string
				want string
			}{
				{"/users/me", "static-me"},
				{"/users/42", "42"},
				{"/users/me/", "static-me"},
				{"//users//me", "static-me"},
			} {
				t.Run(tt.path, func(t *testing.T) {
					recorder := routeTestServe(e, http.MethodGet, tt.path)
					if recorder.Code != http.StatusOK || recorder.Body.String() != tt.want {
						t.Fatalf("GET %s = (%d, %q), want (200, %q)", tt.path, recorder.Code, recorder.Body.String(), tt.want)
					}
				})
			}
		})
	}
}

func TestRouteMatchNormalizesRequestPaths(t *testing.T) {
	e := routeTestNewEngine(t)
	e.register(methodGet, "/static/path", routeTestNoArgHandler)
	e.register(methodGet, "/users/:id/posts/:postID", routeTestEchoID)
	e.generateRoute()

	for _, tt := range []struct {
		path string
		want string
	}{
		{"/static/path", "ok"},
		{"//static//path/", "ok"},
		{"/users/42/posts/7", "42"},
		{"//users//42///posts/7/", "42"},
	} {
		t.Run(tt.path, func(t *testing.T) {
			recorder := routeTestServe(e, http.MethodGet, tt.path)
			if recorder.Code != http.StatusOK || recorder.Body.String() != tt.want {
				t.Fatalf("GET %s = (%d, %q), want (200, %q)", tt.path, recorder.Code, recorder.Body.String(), tt.want)
			}
		})
	}
}

func TestRouteConflicts(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*engine)
	}{
		{
			name: "duplicate static route",
			setup: func(e *engine) {
				e.register(methodGet, "/users/me", routeTestNoArgHandler)
				e.register(methodGet, "/users/me", routeTestNoArgHandler)
			},
		},
		{
			name: "duplicate normalized static route",
			setup: func(e *engine) {
				e.register(methodGet, "/users/me", routeTestNoArgHandler)
				e.register(methodGet, " //users//me/ ", routeTestNoArgHandler)
			},
		},
		{
			name: "duplicate dynamic route",
			setup: func(e *engine) {
				e.register(methodGet, "/users/{id}", routeTestEchoID)
				e.register(methodGet, "/users/{id}", routeTestEchoID)
			},
		},
		{
			name: "equivalent dynamic names",
			setup: func(e *engine) {
				e.register(methodGet, "/users/{id}", routeTestEchoID)
				e.register(methodGet, "/users/:userID", routeTestEchoID)
			},
		},
		{
			name: "equivalent mixed markers",
			setup: func(e *engine) {
				e.register(methodGet, "/users/:id/posts/:postID", routeTestEchoID)
				e.register(methodGet, "/users/{id}/posts/{postID}", routeTestEchoID)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := routeTestNewEngine(t)
			tt.setup(e)
			routeTestRequirePanic(t, e.generateRoute)
		})
	}
}

func TestRouteDuplicatePathParameterNamesRejected(t *testing.T) {
	e := routeTestNewEngine(t)
	e.register(methodGet, "/users/{id}/posts/{id}", routeTestEchoID)
	// One map key cannot represent two distinct path positions. Startup must
	// reject the pattern instead of silently binding both arguments to the last.
	routeTestRequirePanic(t, e.generateRoute)
}

func TestRouteCompatibleAndCrossMethodRegistrations(t *testing.T) {
	t.Run("dynamic prefix routes coexist", func(t *testing.T) {
		e := routeTestNewEngine(t)
		e.register(methodGet, "/users/{id}", routeTestEchoID)
		e.register(methodGet, "/users/{id}/posts/{postID}", routeTestEchoPostID)
		e.generateRoute()
		if got := routeTestServe(e, http.MethodGet, "/users/42").Body.String(); got != "42" {
			t.Fatalf("prefix route body = %q, want 42", got)
		}
		recorder := routeTestServe(e, http.MethodGet, "/users/42/posts/7")
		if recorder.Code != http.StatusOK || recorder.Body.String() != "42/7" {
			t.Fatalf("longer dynamic route = (%d, %q), want (200, 42/7)", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("same path on different methods", func(t *testing.T) {
		e := routeTestNewEngine(t)
		e.register(methodGet, "/resource/{id}", routeTestEchoID)
		e.register(methodPost, "/resource/{id}", routeTestEchoID)
		e.register(methodDelete, "/resource/{id}", routeTestEchoID)
		e.generateRoute()
		for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
			recorder := routeTestServe(e, method, "/resource/42")
			if recorder.Code != http.StatusOK || recorder.Body.String() != "42" {
				t.Errorf("%s = (%d, %q), want (200, 42)", method, recorder.Code, recorder.Body.String())
			}
		}
	})
}

func TestRouteHEADFallbackAndExplicitOverride(t *testing.T) {
	t.Run("static GET fallback", func(t *testing.T) {
		e := routeTestNewEngine(t)
		e.register(methodGet, "/resource", routeTestNoArgHandler)
		e.generateRoute()
		recorder := routeTestServe(e, http.MethodHead, "/resource")
		if recorder.Code != http.StatusOK {
			t.Fatalf("HEAD status = %d, want 200", recorder.Code)
		}
	})

	t.Run("dynamic GET fallback binds path parameters", func(t *testing.T) {
		e := routeTestNewEngine(t)
		e.register(methodGet, "/users/:id", routeTestEchoID)
		e.generateRoute()
		recorder := routeTestServe(e, http.MethodHead, "/users/42")
		if recorder.Code != http.StatusOK || recorder.Body.String() != "42" {
			t.Fatalf("HEAD fallback = (%d, %q), want handler result (200, 42)", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("explicit HEAD wins", func(t *testing.T) {
		e := routeTestNewEngine(t)
		e.register(methodGet, "/resource", routeTestNoArgHandler)
		e.register(methodHead, "/resource", routeTestExplicitHead)
		e.generateRoute()
		recorder := routeTestServe(e, http.MethodHead, "/resource")
		if recorder.Code != http.StatusOK || recorder.Body.String() != "explicit-head" {
			t.Fatalf("explicit HEAD = (%d, %q), want (200, explicit-head)", recorder.Code, recorder.Body.String())
		}
	})
}

func TestRouteAutomaticOptionsAndAllow(t *testing.T) {
	e := routeTestNewEngine(t)
	e.register(methodGet, "/resource/{id}", routeTestEchoID)
	e.register(methodPost, "/resource/{id}", routeTestEchoID)
	e.generateRoute()

	recorder := routeTestServe(e, http.MethodOptions, "/resource/42")
	if recorder.Code != http.StatusNoContent {
		t.Errorf("automatic OPTIONS status = %d, want 204", recorder.Code)
	}
	if recorder.Body.Len() != 0 {
		t.Errorf("automatic OPTIONS body = %q, want empty", recorder.Body.String())
	}
	routeTestRequireAllow(t, recorder.Header().Get(allow), http.MethodGet, http.MethodHead, http.MethodPost, http.MethodOptions)
}

func TestRouteAutomaticOptionsCanBeOverridden(t *testing.T) {
	e := routeTestNewEngine(t)
	e.register(methodGet, "/resource", routeTestNoArgHandler)
	e.register(methodOptions, "/resource", routeTestCustomOptions)
	e.generateRoute()

	recorder := routeTestServe(e, http.MethodOptions, "/resource")
	if recorder.Code != http.StatusOK || recorder.Body.String() != "custom-options" {
		t.Fatalf("custom OPTIONS = (%d, %q), want (200, custom-options)", recorder.Code, recorder.Body.String())
	}
}

func TestRouteMethodNotAllowedAndNotFound(t *testing.T) {
	e := routeTestNewEngine(t)
	e.register(methodGet, "/only-get", routeTestNoArgHandler)
	e.register(methodPost, "/items/:id", routeTestEchoID)
	e.register(methodHead, "/explicit-head", routeTestExplicitHead)
	e.generateRoute()

	t.Run("405 includes GET implied HEAD and automatic OPTIONS", func(t *testing.T) {
		recorder := routeTestServe(e, http.MethodPut, "/only-get")
		if recorder.Code != http.StatusMethodNotAllowed {
			t.Fatalf("PUT /only-get status = %d, want 405", recorder.Code)
		}
		routeTestRequireAllow(t, recorder.Header().Get(allow), http.MethodGet, http.MethodHead, http.MethodOptions)
	})

	t.Run("405 on dynamic path", func(t *testing.T) {
		recorder := routeTestServe(e, http.MethodDelete, "/items/42")
		if recorder.Code != http.StatusMethodNotAllowed {
			t.Fatalf("DELETE /items/42 status = %d, want 405", recorder.Code)
		}
		routeTestRequireAllow(t, recorder.Header().Get(allow), http.MethodPost, http.MethodOptions)
	})

	t.Run("explicit methods are advertised", func(t *testing.T) {
		recorder := routeTestServe(e, http.MethodGet, "/explicit-head")
		if recorder.Code != http.StatusMethodNotAllowed {
			t.Fatalf("GET /explicit-head status = %d, want 405", recorder.Code)
		}
		routeTestRequireAllow(t, recorder.Header().Get(allow), http.MethodHead, http.MethodOptions)
	})

	t.Run("unknown path is 404 without Allow", func(t *testing.T) {
		recorder := routeTestServe(e, http.MethodPost, "/missing")
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("POST /missing status = %d, want 404", recorder.Code)
		}
		if got := recorder.Header().Get(allow); got != "" {
			t.Fatalf("404 Allow = %q, want empty", got)
		}
	})

	t.Run("unsupported method is 501", func(t *testing.T) {
		recorder := routeTestServe(e, http.MethodTrace, "/only-get")
		if recorder.Code != http.StatusNotImplemented {
			t.Fatalf("TRACE /only-get status = %d, want 501", recorder.Code)
		}
		if got := recorder.Header().Get(allow); got != "" {
			t.Fatalf("501 Allow = %q, want empty", got)
		}
	})
}

func TestRouteAllowForIncludesAutomaticMethods(t *testing.T) {
	e := routeTestNewEngine(t)
	e.register(methodGet, "/users/:id", routeTestEchoID)
	e.register(methodPatch, "/users/:id", routeTestEchoID)
	e.generateRoute()
	routeTestRequireAllow(t, strings.Join(e.allowFor("/users/42"), headerComma),
		http.MethodGet, http.MethodHead, http.MethodPatch, http.MethodOptions)
}

func routeTestOrderMiddleware(name string, order *[]string) HandlerFunc {
	return func(c *Context) {
		*order = append(*order, name+":before")
		c.Next()
		*order = append(*order, name+":after")
	}
}

func TestRouteGroupPrefixAndMiddlewareOrder(t *testing.T) {
	e := routeTestNewEngine(t)
	order := make([]string, 0, 8)
	e.middlewares = []HandlerFunc{routeTestOrderMiddleware("global", &order)}

	group := e.group("/api//v1/")
	if got := group.GroupUse(routeTestOrderMiddleware("group-one", &order)); got != group {
		t.Fatal("GroupUse did not return the same group")
	}
	if got := group.Get("/users/:id", routeTestEchoID); got != group {
		t.Fatal("Get did not return the same group")
	}

	other := e.group("/other").GroupUse(routeTestOrderMiddleware("other", &order))
	other.Get("/users/:id", routeTestEchoID)
	e.register(methodGet, "/outside", routeTestNoArgHandler)
	e.generateRoute()

	recorder := routeTestServe(e, http.MethodGet, "/api/v1/users/7")
	if recorder.Code != http.StatusOK || recorder.Body.String() != "7" {
		t.Fatalf("grouped route = (%d, %q), want (200, 7)", recorder.Code, recorder.Body.String())
	}
	wantOrder := []string{
		"global:before", "group-one:before", "group-one:after", "global:after",
	}
	if !reflect.DeepEqual(order, wantOrder) {
		t.Fatalf("middleware order = %v, want %v", order, wantOrder)
	}

	order = order[:0]
	recorder = routeTestServe(e, http.MethodGet, "/outside")
	if recorder.Code != http.StatusOK || recorder.Body.String() != "ok" {
		t.Fatalf("outside route = (%d, %q), want (200, ok)", recorder.Code, recorder.Body.String())
	}
	if want := []string{"global:before", "global:after"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("outside middleware order = %v, want %v", order, want)
	}

	order = order[:0]
	recorder = routeTestServe(e, http.MethodGet, "/other/users/8")
	if recorder.Code != http.StatusOK || recorder.Body.String() != "8" {
		t.Fatalf("other group route = (%d, %q), want (200, 8)", recorder.Code, recorder.Body.String())
	}
	if want := []string{"global:before", "other:before", "other:after", "global:after"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("other group middleware order = %v, want %v", order, want)
	}
}

func TestRouteGroupRegistersEveryVerb(t *testing.T) {
	e := routeTestNewEngine(t)
	group := e.group("/verbs")
	group.GroupUse(routeTestOrderMiddleware("group", new([]string)))

	registrations := []struct {
		name string
		call func(RouteGroup) RouteGroup
	}{
		{"GET", func(g RouteGroup) RouteGroup { return g.Get("/get", routeTestNoArgHandler) }},
		{"HEAD", func(g RouteGroup) RouteGroup { return g.Head("/head", routeTestNoArgHandler) }},
		{"POST", func(g RouteGroup) RouteGroup { return g.Post("/post", routeTestNoArgHandler) }},
		{"PUT", func(g RouteGroup) RouteGroup { return g.Put("/put", routeTestNoArgHandler) }},
		{"PATCH", func(g RouteGroup) RouteGroup { return g.Patch("/patch", routeTestNoArgHandler) }},
		{"DELETE", func(g RouteGroup) RouteGroup { return g.Delete("/delete", routeTestNoArgHandler) }},
		{"OPTIONS", func(g RouteGroup) RouteGroup { return g.Options("/options", routeTestNoArgHandler) }},
	}
	for _, registration := range registrations {
		if got := registration.call(group); got != group {
			t.Errorf("%s did not return the same group", registration.name)
		}
	}
	if len(e.routeMetaSlice) != len(registrations) {
		t.Fatalf("queued registrations = %d, want %d", len(e.routeMetaSlice), len(registrations))
	}
	for i, registration := range registrations {
		meta := e.routeMetaSlice[i]
		wantMethod, ok := methodOf(registration.name)
		if !ok || meta.method != wantMethod {
			t.Errorf("registration %d method = %d, want %d", i, meta.method, wantMethod)
		}
		wantPath := "/verbs/" + strings.ToLower(registration.name)
		if meta.path != wantPath {
			t.Errorf("registration %d path = %q, want %q", i, meta.path, wantPath)
		}
		if len(meta.mw) != 1 {
			t.Errorf("registration %d middleware count = %d, want 1", i, len(meta.mw))
		}
	}
}

// routeTestDWARFNameHandler provides a stable position for injected DWARF names.
func routeTestDWARFNameHandler(value int) string { return strconv.Itoa(value) }

func TestRouteGenerationUsesDWARFNames(t *testing.T) {
	e := routeTestNewEngine(t)
	value := reflect.ValueOf(routeTestDWARFNameHandler)
	file, line := runtime.FuncForPC(value.Pointer()).FileLine(value.Pointer())
	key := file + colon + strconv.Itoa(line)
	e.dwarfIndex = map[string]dwarfParamNames{key: {names: []string{"binaryName"}}}
	e.register(methodGet, "/dwarf", routeTestDWARFNameHandler)
	e.generateRoute()
	handler := e.staticRoutes[methodGet]["/dwarf"]
	var names []string
	if handler != nil {
		names = handler.paramNames
	}
	if len(names) != 1 || names[0] != "binaryName" {
		t.Fatalf("recovered names = %#v, want binaryName", names)
	}
}

func TestRouteGenerationUsesSourceNamesWhenDWARFMissing(t *testing.T) {
	e := routeTestNewEngine(t)
	e.dwarfIndex = map[string]dwarfParamNames{}
	e.register(methodGet, "/source", routeTestSourceNamed)
	e.generateRoute()
	handler := e.staticRoutes[methodGet]["/source"]
	var names []string
	if handler != nil {
		names = handler.paramNames
	}
	if !reflect.DeepEqual(names, []string{"id", "count"}) {
		t.Fatalf("source parameter names = %#v, want [id count]", names)
	}
}

func TestRouteSourceParamNameShapes(t *testing.T) {
	tests := []struct {
		name string
		fn   any
		want []string
	}{
		{name: "named", fn: routeTestSourceNamed, want: []string{"id", "count"}},
		{name: "unnamed", fn: routeTestSourceUnnamed, want: []string{"", ""}},
		{name: "blank", fn: routeTestSourceBlank, want: []string{"_", "named"}},
		{name: "closure", fn: routeTestSourceClosure(), want: []string{"closureID"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			names, resolved, ambiguous := resolveSourceParamNames(reflect.ValueOf(tt.fn))
			if ambiguous != "" || !resolved || !reflect.DeepEqual(names, tt.want) {
				t.Fatalf("source names = (%#v, %t, %q), want (%#v, true, empty)",
					names, resolved, ambiguous, tt.want)
			}
		})
	}
}

func TestRouteSourceParamNameAmbiguity(t *testing.T) {
	spans := []sourceParamSpan{
		{start: 1, end: 5, names: []string{"outer"}},
		{start: 1, end: 5, names: []string{"inner"}},
	}
	span, ambiguous := selectSourceParamSpan(spans, 3, "same.go")
	if span != nil || ambiguous != "same.go:3" {
		t.Fatalf("same-size spans = (%#v, %q), want ambiguous", span, ambiguous)
	}

	spans = []sourceParamSpan{
		{start: 1, end: 8, names: []string{"outer"}},
		{start: 2, end: 4, names: []string{"inner"}},
	}
	span, ambiguous = selectSourceParamSpan(spans, 3, "nested.go")
	if ambiguous != "" || span == nil || !reflect.DeepEqual(span.names, []string{"inner"}) {
		t.Fatalf("nested spans = (%#v, %q), want inner", span, ambiguous)
	}

	span, ambiguous = selectSourceParamSpan(spans, 20, "nested.go")
	if span != nil || ambiguous != "" {
		t.Fatalf("outside spans = (%#v, %q), want no match", span, ambiguous)
	}
}

func TestRouteSourceParamNameMissingFile(t *testing.T) {
	names, resolved, ambiguous := resolveSourceParamNames(reflect.ValueOf(routeTestSourceMissing))
	if names != nil || resolved || ambiguous != "" {
		t.Fatalf("missing source = (%#v, %t, %q), want unresolved", names, resolved, ambiguous)
	}
}

func TestRouteSourceParamNameDefensiveBranches(t *testing.T) {
	if names, resolved, ambiguous := resolveSourceParamNames(reflect.Value{}); names != nil ||
		resolved || ambiguous != "" {
		t.Fatalf("invalid source value = (%#v, %t, %q), want unresolved", names, resolved, ambiguous)
	}
	if names := sourceParamNames(&ast.FuncType{}); names != nil {
		t.Fatalf("parameterless source signature = %#v, want nil", names)
	}

	value := reflect.ValueOf(routeTestSourceNamed)
	file, line := runtime.FuncForPC(value.Pointer()).FileLine(value.Pointer())
	previous, loaded := sourceParamFileCache.Load(file)
	t.Cleanup(func() {
		if loaded {
			sourceParamFileCache.Store(file, previous)
		} else {
			sourceParamFileCache.Delete(file)
		}
	})

	sourceParamFileCache.Store(file, sourceParamFile{
		spans:  []sourceParamSpan{{start: line + 10, end: line + 20}},
		parsed: true,
	})
	if names, resolved, ambiguous := resolveSourceParamNames(value); names != nil ||
		resolved || ambiguous != "" {
		t.Fatalf("source line outside spans = (%#v, %t, %q), want unresolved",
			names, resolved, ambiguous)
	}

	sourceParamFileCache.Store(file, sourceParamFile{
		spans: []sourceParamSpan{
			{start: line, end: line, names: []string{"left"}},
			{start: line, end: line, names: []string{"right"}},
		},
		parsed: true,
	})
	names, resolved, ambiguous := resolveSourceParamNames(value)
	if names != nil || resolved || ambiguous == "" {
		t.Fatalf("ambiguous source line = (%#v, %t, %q), want ambiguous",
			names, resolved, ambiguous)
	}
}

func TestRouteGoTestBinaryDetection(t *testing.T) {
	if !runningUnderGoTest() {
		t.Fatal("go test binary was not detected")
	}
	tests := []struct {
		name      string
		testFlags bool
		args      []string
		want      bool
	}{
		{name: "flags", testFlags: true, want: true},
		{name: "unix suffix", args: []string{"/tmp/pkg.test"}, want: true},
		{name: "windows suffix", args: []string{`C:\tmp\pkg.test.exe`}, want: true},
		{name: "test argument", args: []string{"custom", "-test.v"}, want: true},
		{name: "production", args: []string{"server"}, want: false},
		{name: "empty", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isGoTestBinary(tt.testFlags, tt.args); got != tt.want {
				t.Fatalf("isGoTestBinary(%t, %#v) = %t, want %t", tt.testFlags, tt.args, got, tt.want)
			}
		})
	}
}

func TestRouteNormalizeParamNames(t *testing.T) {
	tests := []struct {
		name  string
		names []string
		count int
		want  []string
	}{
		{"exact", []string{"id", "count"}, 2, []string{"id", "count"}},
		{"padding", []string{"id"}, 3, []string{"id", "arg1", "arg2"}},
		{"empty slot", []string{"id", "", "count"}, 3, []string{"id", "arg1", "count"}},
		{"truncation", []string{"a", "b", "c"}, 2, []string{"a", "b"}},
		{"all unnamed", nil, 2, []string{"arg0", "arg1"}},
		{"zero", nil, 0, []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeParamNames(tt.names, tt.count)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("normalizeParamNames(%v, %d) = %v, want %v", tt.names, tt.count, got, tt.want)
			}
		})
	}
}

func routeTestFunctionKey(t *testing.T, fn any) string {
	t.Helper()
	value := reflect.ValueOf(fn)
	fun := runtime.FuncForPC(value.Pointer())
	if fun == nil {
		t.Fatal("runtime function metadata is unavailable")
	}
	file, line := fun.FileLine(value.Pointer())
	return file + ":" + strconv.Itoa(line)
}

func TestRouteDWARFLookup(t *testing.T) {
	key := routeTestFunctionKey(t, routeTestDWARFNamed)
	tests := []struct {
		name      string
		index     map[string]dwarfParamNames
		want      []string
		resolved  bool
		ambiguous string
	}{
		{
			name:     "named entry",
			index:    map[string]dwarfParamNames{key: {names: []string{"id", "count"}}},
			want:     []string{"id", "count"},
			resolved: true,
		},
		{
			name:     "missing entry",
			index:    map[string]dwarfParamNames{},
			resolved: false,
		},
		{
			name:     "entry without names",
			index:    map[string]dwarfParamNames{key: {}},
			resolved: false,
		},
		{
			name:      "ambiguous entry",
			index:     map[string]dwarfParamNames{key: {ambiguous: true}},
			resolved:  false,
			ambiguous: key,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := routeTestNewEngine(t)
			e.dwarfIndex = tt.index
			names, resolved, ambiguous := e.lookupParamNamesDWARF(reflect.ValueOf(routeTestDWARFNamed))
			if resolved != tt.resolved || ambiguous != tt.ambiguous || !reflect.DeepEqual(names, tt.want) {
				t.Fatalf("DWARF lookup = (%v, %v, %q), want (%v, %v, %q)", names, resolved, ambiguous, tt.want, tt.resolved, tt.ambiguous)
			}
		})
	}
}

func TestRouteDWARFLookupCachesUnavailableDWARF(t *testing.T) {
	const executable = "route-test-executable"
	loads := 0
	coverageSeamReplace(t, &osExecutable, func() (string, error) {
		return executable, nil
	})
	coverageSeamReplace(t, &selfDWARFLoader, func(path string) *dwarf.Data {
		if path != executable {
			t.Fatalf("DWARF loader path = %q, want %q", path, executable)
		}
		loads++
		return nil
	})

	e := routeTestNewEngine(t)
	value := reflect.ValueOf(routeTestDWARFNamed)
	for range 2 {
		names, resolved, ambiguous := e.lookupParamNamesDWARF(value)
		if names != nil || resolved || ambiguous != empty {
			t.Fatalf("DWARF lookup without debug data = (%v, %t, %q), want (nil, false, empty)",
				names, resolved, ambiguous)
		}
	}
	if loads != 1 {
		t.Fatalf("DWARF loader calls = %d, want 1 cached unavailable lookup", loads)
	}
	if e.dwarfIndex == nil || len(e.dwarfIndex) != 0 {
		t.Fatalf("cached DWARF index = %#v, want a non-nil empty map", e.dwarfIndex)
	}
}

func TestRouteDWARFIndexFromTestBinary(t *testing.T) {
	index := buildDWARFIndex()
	if index == nil {
		t.Skip("test binary has no readable DWARF; rebuild without -s or -w")
	}
	key := routeTestFunctionKey(t, routeTestDWARFNamed)
	entry, ok := index[key]
	if !ok {
		t.Fatalf("DWARF index has no entry for test handler at %s", key)
	}
	if entry.ambiguous {
		t.Fatalf("DWARF index marks test handler ambiguous at %s", key)
	}
	want := []string{"id", "count"}
	if len(entry.names) < len(want) || !reflect.DeepEqual(entry.names[:len(want)], want) {
		t.Fatalf("DWARF input names at %s = %v, want prefix %v", key, entry.names, want)
	}
}

func TestRouteOpenSelfDWARFRejectsUnreadableFiles(t *testing.T) {
	if got := openSelfDWARF(t.TempDir() + string(os.PathSeparator) + "missing.exe"); got != nil {
		t.Fatal("missing executable unexpectedly produced DWARF data")
	}
	path := t.TempDir() + string(os.PathSeparator) + "not-an-executable"
	if err := os.WriteFile(path, []byte("not an executable"), 0o600); err != nil {
		t.Fatalf("write malformed executable fixture: %v", err)
	}
	if got := openSelfDWARF(path); got != nil {
		t.Fatal("malformed executable unexpectedly produced DWARF data")
	}
}

func routeTestSourceNamed(id string, count int) string { return id }

func routeTestSourceUnnamed(string, int) string { return "ok" }

func routeTestSourceBlank(_ string, named int) int { return named }

func routeTestSourceClosure() func(string) string {
	return func(closureID string) string { return closureID }
}

//go:noinline
func routeTestDWARFNamed(id string, count int) string { return id }

func routeTestRequirePanicMessage(t *testing.T, want string, fn func()) any {
	t.Helper()
	panicValue := routeTestRequirePanic(t, fn)
	if message := fmt.Sprint(panicValue); !strings.Contains(message, want) {
		t.Fatalf("panic = %v, want substring %q", panicValue, want)
	}
	return panicValue
}

type routeTestMethodReceiver struct{}

func (*routeTestMethodReceiver) Lookup(id string) string { return id }

func routeTestGenericHandler[T any](value T) T { return value }

func routeTestMultipleReturns() (string, error) { return "value", nil }

func routeTestVariadicHandler(values ...string) {}

var routeTestTypedNilHandler func(string) string

type routeTestRecursiveResult struct {
	Value string                    `json:"value"`
	Next  *routeTestRecursiveResult `json:"next,omitempty"`
}

func routeTestRecursiveResultHandler() *routeTestRecursiveResult {
	return &routeTestRecursiveResult{
		Value: "root",
		Next:  &routeTestRecursiveResult{Value: "leaf"},
	}
}

func routeTestPointerChain() reflect.Type {
	output := reflect.TypeFor[int]()
	for range maxTypePointerDepth + 1 {
		output = reflect.PointerTo(output)
	}
	return output
}

func routeTestRecursivePointerReturnHandler() any {
	output := routeTestPointerChain()
	// Generation must reject this signature before reflection can invoke it.
	return reflect.MakeFunc(
		reflect.FuncOf(nil, []reflect.Type{output}, false),
		func([]reflect.Value) []reflect.Value { panic("recursive-pointer handler was invoked") },
	).Interface()
}

func routeTestRecursivePointerParameterHandler() any {
	input := routeTestPointerChain()
	return reflect.MakeFunc(
		reflect.FuncOf([]reflect.Type{input}, nil, false),
		func([]reflect.Value) []reflect.Value { panic("recursive-pointer handler was invoked") },
	).Interface()
}

// routeTestHandlerForParameterType builds a no-return handler with an exact
// business parameter type, including dynamic ambiguity fixtures.
func routeTestHandlerForParameterType(typ reflect.Type) any {
	return reflect.MakeFunc(
		reflect.FuncOf([]reflect.Type{typ}, nil, false),
		func([]reflect.Value) []reflect.Value { return nil },
	).Interface()
}

type routeTestExtendedWriter interface {
	http.ResponseWriter
	Extended()
}

func routeTestCountMismatchHandler(id string, count int) string { return id }

func TestRouteGenerationRejectsInvalidHandlers(t *testing.T) {
	receiver := &routeTestMethodReceiver{}
	tests := []struct {
		name string
		fn   any
		want string
	}{
		{"nil", nil, "invalid route handler"},
		{"non-function", 42, "invalid route handler"},
		{"typed nil", routeTestTypedNilHandler, "invalid route handler"},
		{"multiple returns", routeTestMultipleReturns, "at most one return value"},
		{"variadic", routeTestVariadicHandler, "variadic handlers are not supported"},
		{"method value", receiver.Lookup, "not supported: method value"},
		{"method expression", (*routeTestMethodReceiver).Lookup, "not supported: method expression"},
		{"generic instantiation", routeTestGenericHandler[int], "not supported: generic instantiation"},
		{"recursive pointer return", routeTestRecursivePointerReturnHandler(), "recursive pointer return type"},
		{"recursive pointer parameter", routeTestRecursivePointerParameterHandler(), "invalid handler parameter type"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := routeTestNewEngine(t)
			e.register(methodGet, "/invalid", tt.fn)
			routeTestRequirePanicMessage(t, tt.want, e.generateRoute)
			if e.routeMetaSlice == nil {
				t.Fatal("invalid handler cleared the registration queue")
			}
		})
	}
}

func TestRouteGenerationRejectsExplosiveFormPointerTree(t *testing.T) {
	typ := reflect.TypeFor[struct{}]()
	for range 16 {
		typ = reflect.StructOf([]reflect.StructField{
			{Name: "Left", Type: reflect.PointerTo(typ)},
			{Name: "Right", Type: reflect.PointerTo(typ)},
		})
	}
	e := routeTestNewEngine(t)
	e.register(methodGet, "/wide", routeTestHandlerForParameterType(typ))
	panicValue := routeTestRequirePanicMessage(t, "invalid handler parameter type", e.generateRoute)
	if message := fmt.Sprint(panicValue); !strings.Contains(message, "form pointer expansion") {
		t.Fatalf("startup panic = %v, want pointer expansion rejection", panicValue)
	}
}

// TestRouteGenerationRejectsAmbiguousStructFields proves binding validation fails
// during startup, before a route can be frozen or serve a request.
func TestRouteGenerationRejectsAmbiguousStructFields(t *testing.T) {
	duplicate := correctnessBindDuplicateType()
	for _, tt := range []struct {
		name string
		typ  reflect.Type
		want []string
	}{
		{
			name: "duplicate exact tag",
			typ:  duplicate,
			want: []string{duplicate.String(), "First", "Second", "duplicate exact bind name"},
		},
		{
			name: "anonymous embedding",
			typ:  correctnessConflictType(),
			want: []string{"CorrectnessLeft.Shared", "CorrectnessRight.Shared", "shared"},
		},
		{
			name: "Unicode fold",
			typ:  reflect.TypeFor[CorrectnessBindUnicodeFoldConflict](),
			want: []string{"CorrectnessBindUnicodeFoldConflict", "K", "Kelvin", "case-folding"},
		},
		{
			name: "nested slice",
			typ:  reflect.SliceOf(duplicate),
			want: []string{duplicate.String(), "First", "Second"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := routeTestNewEngine(t)
			e.register(methodGet, "/ambiguous", routeTestHandlerForParameterType(tt.typ))
			panicValue := routeTestRequirePanicMessage(t, "invalid handler parameter type", e.generateRoute)
			message := fmt.Sprint(panicValue)
			for _, want := range append([]string{"GET", "/ambiguous"}, tt.want...) {
				if !strings.Contains(message, want) {
					t.Errorf("startup panic = %v, want substring %q", panicValue, want)
				}
			}
			if e.routeMetaSlice == nil {
				t.Fatal("ambiguous handler cleared the registration queue")
			}
		})
	}
}

// TestRouteGenerationAllowsShallowStructDominance keeps the startup validator
// aligned with the low-level JSON promotion rule.
func TestRouteGenerationAllowsShallowStructDominance(t *testing.T) {
	e := routeTestNewEngine(t)
	e.register(methodGet, "/shallow",
		routeTestHandlerForParameterType(reflect.TypeFor[CorrectnessShallow]()))
	e.generateRoute()
	if e.routeMetaSlice != nil {
		t.Fatal("successful generation did not release the registration queue")
	}
}

func TestRouteGenerationRejectsUnsupportedWritersAndSSE(t *testing.T) {
	tests := []struct {
		name   string
		method httpMethod
		fn     any
		want   string
	}{
		{
			name:   "concrete writer mismatch",
			method: methodGet,
			fn:     func(http.ResponseWriter, *httptest.ResponseRecorder) {},
			want:   "response writer type is not supported",
		},
		{
			name:   "extended writer interface mismatch",
			method: methodGet,
			fn:     func(routeTestExtendedWriter) {},
			want:   "response writer type is not supported",
		},
		{
			name:   "SSE on POST",
			method: methodPost,
			fn:     func(SSEWriter) {},
			want:   "SSEWriter is only supported on GET routes",
		},
		{
			name:   "SSE on HEAD",
			method: methodHead,
			fn:     func(SSEWriter) {},
			want:   "SSEWriter is only supported on GET routes",
		},
		{
			name:   "SSE with return value",
			method: methodGet,
			fn:     func(SSEWriter) string { return "not allowed" },
			want:   "SSE handler must not return a value",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := routeTestNewEngine(t)
			e.register(tt.method, "/invalid", tt.fn)
			routeTestRequirePanicMessage(t, tt.want, e.generateRoute)
		})
	}
}

func TestRouteGenerationAllowsRecursiveStructPointerReturn(t *testing.T) {
	e := routeTestNewEngine(t)
	e.register(methodGet, "/recursive", routeTestRecursiveResultHandler)
	e.generateRoute()
	recorder := routeTestServe(e, http.MethodGet, "/recursive")
	if recorder.Code != http.StatusOK {
		t.Fatalf("recursive struct pointer status = %d, want 200", recorder.Code)
	}
	if got := recorder.Header().Get(contentType); got != applicationJsonCharset {
		t.Errorf("recursive struct pointer Content-Type = %q, want %q", got, applicationJsonCharset)
	}
	want := `{"value":"root","next":{"value":"leaf"}}`
	if got := recorder.Body.String(); got != want {
		t.Fatalf("recursive struct pointer body = %q, want %q", got, want)
	}
}

func TestRouteGenerationClassifiesSliceReturns(t *testing.T) {
	tests := []struct {
		name        string
		fn          any
		contentType string
		body        string
	}{
		{"bytes", func() []byte { return []byte("raw") }, textPlainCharset, "raw"},
		{"JSON", func() []int { return []int{1, 2} }, applicationJsonCharset, "[1,2]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := routeTestNewEngine(t)
			e.register(methodGet, "/slice", tt.fn)
			e.generateRoute()
			recorder := routeTestServe(e, http.MethodGet, "/slice")
			if recorder.Code != http.StatusOK || recorder.Body.String() != tt.body {
				t.Fatalf("slice response = (%d, %q), want (200, %q)", recorder.Code, recorder.Body.String(), tt.body)
			}
			if got := recorder.Header().Get(contentType); got != tt.contentType {
				t.Fatalf("slice Content-Type = %q, want %q", got, tt.contentType)
			}
		})
	}
}

func TestRouteUnsupportedReturnKindFailsWhenInvoked(t *testing.T) {
	e := routeTestNewEngine(t)
	e.register(methodGet, "/unsupported", func() chan int { return make(chan int) })
	// Route generation has no return-kind whitelist: channels take the default
	// plain-text branch and are rejected by response serialization at request time.
	e.generateRoute()
	recorder := routeTestServe(e, http.MethodGet, "/unsupported")
	if recorder.Code != http.StatusInternalServerError || recorder.Body.String() != responseSerializationFailed {
		t.Fatalf("unsupported return response = (%d, %q), want (500, %q)",
			recorder.Code, recorder.Body.String(), responseSerializationFailed)
	}
}

// routeTestDWARFKey returns the runtime position used by the DWARF index.
func routeTestDWARFKey(fn any) string {
	value := reflect.ValueOf(fn)
	file, line := runtime.FuncForPC(value.Pointer()).FileLine(value.Pointer())
	return file + colon + strconv.Itoa(line)
}

func TestRouteGenerationRejectsUnreliableParameterNames(t *testing.T) {
	tests := []struct {
		name  string
		fn    any
		setup func(*testing.T, *engine, any)
		want  string
	}{
		{
			name: "DWARF names missing",
			fn:   routeTestCountMismatchHandler,
			setup: func(t *testing.T, e *engine, fn any) {
				e.dwarfIndex = map[string]dwarfParamNames{}
			},
			want: "no usable DWARF parameter names",
		},
		{
			name: "DWARF entry missing",
			fn:   routeTestCountMismatchHandler,
			setup: func(t *testing.T, e *engine, fn any) {
				e.dwarfIndex = map[string]dwarfParamNames{"missing.go:1": {names: []string{"id"}}}
			},
			want: "no usable DWARF parameter names",
		},
		{
			name: "DWARF ambiguity",
			fn:   routeTestCountMismatchHandler,
			setup: func(t *testing.T, e *engine, fn any) {
				e.dwarfIndex = map[string]dwarfParamNames{routeTestDWARFKey(fn): {ambiguous: true}}
			},
			want: "ambiguous parameter names",
		},
		{
			name: "DWARF count mismatch",
			fn:   routeTestCountMismatchHandler,
			setup: func(t *testing.T, e *engine, fn any) {
				e.dwarfIndex = map[string]dwarfParamNames{routeTestDWARFKey(fn): {names: []string{"id"}}}
			},
			want: "DWARF input parameter count mismatch: got 1 names for 2 handler parameters",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := routeTestNewEngine(t)
			e.paramNameResolver = e.lookupParamNamesDWARF
			tt.setup(t, e, tt.fn)
			e.register(methodGet, "/names/:id", tt.fn)
			routeTestRequirePanicMessage(t, tt.want, e.generateRoute)
		})
	}
}

//line missing_source.go:7
func routeTestSourceMissing(id string) string { return id }
