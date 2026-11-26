// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

const requestTestBenchQueryPayload = "id=42&name=alice&active=true&tags=go&tags=http&pair=1&pair=2&raw=hello" +
	"&noise0=0&noise1=1&noise2=2&noise3=3&noise4=4&noise5=5&noise6=6&noise7=7&noise8=8&noise9=9" +
	"&extra0=a&extra1=b&extra2=c&extra3=d&extra4=e&extra5=f&extra6=g&extra7=h&extra8=i&extra9=j"

// Request benchmarks isolate parsing. The request and payload are built once
// outside the timer; each iteration includes only body-reader replacement and
// parser-required buffer/temp-file cleanup. Handler binding and response writing
// are excluded, so these are not end-to-end HTTP costs. Scalar and struct use the
// exact same fixture and reset path; JSON and multipart reuse the binding
// benchmarks' payload construction helpers.
func requestTestBenchResetBody(r *http.Request, payload []byte) {
	r.Body = io.NopCloser(bytes.NewReader(payload))
	r.Form = nil
	r.PostForm = nil
	r.MultipartForm = nil
}

func requestTestBenchURLEncoded(b *testing.B, keys []string) {
	b.StopTimer()
	payload := []byte(bindTestBenchFormPayload)
	r := httptest.NewRequest(http.MethodPost, "/bench", bytes.NewReader(payload))
	r.Header.Set(contentType, xWwwFormUrlencoded)
	recorder := httptest.NewRecorder()
	b.ReportAllocs()
	b.ResetTimer()
	b.StartTimer()
	for i := 0; i < b.N; i++ {
		requestTestBenchResetBody(r, payload)
		form, files, body, jsonMedia, err := parseRequestParameters(recorder, r, methodPost, keys)
		if err != nil {
			b.Fatalf("parse urlencoded benchmark: %v", err)
		}
		if body != nil {
			releaseBodyBuffer(body)
		}
		if form.Get("id") != "42" || files != nil || jsonMedia {
			b.Fatalf("unexpected urlencoded parse result: form=%#v files=%#v json=%v", form, files, jsonMedia)
		}
	}
	b.StopTimer()
}

func BenchmarkRequestParseScalar(b *testing.B) {
	requestTestBenchURLEncoded(b, []string{"id"})
}

func BenchmarkRequestParseStruct(b *testing.B) {
	requestTestBenchURLEncoded(b, []string{"id", "name", "active", "tags", "pair", "raw"})
}

func BenchmarkRequestParseJSON(b *testing.B) {
	b.StopTimer()
	payload := []byte(bindTestBenchJSONPayload)
	r := httptest.NewRequest(http.MethodPost, "/bench", bytes.NewReader(payload))
	r.Header.Set(contentType, applicationJson)
	recorder := httptest.NewRecorder()
	b.ReportAllocs()
	b.ResetTimer()
	b.StartTimer()
	for i := 0; i < b.N; i++ {
		requestTestBenchResetBody(r, payload)
		form, files, body, jsonMedia, err := parseRequestParameters(recorder, r, methodPost, nil)
		if err != nil {
			b.Fatalf("parse JSON benchmark: %v", err)
		}
		if body == nil || !jsonMedia || form != nil || files != nil {
			b.Fatalf("unexpected JSON parse result: form=%#v files=%#v body=%v json=%v", form, files, body, jsonMedia)
		}
		releaseBodyBuffer(body)
	}
	b.StopTimer()
}

func BenchmarkRequestParseMultipart(b *testing.B) {
	b.StopTimer()
	fields := url.Values{"name": {"alice"}}
	upload := requestTestUpload{field: "upload", filename: "bench.txt", body: []byte("hello")}
	r, payload := requestTestNewMultipart(b, "/bench", fields, upload)
	recorder := httptest.NewRecorder()
	b.ReportAllocs()
	b.ResetTimer()
	b.StartTimer()
	for i := 0; i < b.N; i++ {
		requestTestBenchResetBody(r, payload)
		form, files, body, jsonMedia, err := parseRequestParameters(recorder, r, methodPost, []string{"name", "upload"})
		if err != nil {
			b.Fatalf("parse multipart benchmark: %v", err)
		}
		if body != nil {
			releaseBodyBuffer(body)
		}
		if form.Get("name") != "alice" || len(files["upload"]) != 1 || jsonMedia {
			b.Fatalf("unexpected multipart parse result: form=%#v files=%#v json=%v", form, files, jsonMedia)
		}
		if r.MultipartForm != nil {
			if err := r.MultipartForm.RemoveAll(); err != nil {
				b.Fatalf("clean multipart benchmark: %v", err)
			}
		}
	}
	b.StopTimer()
}

func BenchmarkRequestQuerySelective(b *testing.B) {
	r := httptest.NewRequest(http.MethodGet, "/bench?"+requestTestBenchQueryPayload, nil)
	keys := []string{"id", "name"}
	b.Run("full", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			values := queryValues(r)
			if values.Get("id") != "42" || len(values) <= len(keys) {
				b.Fatalf("unexpected full query result: %#v", values)
			}
		}
	})
	b.Run("selected", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			values := queryValuesFor(r, keys)
			if values.Get("id") != "42" || len(values) != len(keys) {
				b.Fatalf("unexpected selected query result: %#v", values)
			}
		}
	})
}
