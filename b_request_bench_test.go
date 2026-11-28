// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

const requestTestBenchQueryPayload = "id=42&name=alice&active=true&tags=go&tags=http&pair=1&pair=2&raw=hello" +
	"&noise0=0&noise1=1&noise2=2&noise3=3&noise4=4&noise5=5&noise6=6&noise7=7&noise8=8&noise9=9" +
	"&extra0=a&extra1=b&extra2=c&extra3=d&extra4=e&extra5=f&extra6=g&extra7=h&extra8=i&extra9=j"

// requestTestBenchBody reuses one reader so fixture setup stays outside measured allocations.
type requestTestBenchBody struct {
	*bytes.Reader
}

// Close implements io.Closer without releasing the reusable reader.
func (b *requestTestBenchBody) Close() error { return nil }

// requestTestBenchResetBody restores the reusable request body between iterations.
func requestTestBenchResetBody(r *http.Request, payload []byte, body *requestTestBenchBody) {
	body.Reset(payload)
	r.Body = body
	r.Form = nil
	r.PostForm = nil
	r.MultipartForm = nil
}

// BenchmarkFrameworkFormMergeInPlace measures only the framework's query/body merge.
func BenchmarkFrameworkFormMergeInPlace(b *testing.B) {
	body := url.Values{
		"name": {"body"}, "bodyOnly": {"yes"}, "tag": {"a", "b"},
	}
	query := url.Values{"queryOnly": {"yes"}}
	var sink url.Values
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		delete(body, "queryOnly")
		sink = mergeSelectedFormInPlace(query, body)
	}
	b.StopTimer()
	if sink.Get("name") != "body" || sink.Get("queryOnly") != "yes" {
		b.Fatalf("unexpected merged form: %#v", sink)
	}
}

// BenchmarkFrameworkFormSelectiveBody measures framework parsing without HTTP or ParseQuery.
func BenchmarkFrameworkFormSelectiveBody(b *testing.B) {
	payload := []byte(bindTestBenchFormPayload + strings.Repeat("&noise=ignored", 64))
	keys := newRequestKeySet([]string{"id", "name", "active", "tags", "pair", "raw"})
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for range b.N {
		form, err := formValuesForSet(payload, keys)
		if err != nil || form.urlValues().Get("id") != "42" || len(form.values) != len(keys) {
			releasePooledForm(form)
			b.Fatalf("unexpected selected form: (%#v, %v)", form, err)
		}
		releasePooledForm(form)
	}
}

// BenchmarkFrameworkQuerySelection measures only the framework's selective query parser.
func BenchmarkFrameworkQuerySelection(b *testing.B) {
	r := &http.Request{URL: &url.URL{RawQuery: requestTestBenchQueryPayload}}
	keys := newRequestKeySet([]string{"id", "name"})
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		form := acquirePooledForm(len(keys))
		form.parseQuery(r, keys)
		if form.values.Get("id") != "42" || len(form.values) != len(keys) {
			releasePooledForm(form)
			b.Fatalf("unexpected selected query: %#v", form.values)
		}
		releasePooledForm(form)
	}
}

// BenchmarkFrameworkJSONBodyBuffer measures only framework body buffering, not JSON decoding.
func BenchmarkFrameworkJSONBodyBuffer(b *testing.B) {
	payload := []byte(bindTestBenchJSONPayload)
	body := &requestTestBenchBody{Reader: bytes.NewReader(payload)}
	r := httptest.NewRequest(http.MethodPost, "/bench", body)
	r.Header.Set(contentType, applicationJson)
	recorder := httptest.NewRecorder()
	b.ReportAllocs()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for range b.N {
		requestTestBenchResetBody(r, payload, body)
		form, files, bodyBuf, jsonMedia, err := parseRequestParameters(recorder, r, methodPost, nil)
		if err != nil {
			b.Fatalf("buffer JSON body: %v", err)
		}
		if bodyBuf == nil || !jsonMedia || form != nil || files != nil {
			b.Fatalf("unexpected JSON buffer result: form=%#v files=%#v body=%v json=%v",
				form, files, bodyBuf, jsonMedia)
		}
		releaseBodyBuffer(bodyBuf)
	}
}
