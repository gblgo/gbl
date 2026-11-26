// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"bytes"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const requestTestMultipartBoundary = "requestTestMultipartBoundary"

type requestTestLogger struct{}

func (requestTestLogger) Info(string, ...any)  {}
func (requestTestLogger) Warn(string, ...any)  {}
func (requestTestLogger) Error(string, ...any) {}

type requestTestUpload struct {
	field    string
	filename string
	body     []byte
}

type requestTestValidated struct {
	Name string
}

var requestTestValidationError = errors.New("requestTest name is required")

func (v *requestTestValidated) Validate() error {
	if v.Name == "" {
		return requestTestValidationError
	}
	return nil
}

type requestTestReadCounter struct {
	reader io.Reader
	reads  int
}

func (r *requestTestReadCounter) Read(p []byte) (int, error) {
	r.reads++
	return r.reader.Read(p)
}

func (*requestTestReadCounter) Close() error { return nil }

func requestTestEngine(t *testing.T, method, path string, fn any, names []string, middleware ...HandlerFunc) *engine {
	t.Helper()
	e := newEngine()
	e.logger = requestTestLogger{}
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
	chain := make([]HandlerFunc, 0, len(middleware)+1)
	chain = append(chain, middleware...)
	chain = append(chain, routeEndpoint)
	rh := &routeHandler{
		v:            v,
		t:            typ,
		paramNames:   paramNames,
		plan:         plan,
		handlers:     chain,
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

func requestTestLimits(t *testing.T, body, memory, upload int64) {
	t.Helper()
	oldBody, oldMemory, oldUpload := maxBodySize.Load(), maxFileMemory.Load(), maxUploadFileSize.Load()
	maxBodySize.Store(body)
	maxFileMemory.Store(memory)
	maxUploadFileSize.Store(upload)
	t.Cleanup(func() {
		maxBodySize.Store(oldBody)
		maxFileMemory.Store(oldMemory)
		maxUploadFileSize.Store(oldUpload)
	})
}

func requestTestParse(t *testing.T, r *http.Request, keys []string) (url.Values, filesMap, *bytes.Buffer, bool, error) {
	t.Helper()
	form, files, body, jsonMedia, err := parseRequestParameters(httptest.NewRecorder(), r, methodPost, keys)
	t.Cleanup(func() {
		if body != nil {
			releaseBodyBuffer(body)
		}
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	})
	return form, files, body, jsonMedia, err
}

func requestTestMultipartBody(tb testing.TB, fields url.Values, uploads ...requestTestUpload) []byte {
	tb.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.SetBoundary(requestTestMultipartBoundary); err != nil {
		tb.Fatalf("set deterministic multipart boundary: %v", err)
	}
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		for _, value := range fields[key] {
			if err := writer.WriteField(key, value); err != nil {
				tb.Fatalf("write multipart field %q: %v", key, err)
			}
		}
	}
	for _, upload := range uploads {
		part, err := writer.CreateFormFile(upload.field, upload.filename)
		if err != nil {
			tb.Fatalf("create multipart file %q: %v", upload.filename, err)
		}
		if _, err := part.Write(upload.body); err != nil {
			tb.Fatalf("write multipart file %q: %v", upload.filename, err)
		}
	}
	if err := writer.Close(); err != nil {
		tb.Fatalf("close multipart writer: %v", err)
	}
	return body.Bytes()
}

func requestTestNewMultipart(tb testing.TB, target string, fields url.Values, uploads ...requestTestUpload) (*http.Request, []byte) {
	tb.Helper()
	body := requestTestMultipartBody(tb, fields, uploads...)
	r := httptest.NewRequest(http.MethodPost, target, bytes.NewReader(body))
	r.Header.Set(contentType, multipartFormData+"; boundary="+requestTestMultipartBoundary)
	return r, body
}

func requestTestReadHeader(tb testing.TB, header *multipart.FileHeader) ([]byte, string, error) {
	tb.Helper()
	file, err := header.Open()
	if err != nil {
		return nil, "", err
	}
	content, readErr := io.ReadAll(file)
	closeErr := file.Close()
	if readErr != nil {
		return nil, "", readErr
	}
	if closeErr != nil {
		return nil, "", closeErr
	}
	var name string
	if named, ok := file.(interface{ Name() string }); ok {
		name = named.Name()
	}
	return content, name, nil
}

func requestTestIsolateTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	// os.TempDir consults TMPDIR on Unix and TMP/TEMP on Windows. Point all
	// three at an isolated directory so leaked multipart files are observable.
	t.Setenv("TMPDIR", dir)
	t.Setenv("TMP", dir)
	t.Setenv("TEMP", dir)
	return dir
}

func requestTestAssertTempDirEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read isolated temp directory: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("temporary multipart files still exist after cleanup: %v", names)
	}
}

func TestRequestURLEncodedFormAndPostForm(t *testing.T) {
	for _, mediaType := range []string{"", xWwwFormUrlencoded, xWwwFormUrlencoded + "; charset=utf-8"} {
		t.Run(mediaType, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/form?name=query&queryOnly=yes", strings.NewReader("name=body&bodyOnly=yes&tag=a&tag=b"))
			if mediaType != "" {
				r.Header.Set(contentType, mediaType)
			}
			form, files, body, jsonMedia, err := requestTestParse(t, r, []string{"name", "queryOnly", "bodyOnly", "tag"})
			if err != nil {
				t.Fatalf("parse urlencoded body: %v", err)
			}
			if files != nil || body != nil || jsonMedia {
				t.Fatalf("urlencoded parse returned files=%v body=%v json=%v", files, body, jsonMedia)
			}
			wantForm := url.Values{
				"name":      {"body"},
				"queryOnly": {"yes"},
				"bodyOnly":  {"yes"},
				"tag":       {"a", "b"},
			}
			wantPost := url.Values{
				"name":     {"body"},
				"bodyOnly": {"yes"},
				"tag":      {"a", "b"},
			}
			if !reflect.DeepEqual(form, wantForm) {
				t.Fatalf("merged form = %#v, want %#v", form, wantForm)
			}
			if !reflect.DeepEqual(r.PostForm, wantPost) {
				t.Fatalf("Request.PostForm = %#v, want %#v", r.PostForm, wantPost)
			}
			if !reflect.DeepEqual(r.Form, wantForm) {
				t.Fatalf("Request.Form = %#v, want merged body-wins form %#v", r.Form, wantForm)
			}
		})
	}
}

func TestRequestMultipartFormAndPostForm(t *testing.T) {
	r, _ := requestTestNewMultipart(t, "/form?name=query&queryOnly=yes", url.Values{
		"name":     {"body"},
		"bodyOnly": {"yes"},
		"tag":      {"a", "b"},
	}, requestTestUpload{field: "upload", filename: "a.txt", body: []byte("abc")})
	form, files, body, jsonMedia, err := requestTestParse(t, r, []string{"name", "queryOnly", "bodyOnly", "tag", "upload"})
	if err != nil {
		t.Fatalf("parse multipart body: %v", err)
	}
	if body != nil || jsonMedia {
		t.Fatalf("multipart parse returned body=%v json=%v", body, jsonMedia)
	}
	wantForm := url.Values{
		"name":      {"body"},
		"queryOnly": {"yes"},
		"bodyOnly":  {"yes"},
		"tag":       {"a", "b"},
	}
	wantPost := url.Values{
		"name":     {"body"},
		"bodyOnly": {"yes"},
		"tag":      {"a", "b"},
	}
	if !reflect.DeepEqual(form, wantForm) {
		t.Fatalf("merged form = %#v, want %#v", form, wantForm)
	}
	if !reflect.DeepEqual(r.PostForm, wantPost) {
		t.Fatalf("Request.PostForm = %#v, want %#v", r.PostForm, wantPost)
	}
	if !reflect.DeepEqual(r.Form, wantForm) {
		t.Fatalf("Request.Form = %#v, want the same body-wins view as binding %#v", r.Form, wantForm)
	}
	headers := files["upload"]
	if len(headers) != 1 || headers[0].Filename != "a.txt" || headers[0].Size != 3 {
		t.Fatalf("multipart files = %#v, want one a.txt of size 3", headers)
	}
}

func TestRequestFormMapsDoNotAliasWithoutQuery(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/form", strings.NewReader("name=body&tag=a&tag=b"))
	r.Header.Set(contentType, xWwwFormUrlencoded)
	_, _, _, _, err := requestTestParse(t, r, []string{"name", "tag"})
	if err != nil {
		t.Fatalf("parse urlencoded body: %v", err)
	}
	if r.Form == nil || r.PostForm == nil {
		t.Fatalf("Request.Form/PostForm must both be populated: form=%#v post=%#v", r.Form, r.PostForm)
	}
	r.Form.Set("formOnly", "value")
	if got := r.PostForm.Get("formOnly"); got != "" {
		t.Fatalf("mutating Request.Form changed Request.PostForm to %q; maps must not alias", got)
	}
	r.PostForm.Set("postOnly", "value")
	if got := r.Form.Get("postOnly"); got != "" {
		t.Fatalf("mutating Request.PostForm changed Request.Form to %q; maps must not alias", got)
	}
	r.Form["tag"][0] = "form"
	if got := r.PostForm["tag"][0]; got != "a" {
		t.Fatalf("mutating Request.Form value slice changed Request.PostForm to %q", got)
	}
	r.PostForm["tag"][1] = "post"
	if got := r.Form["tag"][1]; got != "b" {
		t.Fatalf("mutating Request.PostForm value slice changed Request.Form to %q", got)
	}
}

func TestRequestQuerySelectiveParsing(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/query", nil)
	r.URL.RawQuery = "id=1&id=2&name=a+b&na%6De=percent&empty=&flag&bad=%zz&semi=1;x=2&other=9&ID=wrong&=novalue&id=3"
	keys := []string{"id", "name", "empty", "flag", "bad", "semi"}
	got := queryValuesFor(r, keys)
	want := url.Values{
		"id":    {"1", "2", "3"},
		"name":  {"a b", "percent"},
		"empty": {""},
		"flag":  {""},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("selected query = %#v, want %#v", got, want)
	}
	if got := queryValuesFor(httptest.NewRequest(http.MethodGet, "/query", nil), keys); got != nil {
		t.Fatalf("query without RawQuery = %#v, want nil", got)
	}
	if got := queryValuesFor(r, nil); got != nil {
		t.Fatalf("query without selected keys = %#v, want nil", got)
	}
}

func TestRequestBodyMethodPolicy(t *testing.T) {
	requestTestLimits(t, 16, 8, 128)
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodDelete, http.MethodOptions} {
		t.Run(method+" ignores body", func(t *testing.T) {
			counter := &requestTestReadCounter{reader: strings.NewReader(strings.Repeat("x", 128))}
			r := httptest.NewRequest(method, "/body?id=7", counter)
			r.Header.Set(contentType, applicationJson)
			form, files, body, jsonMedia, err := parseRequestParameters(httptest.NewRecorder(), r, requestTestMethodOf(t, method), []string{"id"})
			if err != nil {
				t.Fatalf("parse non-body method: %v", err)
			}
			if counter.reads != 0 {
				t.Fatalf("body reads = %d, want 0 for %s", counter.reads, method)
			}
			if form.Get("id") != "7" || files != nil || body != nil || jsonMedia {
				t.Fatalf("non-body parse = form %#v, files %#v, body %v, json %v", form, files, body, jsonMedia)
			}
		})
	}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch} {
		t.Run(method+" reads body", func(t *testing.T) {
			counter := &requestTestReadCounter{reader: strings.NewReader("id=7")}
			r := httptest.NewRequest(method, "/body", counter)
			r.Header.Set(contentType, xWwwFormUrlencoded)
			form, _, _, _, err := parseRequestParameters(httptest.NewRecorder(), r, requestTestMethodOf(t, method), []string{"id"})
			if err != nil {
				t.Fatalf("parse body method: %v", err)
			}
			if counter.reads == 0 || form.Get("id") != "7" {
				t.Fatalf("body method parse reads=%d form=%#v", counter.reads, form)
			}
		})
	}
}

func requestTestMethodOf(t *testing.T, method string) httpMethod {
	t.Helper()
	m, ok := methodOf(method)
	if !ok {
		t.Fatalf("unsupported method %q", method)
	}
	return m
}

func TestRequestJSONMediaTypesAndEmptyBody(t *testing.T) {
	for _, mediaType := range []string{
		applicationJson,
		applicationJson + "; charset=utf-8",
		"application/vnd.api+json",
		"application/problem+json; charset=utf-8",
	} {
		t.Run(mediaType, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/json", strings.NewReader(`{"id":7}`))
			r.Header.Set(contentType, mediaType)
			form, files, body, jsonMedia, err := requestTestParse(t, r, []string{"id"})
			if err != nil {
				t.Fatalf("parse JSON media type: %v", err)
			}
			if !jsonMedia || body == nil || string(body.Bytes()) != `{"id":7}` || form != nil || files != nil {
				t.Fatalf("JSON parse = form %#v files %#v body %v json %v", form, files, body, jsonMedia)
			}
		})
	}
	for _, body := range []string{"", " \t\r\n"} {
		t.Run("empty "+body, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/json", strings.NewReader(body))
			r.Header.Set(contentType, applicationJson)
			_, _, _, _, err := requestTestParse(t, r, nil)
			if err == nil || !strings.Contains(err.Error(), "JSON body is empty") {
				t.Fatalf("empty JSON error = %v", err)
			}
			if errors.Is(err, requestBodyTooLargeError) {
				t.Fatalf("empty JSON misclassified as too large: %v", err)
			}
		})
	}
}

func TestRequestBodyLimit(t *testing.T) {
	const limit = 64
	requestTestLimits(t, limit, 8, limit)

	jsonPrefix, jsonSuffix := `{"value":"`, `"}`
	for _, mediaType := range []string{applicationJson, xWwwFormUrlencoded} {
		t.Run(mediaType, func(t *testing.T) {
			for _, size := range []int{limit - 1, limit, limit + 1} {
				t.Run("size "+strconv.Itoa(size), func(t *testing.T) {
					payload := strings.Repeat("x", size)
					if mediaType == applicationJson {
						payload = jsonPrefix + strings.Repeat("x", size-len(jsonPrefix)-len(jsonSuffix)) + jsonSuffix
					}
					r := httptest.NewRequest(http.MethodPost, "/limit", strings.NewReader(payload))
					r.Header.Set(contentType, mediaType)
					_, _, _, _, err := requestTestParse(t, r, nil)
					if size <= limit && err != nil {
						t.Fatalf("body at or below limit rejected: %v", err)
					}
					if size > limit && !errors.Is(err, requestBodyTooLargeError) {
						t.Fatalf("oversize body error = %v, want requestBodyTooLargeError", err)
					}
				})
			}
		})
	}

	t.Run("unknown content length", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/limit", strings.NewReader(strings.Repeat("x", limit+1)))
		r.Header.Set(contentType, xWwwFormUrlencoded)
		r.ContentLength = -1
		_, _, _, _, err := requestTestParse(t, r, nil)
		if !errors.Is(err, requestBodyTooLargeError) {
			t.Fatalf("unknown-length oversize body error = %v", err)
		}
	})

	t.Run("multipart total body", func(t *testing.T) {
		r, payload := requestTestNewMultipart(t, "/limit", nil, requestTestUpload{field: "upload", filename: "a.txt", body: []byte("abc")})
		maxBodySize.Store(int64(len(payload)))
		_, _, _, _, err := requestTestParse(t, r, nil)
		if err != nil {
			t.Fatalf("multipart body exactly at limit rejected: %v", err)
		}

		r, _ = requestTestNewMultipart(t, "/limit", nil, requestTestUpload{field: "upload", filename: "a.txt", body: []byte("abc")})
		maxBodySize.Store(int64(len(payload) - 1))
		_, _, _, _, err = requestTestParse(t, r, nil)
		if !errors.Is(err, requestBodyTooLargeError) {
			t.Fatalf("multipart body over limit error = %v", err)
		}
	})
}

func TestRequestUnsupportedMediaHonorsBodyLimit(t *testing.T) {
	requestTestLimits(t, 32, 8, 128)
	r := httptest.NewRequest(http.MethodPost, "/raw?name=query", strings.NewReader(strings.Repeat("x", 33)))
	r.Header.Set(contentType, "text/plain")
	r.ContentLength = -1
	form, files, body, jsonMedia, err := requestTestParse(t, r, []string{"name"})
	if !errors.Is(err, requestBodyTooLargeError) {
		t.Fatalf("unsupported-media oversize error = %v, want requestBodyTooLargeError (form=%#v files=%#v body=%v json=%v)", err, form, files, body, jsonMedia)
	}
}

func TestRequestRawUnknownBodyEnforcesLimitWhileStreaming(t *testing.T) {
	requestTestLimits(t, 32, 8, 128)
	called := false
	var readErr error
	e := requestTestEngine(t, http.MethodPost, "/raw", func(r *http.Request) string {
		called = true
		_, readErr = io.ReadAll(r.Body)
		return "ok"
	}, []string{"r"})
	r := httptest.NewRequest(http.MethodPost, "/raw", strings.NewReader(strings.Repeat("x", 33)))
	r.Header.Set(contentType, applicationJson)
	r.ContentLength = -1
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, r)
	if recorder.Code != http.StatusOK {
		t.Fatalf("raw request response = %d %q, want 200", recorder.Code, recorder.Body.String())
	}
	if !called {
		t.Fatal("handler did not run for an unknown-length raw request")
	}
	var maxBytesErr *http.MaxBytesError
	if !errors.As(readErr, &maxBytesErr) {
		t.Fatalf("raw body read error = %v, want MaxBytesError", readErr)
	}
}

func TestRequestMultipartMemoryThresholdAndCleanup(t *testing.T) {
	requestTestLimits(t, 4096, 1, 128)
	tempDir := requestTestIsolateTempDir(t)
	content := []byte("file content that must spill to disk")
	r, _ := requestTestNewMultipart(t, "/upload", nil, requestTestUpload{field: "upload", filename: "spill.txt", body: content})
	_, files, _, _, err := requestTestParse(t, r, nil)
	if err != nil {
		t.Fatalf("parse multipart below hard limits: %v", err)
	}
	headers := files["upload"]
	if len(headers) != 1 || headers[0].Size != int64(len(content)) {
		t.Fatalf("file headers = %#v, want one file of size %d", headers, len(content))
	}
	got, name, err := requestTestReadHeader(t, headers[0])
	if err != nil {
		t.Fatalf("read spilled file: %v", err)
	}
	if !bytes.Equal(got, content) || name == "" {
		t.Fatalf("spilled file content=%q temp path=%q", got, name)
	}
	if err := r.MultipartForm.RemoveAll(); err != nil {
		t.Fatalf("remove multipart temp files: %v", err)
	}
	requestTestAssertTempDirEmpty(t, tempDir)
}

func TestRequestMultipartFileLimitCleansSpilledFiles(t *testing.T) {
	t.Run("at limit", func(t *testing.T) {
		requestTestLimits(t, 4096, 1, 8)
		tempDir := requestTestIsolateTempDir(t)
		r, _ := requestTestNewMultipart(t, "/upload", nil, requestTestUpload{field: "upload", filename: "exact.txt", body: []byte("12345678")})
		_, files, _, _, err := requestTestParse(t, r, nil)
		if err != nil {
			t.Fatalf("file exactly at limit rejected: %v", err)
		}
		if headers := files["upload"]; len(headers) != 1 || headers[0].Size != 8 {
			t.Fatalf("file headers = %#v, want one file of size 8", headers)
		}
		if err := r.MultipartForm.RemoveAll(); err != nil {
			t.Fatalf("remove multipart temp files: %v", err)
		}
		requestTestAssertTempDirEmpty(t, tempDir)
	})

	t.Run("over limit", func(t *testing.T) {
		requestTestLimits(t, 4096, 1, 8)
		tempDir := requestTestIsolateTempDir(t)
		r, _ := requestTestNewMultipart(t, "/upload", nil, requestTestUpload{field: "upload", filename: "large.txt", body: []byte("123456789")})
		t.Cleanup(func() {
			if r.MultipartForm != nil {
				_ = r.MultipartForm.RemoveAll()
			}
		})
		_, _, _, _, err := requestTestParse(t, r, nil)
		if !errors.Is(err, requestBodyTooLargeError) {
			t.Fatalf("per-file oversize error = %v, want requestBodyTooLargeError", err)
		}
		requestTestAssertTempDirEmpty(t, tempDir)
	})
}

func TestRequestMultipartPipelineShapesAndCleanup(t *testing.T) {
	requestTestLimits(t, 4096, 1, 128)
	tempDir := requestTestIsolateTempDir(t)
	contentA, contentB := []byte("first file content"), []byte("second file content")
	var (
		valueCount, ptrCount int
		firstValue, firstPtr []byte
		pointerContents      [][]byte
		valueContents        [][]byte
		note                 string
		readErr              error
	)
	e := requestTestEngine(t, http.MethodPost, "/upload", func(
		value multipart.FileHeader,
		pointer *multipart.FileHeader,
		pointers []*multipart.FileHeader,
		values []multipart.FileHeader,
		noteValue string,
	) string {
		note = noteValue
		firstValue, _, readErr = requestTestReadHeader(t, &value)
		if readErr != nil {
			return "read failed"
		}
		if pointer != nil {
			firstPtr, _, readErr = requestTestReadHeader(t, pointer)
		}
		ptrCount = len(pointers)
		for _, header := range pointers {
			content, _, err := requestTestReadHeader(t, header)
			if err != nil {
				readErr = err
			}
			pointerContents = append(pointerContents, content)
		}
		valueCount = len(values)
		for i := range values {
			content, _, err := requestTestReadHeader(t, &values[i])
			if err != nil {
				readErr = err
			}
			valueContents = append(valueContents, content)
		}
		entries, err := os.ReadDir(tempDir)
		if err != nil {
			readErr = err
		} else if len(entries) == 0 {
			readErr = errors.New("small aggregate memory threshold did not spill any file to disk")
		}
		return "ok"
	}, []string{"value", "pointer", "pointers", "values", "noteValue"})
	r, _ := requestTestNewMultipart(t, "/upload?noteValue=query", url.Values{"noteValue": {"body"}},
		requestTestUpload{field: "value", filename: "a.txt", body: contentA},
		requestTestUpload{field: "value", filename: "b.txt", body: contentB},
		requestTestUpload{field: "pointer", filename: "p.txt", body: contentA},
		requestTestUpload{field: "pointers", filename: "ps1.txt", body: contentA},
		requestTestUpload{field: "pointers", filename: "ps2.txt", body: contentB},
		requestTestUpload{field: "values", filename: "vs1.txt", body: contentA},
		requestTestUpload{field: "values", filename: "vs2.txt", body: contentB},
	)
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, r)
	if readErr != nil {
		t.Fatalf("read bound file: %v", readErr)
	}
	if recorder.Code != http.StatusOK || recorder.Body.String() != "ok" {
		t.Fatalf("response = %d %q, want 200 ok", recorder.Code, recorder.Body.String())
	}
	if !bytes.Equal(firstValue, contentA) || !bytes.Equal(firstPtr, contentA) {
		t.Fatalf("single file contents = %q and %q, want first upload", firstValue, firstPtr)
	}
	wantSlices := [][]byte{contentA, contentB}
	if ptrCount != 2 || valueCount != 2 || !reflect.DeepEqual(pointerContents, wantSlices) || !reflect.DeepEqual(valueContents, wantSlices) {
		t.Fatalf("slice shapes: pointers=%d %q values=%d %q", ptrCount, pointerContents, valueCount, valueContents)
	}
	if note != "body" {
		t.Fatalf("multipart body field = %q, want body to win over query", note)
	}
	requestTestAssertTempDirEmpty(t, tempDir)
}

func TestRequestMultipartCleanupAfterContextRequestReplacement(t *testing.T) {
	requestTestLimits(t, 4096, 1, 128)
	tempDir := requestTestIsolateTempDir(t)
	e := requestTestEngine(t, http.MethodPost, "/upload", func(value *multipart.FileHeader) string {
		return value.Filename
	}, []string{"value"}, func(c *Context) {
		c.Next()
		// Middleware is allowed to replace the public request. Cleanup must still
		// retain and clean the request whose multipart body the framework parsed.
		c.Request = httptest.NewRequest(http.MethodGet, "/replacement", nil)
	})
	r, _ := requestTestNewMultipart(t, "/upload", nil, requestTestUpload{field: "value", filename: "original.txt", body: []byte("content that spills")})
	t.Cleanup(func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	})
	recorder := httptest.NewRecorder()
	e.ServeHTTP(recorder, r)
	if recorder.Code != http.StatusOK || recorder.Body.String() != "original.txt" {
		t.Fatalf("response = %d %q, want 200 original.txt", recorder.Code, recorder.Body.String())
	}
	requestTestAssertTempDirEmpty(t, tempDir)
}

func TestRequestParseErrorClassification(t *testing.T) {
	requestTestLimits(t, 128, 8, 128)
	tests := []struct {
		name        string
		contentType string
		body        string
	}{
		{name: "invalid content type", contentType: "application/json; charset", body: `{}`},
		{name: "malformed urlencoded", contentType: xWwwFormUrlencoded, body: "value=%zz"},
		{name: "missing multipart boundary", contentType: multipartFormData, body: "not multipart"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/parse", strings.NewReader(tt.body))
			r.Header.Set(contentType, tt.contentType)
			_, _, _, _, err := requestTestParse(t, r, nil)
			if err == nil {
				t.Fatal("parseRequestParameters() succeeded, want an error")
			}
			if errors.Is(err, requestBodyTooLargeError) {
				t.Fatalf("malformed request misclassified as too large: %v", err)
			}
		})
	}
}

func TestRequestHTTPErrorClassification(t *testing.T) {
	requestTestLimits(t, 64, 8, 64)
	tests := []struct {
		name        string
		contentType string
		body        string
		fn          any
		names       []string
		wantStatus  int
		wantBody    string
	}{
		{
			name: "invalid content type is a parse error", contentType: "application/json; charset", body: `{}`,
			fn: func(id int) string { return "ok" }, names: []string{"id"},
			wantStatus: http.StatusBadRequest, wantBody: "invalid Content-Type",
		},
		{
			name: "empty JSON is a parse error", contentType: applicationJson, body: " \n",
			fn: func(id int) string { return "ok" }, names: []string{"id"},
			wantStatus: http.StatusBadRequest, wantBody: "JSON body is empty",
		},
		{
			name: "malformed form is a parse error", contentType: xWwwFormUrlencoded, body: "id=%zz",
			fn: func(id int) string { return "ok" }, names: []string{"id"},
			wantStatus: http.StatusBadRequest, wantBody: "invalid URL escape",
		},
		{
			name: "scalar conversion is a generic binding error", contentType: xWwwFormUrlencoded, body: "id=secret-invalid-number",
			fn: func(id int) string { return "ok" }, names: []string{"id"},
			wantStatus: http.StatusBadRequest, wantBody: http.StatusText(http.StatusBadRequest),
		},
		{
			name: "JSON type is a generic binding error", contentType: applicationJson, body: `{"id":"secret-invalid-type"}`,
			fn: func(id int, name string) string { return "ok" }, names: []string{"id", "name"},
			wantStatus: http.StatusBadRequest, wantBody: http.StatusText(http.StatusBadRequest),
		},
		{
			name: "oversize body is 413", contentType: xWwwFormUrlencoded, body: strings.Repeat("x", 65),
			fn: func(id int) string { return "ok" }, names: []string{"id"},
			wantStatus: http.StatusRequestEntityTooLarge, wantBody: http.StatusText(http.StatusRequestEntityTooLarge),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := requestTestEngine(t, http.MethodPost, "/errors", tt.fn, tt.names)
			r := httptest.NewRequest(http.MethodPost, "/errors", strings.NewReader(tt.body))
			r.Header.Set(contentType, tt.contentType)
			recorder := httptest.NewRecorder()
			e.ServeHTTP(recorder, r)
			if recorder.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body=%q", recorder.Code, tt.wantStatus, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), tt.wantBody) {
				t.Fatalf("body = %q, want it to contain %q", recorder.Body.String(), tt.wantBody)
			}
			if strings.Contains(tt.name, "binding error") && strings.Contains(recorder.Body.String(), "secret-") {
				t.Fatalf("binding error leaked internals: %q", recorder.Body.String())
			}
		})
	}

	t.Run("validation error keeps application message", func(t *testing.T) {
		e := requestTestEngine(t, http.MethodPost, "/validate", func(value requestTestValidated) string {
			t.Fatal("handler ran after validation failed")
			return ""
		}, nil)
		r := httptest.NewRequest(http.MethodPost, "/validate", strings.NewReader(""))
		r.Header.Set(contentType, xWwwFormUrlencoded)
		recorder := httptest.NewRecorder()
		e.ServeHTTP(recorder, r)
		if recorder.Code != http.StatusBadRequest || recorder.Body.String() != requestTestValidationError.Error() {
			t.Fatalf("validation response = %d %q, want 400 %q", recorder.Code, recorder.Body.String(), requestTestValidationError)
		}
	})
}

func TestRequestBodyLimitFallback(t *testing.T) {
	requestTestLimits(t, 0, 0, 0)
	payload := strings.Repeat("x", defaultMaxBodySize+1)
	r := httptest.NewRequest(http.MethodPost, "/limit", strings.NewReader(payload))
	r.Header.Set(contentType, xWwwFormUrlencoded)
	_, _, _, _, err := requestTestParse(t, r, nil)
	if !errors.Is(err, requestBodyTooLargeError) {
		t.Fatalf("corrupt body limit fallback error = %v", err)
	}
}
