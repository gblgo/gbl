// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"mime/multipart"
	"net/url"
	"reflect"
	"testing"
)

// These fixtures are shared with request_bench_test.go. Each binding benchmark
// times only injection/binding: parsed values or the JSON/multipart payload are
// constructed once before the timer, and every variant reuses the same bytes.
const (
	bindTestBenchFormPayload = "id=42&name=alice&active=true&tags=go&tags=http&pair=1&pair=2&raw=hello"
	bindTestBenchJSONPayload = `{"id":42,"name":"alice","active":true,"tags":["go","http"],"pair":[1,2],"raw":"aGVsbG8="}`
)

type bindTestBenchStruct struct {
	ID     int      `json:"id"`
	Name   string   `json:"name"`
	Active bool     `json:"active"`
	Tags   []string `json:"tags"`
	Pair   [2]int   `json:"pair"`
	Raw    []byte   `json:"raw"`
}

func bindTestBenchContext(typ reflect.Type, name string) (*Context, []reflect.Value) {
	fnType := reflect.FuncOf([]reflect.Type{typ}, nil, false)
	plan, err := buildBindPlan(fnType)
	if err != nil {
		panic(err)
	}
	names := []string{name}
	plan.prepareKeys(names)
	c := &Context{rh: &routeHandler{plan: plan, paramNames: names}}
	return c, make([]reflect.Value, 1)
}

func bindTestBenchCheck(b *testing.B, c *Context, params []reflect.Value, form url.Values, files filesMap, body []byte, jsonMedia bool) {
	b.Helper()
	if err := injectedParameters(c, form, files, body, jsonMedia, params); err != nil {
		b.Fatalf("inject benchmark parameters: %v", err)
	}
}

func BenchmarkBindScalar(b *testing.B) {
	form, err := url.ParseQuery(bindTestBenchFormPayload)
	if err != nil {
		b.Fatal(err)
	}
	c, params := bindTestBenchContext(reflect.TypeFor[int](), "id")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bindTestBenchCheck(b, c, params, form, nil, nil, false)
		if params[0].Int() != 42 {
			b.Fatalf("bound scalar = %v, want 42", params[0].Interface())
		}
	}
}

func BenchmarkBindStruct(b *testing.B) {
	form, err := url.ParseQuery(bindTestBenchFormPayload)
	if err != nil {
		b.Fatal(err)
	}
	c, params := bindTestBenchContext(reflect.TypeFor[bindTestBenchStruct](), "payload")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		bindTestBenchCheck(b, c, params, form, nil, nil, false)
		got := params[0].Addr().Interface().(*bindTestBenchStruct)
		if got.Name != "alice" {
			b.Fatalf("bound struct = %#v", got)
		}
	}
}

// BenchmarkFrameworkJSONFieldExtraction measures pooled multi-parameter member selection.
func BenchmarkFrameworkJSONFieldExtraction(b *testing.B) {
	body := []byte(`{"id":42,"name":"alice","enabled":true,"noise":"ignored"}`)
	keys := []string{"id", "name", "enabled"}
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	b.ResetTimer()
	for range b.N {
		fields := acquireJSONFields(len(keys))
		if err := decodeJSONFieldsListInto(body, keys, fields.values); err != nil {
			releaseJSONFields(fields)
			b.Fatalf("select JSON fields: %v", err)
		}
		if len(fields.values) != len(keys) {
			releaseJSONFields(fields)
			b.Fatalf("selected JSON fields = %#v", fields.values)
		}
		releaseJSONFields(fields)
	}
}

func BenchmarkBindMultipart(b *testing.B) {
	b.StopTimer()
	r, _ := requestTestNewMultipart(b, "/bench", url.Values{"name": {"alice"}},
		requestTestUpload{field: "upload", filename: "bench.txt", body: []byte("hello")})
	form, files, body, jsonMedia, err := parseRequestParameters(nil, r, methodPost, []string{"name", "upload"})
	if err != nil {
		b.Fatalf("prepare multipart benchmark: %v", err)
	}
	if body != nil {
		releaseBodyBuffer(body)
	}
	c, params := bindTestBenchContext(reflect.TypeFor[multipart.FileHeader](), "upload")
	b.Cleanup(func() {
		releasePooledForm(form)
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	})
	b.ReportAllocs()
	b.ResetTimer()
	b.StartTimer()
	for i := 0; i < b.N; i++ {
		bindTestBenchCheck(b, c, params, form.urlValues(), files, nil, jsonMedia)
		if params[0].Interface().(multipart.FileHeader).Filename != "bench.txt" {
			b.Fatalf("bound file = %#v", params[0].Interface())
		}
	}
	b.StopTimer()
}
