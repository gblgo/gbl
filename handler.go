// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

type (
	// filesMap maps a multipart form field name to the headers of the files
	// uploaded under that name. It stays unexported on purpose: the binder
	// recognizes only the four multipart.FileHeader shapes as handler parameter
	// types, so this map is internal plumbing with no usable public role.
	filesMap map[string][]*multipart.FileHeader

	// ByteSize is a human-readable byte count used by the size setters: the
	// exported type name tells a caller, at the signature, that the value is bytes.
	ByteSize int

	// paramClass is the precomputed binding strategy for one handler parameter,
	// decided once at route-generation time so the request path never re-runs
	// reflection-based type classification.
	paramClass uint8

	// structField is one settable (exported) field of a struct parameter,
	// cached so bindStruct can skip reflect.Type field introspection per request.
	structField struct {
		index int
		name  string
		typ   reflect.Type
	}

	// bindPlan is a handler's immutable binding blueprint, built in generateRoute
	// from the handler signature. It carries the per-parameter type and class, the
	// business-parameter indexes, cached top-level struct fields, and whether the
	// request body/query must be parsed at all, so serveHTTP can skip work for
	// handlers that bind nothing.
	bindPlan struct {
		types           []reflect.Type
		classes         []paramClass
		structFields    [][]structField // parallel to types; non-nil only for struct business params
		businessIndexes []int
		needsParse      bool
		// needsNames is true when at least one business parameter must be matched
		// against the request by its handler parameter name — any scalar, or more
		// than one business argument (whose JSON members are split by name). A
		// lone struct/pointer-to-struct is self-describing: its fields come from
		// reflection, so it binds correctly even when source names are unavailable.
		needsNames bool
	}
)

// Parameter binding classes; the zero value classBind is the common case.
const (
	classBind    paramClass = iota // resolved from path/query/body by name
	classRequest                   // *http.Request, injected directly
	classWriter                    // http.ResponseWriter (or a satisfying interface)
	classFiles                     // one of the multipart.FileHeader shapes
	classContext                   // *Context, injected directly
)

var (
	requestBodyTooLargeError = errors.New("request body too large")
	multipleRequireError     = errors.New("multiple handler parameters require a JSON object")
	responseMismatch         = errors.New("response writer type mismatch")
	maxBodySize              atomic.Int64
	maxFileMemory            atomic.Int64
	maxUploadFileSize        atomic.Int64
	requestType              = reflect.TypeFor[*http.Request]()
	contextType              = reflect.TypeFor[*Context]()
	writerType               = reflect.TypeFor[http.ResponseWriter]()
	fileHeaderType           = reflect.TypeFor[multipart.FileHeader]()
	fileHeaderPtr            = reflect.TypeFor[*multipart.FileHeader]()
	fileHeaderPs             = reflect.TypeFor[[]*multipart.FileHeader]()
	fileHeaderSlice          = reflect.TypeFor[[]multipart.FileHeader]()
)

// Default limits applied at startup and re-applied defensively if a stored
// value has been corrupted into a non-positive number.
const (
	defaultMaxBodySize       = 5 << 20 // Maximum total request body size: 5 MiB.
	defaultMaxFileMemory     = 2 << 20 // Multipart in-memory threshold, not total upload limit.
	defaultMaxUploadFileSize = 1 << 20 // Hard cap on each individual uploaded file: 1 MiB.
)

func init() {
	maxBodySize.Store(defaultMaxBodySize)
	maxFileMemory.Store(defaultMaxFileMemory)
	maxUploadFileSize.Store(defaultMaxUploadFileSize)
}

// Per-request scratch reused across the whole process. The wrapper holds only
// plain fields and the map only strings, so both reset cheaply and carry no
// request state between uses.
var (
	responseWriterPool = sync.Pool{
		New: func() any { return &gblResponseWriter{} },
	}
	pathParamsPool = sync.Pool{
		New: func() any { return make(map[string]string, 4) },
	}
)

// recycle clears the wrapped ResponseWriter reference — so the pool never pins
// a finished request's writer — and returns the wrapper for reuse. A named
// method deferred directly (defer rw.recycle()) avoids the heap-allocated
// closure that defer func(){...}() would cost on every request.
func (rw *gblResponseWriter) recycle() {
	rw.w = nil
	responseWriterPool.Put(rw)
}

// releasePathParams empties and recycles a pooled path-parameter map.
func releasePathParams(params map[string]string) {
	clear(params)
	pathParamsPool.Put(params)
}

// SetMaxBodySize sets the hard limit on the total request body size, applied
// to every body method (POST, PUT, PATCH), multipart uploads included. A body
// over the limit is rejected with 413. Panics on a non-positive size.
func SetMaxBodySize(size ByteSize) {
	if size <= 0 {
		defaultEngine.failStartup("gbl: max body size must be greater than zero")
	}
	maxBodySize.Store(int64(size))
}

// SetMaxFileSize sets the multipart in-memory threshold: file parts smaller
// than size stay in memory, larger ones spill to temporary files. It does not
// limit the total upload size; see SetMaxBodySize for that. Panics on a
// non-positive size.
func SetMaxFileSize(size ByteSize) {
	if size <= 0 {
		defaultEngine.failStartup("gbl: multipart memory threshold must be greater than zero")
	}
	maxFileMemory.Store(int64(size))
}

// SetMaxUploadFileSize sets a hard, per-file cap on every uploaded file
// (any field, any part): a multipart file whose size exceeds the cap aborts
// the request with 413, before the handler runs. It defaults to
// defaultMaxUploadFileSize (1 MiB). Distinct from SetMaxFileSize, which is
// only the in-memory spill threshold, and from SetMaxBodySize, which caps the
// total body. Panics on a non-positive size.
func SetMaxUploadFileSize(size ByteSize) {
	if size <= 0 {
		defaultEngine.failStartup("gbl: max upload file size must be greater than zero")
	}
	maxUploadFileSize.Store(int64(size))
}

// serveHTTP is the full request pipeline: method check, route match, HEAD
// fallback to GET, then the route's frozen middleware chain, whose terminal
// endpoint runs the business handler; finally the staged result is written.
// Any panic raised below is recovered and rendered by the deferred
// catchRuntimePanic.
//
// The response wrapper, the path-parameter map and the Context are drawn from
// pools and returned on every exit path. The wrapper handed to middleware and
// handlers therefore must not be retained past the request.
func (e *engine) serveHTTP(w http.ResponseWriter, r *http.Request) {
	rw := responseWriterPool.Get().(*gblResponseWriter)
	*rw = gblResponseWriter{w: w}

	// Registered first so it runs last: the recover/deferred render below still
	// needs rw.w, and only afterward may the wrapper be cleared and recycled.
	defer rw.recycle()
	defer e.catchRuntimePanic(rw)

	method, known := methodOf(r.Method)
	if !known {
		// TRACE, CONNECT and custom verbs are not implemented at all, so this is
		// not a routing failure and 405 would be the wrong answer.
		abort(http.StatusNotImplemented, http.StatusText(http.StatusNotImplemented))
	}

	pathParams := pathParamsPool.Get().(map[string]string)
	defer releasePathParams(pathParams)

	rh := e.matchNode(method, r.URL.Path, pathParams)

	// An unregistered HEAD borrows the GET route: the handler runs normally and
	// net/http drops the body while keeping Content-Length.
	if rh == nil && method == methodHead {
		rh = e.matchNode(methodGet, r.URL.Path, pathParams)
	}

	if rh == nil {
		e.abortUnmatched(rw, method, r.URL.Path)
		return
	}

	c := newContext(rw, r, pathParams, rh, method, e.logger)
	defer c.recycle()

	// Multipart temp files, if any were parsed, are removed once the whole chain
	// has completed, whatever the outcome and including a panic unwinding it.
	defer cleanupMultipart(r)

	c.Next()

	// The chain has fully unwound. If the handler produced a result and nothing
	// wrote a response — the handler directly or a post-handler middleware —
	// render the staged body with the content type chosen at registration.
	//
	// Write through rw, not the raw w: middleware (or the framework) may have
	// already recorded a status via rw.WriteHeader, and that choice must survive.
	// rw.WriteHeader is a no-op once a status is recorded, so the 200 here only
	// applies as a default when nothing set one, and rw.Write flushes the recorded
	// status before the body.
	if c.handled && !rw.written() {
		rw.Header().Set(contentType, c.contentType)
		if !rw.headed() {
			rw.WriteHeader(http.StatusOK)
		}
		_, _ = rw.Write(c.bytes)
	}
}

// cleanupMultipart releases any temporary files a parsed multipart request left
// behind. It is deferred once per request and is a no-op unless the body was
// actually parsed as multipart, so removing files is not actionable here: the
// response is already decided and removal failures are silently dropped.
func cleanupMultipart(r *http.Request) {
	if r.MultipartForm != nil {
		_ = r.MultipartForm.RemoveAll()
	}
}

// abortUnmatched Answers a request that matched no route, telling "no such
// path" apart from "path exists, wrong verb".
func (e *engine) abortUnmatched(rw *gblResponseWriter, method httpMethod, path string) {
	allowed := e.allowFor(path)
	if len(allowed) == 0 {
		abort(http.StatusNotFound, http.StatusText(http.StatusNotFound))
	}

	rw.Header().Set(allow, strings.Join(allowed, headerComma))

	if method == methodOptions {
		// OPTIONS succeeded, and a 204 must not carry a body, so the recorded
		// status is flushed here instead of going through the panic path.
		rw.WriteHeader(http.StatusNoContent)
		rw.writeHeader()
		return
	}

	abort(http.StatusMethodNotAllowed, http.StatusText(http.StatusMethodNotAllowed))
}

// writeRequestError Maps a request-parsing failure to its status code and
// aborts the pipeline. Every error reaching here describes the client's own
// malformed request (bad Content-Type, unparseable body, oversize payload), so
// its message is safe to send back.
func writeRequestError(err error) {
	if errors.Is(err, requestBodyTooLargeError) {
		abort(http.StatusRequestEntityTooLarge, http.StatusText(http.StatusRequestEntityTooLarge))
	}
	abort(http.StatusBadRequest, err.Error())
}

// selfDescribing reports whether a business parameter can be bound without its
// handler parameter name. A struct — or any pointer chain leading to one — names
// its values by field, which reflection supplies even when the source file is
// unavailable at runtime; every other kind (scalar, slice, map, interface and
// pointer to those) is matched against the request by the parameter's own name.
func selfDescribing(t reflect.Type) bool {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.Kind() == reflect.Struct
}

// classifyParam decides a parameter's binding class once, at route-generation
// time. It mirrors the old per-request isSpecialParameter checks but runs the
// costlier interface-satisfaction probe (t.Implements) only during startup.
func classifyParam(t reflect.Type) paramClass {
	if t == contextType {
		return classContext
	}
	if t == requestType {
		return classRequest
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
// It records each parameter's type and class, collects the business-parameter
// indexes, caches the top-level fields of struct parameters, and flags whether
// any parameter forces the request body/query to be parsed.
func buildBindPlan(rt reflect.Type) bindPlan {
	n := rt.NumIn()
	plan := bindPlan{
		types:        make([]reflect.Type, n),
		classes:      make([]paramClass, n),
		structFields: make([][]structField, n),
	}
	for i := range n {
		t := rt.In(i)
		plan.types[i] = t

		class := classifyParam(t)
		plan.classes[i] = class
		switch class {
		case classBind:
			plan.businessIndexes = append(plan.businessIndexes, i)
			plan.needsParse = true
			if t.Kind() == reflect.Struct {
				plan.structFields[i] = precomputeStructFields(t)
			}
			if !selfDescribing(t) {
				plan.needsNames = true
			}
		case classFiles:
			// File headers arrive only through a parsed multipart body.
			plan.needsParse = true
		default:
			// classRequest, classWriter and classContext are injected directly and
			// never force the body/query to be parsed.
		}
	}
	// More than one business argument is name-dependent even when every one is a
	// struct: a JSON object body is distributed across them by parameter name.
	if len(plan.businessIndexes) > 1 {
		plan.needsNames = true
	}
	return plan
}

// precomputeStructFields lists a struct type's exported fields in declaration
// order. Every exported field of a freshly allocated value is settable, so the
// per-request CanSet/PkgPath checks in bindStruct collapse to this cached slice.
func precomputeStructFields(t reflect.Type) []structField {
	n := t.NumField()
	fields := make([]structField, 0, n)
	for i := range n {
		field := t.Field(i)
		if field.PkgPath != null {
			continue // unexported
		}
		fields = append(fields, structField{index: i, name: field.Name, typ: field.Type})
	}
	return fields
}

// injectedParameters builds the reflect argument list for a handler call,
// driven by the handler's precomputed bind plan. Each parameter is injected
// directly (request, writer, file headers) or bound by name from path
// parameters, a JSON body, or form values, in that precedence order. Type
// classification already happened in generateRoute, so nothing here re-inspects
// the signature with reflection.
func injectedParameters(
	c *Context,
	form url.Values,
	files filesMap,
	body []byte,
	mediaType string,
) ([]reflect.Value, error) {
	rh := c.rh
	plan := &rh.plan
	names := rh.paramNames
	params := make([]reflect.Value, len(plan.types))

	var jsonFields map[string]json.RawMessage
	isJSON := isJSONMediaType(mediaType) && hasNonWhitespace(body)
	if isJSON && len(plan.businessIndexes) > 1 {
		if err := json.Unmarshal(body, &jsonFields); err != nil {
			return nil, fmt.Errorf("invalid JSON body: %w", err)
		}
		if jsonFields == nil {
			return nil, multipleRequireError
		}
	}

	for i := range plan.types {
		t := plan.types[i]
		name := paramNameAt(names, i)

		switch plan.classes[i] {
		case classBind:
			// The common case: a business parameter resolved from path/query/
			// body/form by the code below this switch, so nothing to inject
			// here. Breaking out (no continue) reaches that resolution path.
		case classContext:
			params[i] = reflect.ValueOf(c)
			continue
		case classRequest:
			params[i] = reflect.ValueOf(c.Request)
			continue
		case classWriter:
			v, err := injectSpecialParameter(t, c.writer, c.Request, files, name)
			if err != nil {
				return nil, fmt.Errorf(injectParameterErrFormat, name, err)
			}
			params[i] = v
			continue
		case classFiles:
			headers, _ := lookupFiles(files, name)
			v, err := bindFileHeaders(t, headers)
			if err != nil {
				return nil, fmt.Errorf(injectParameterErrFormat, name, err)
			}
			params[i] = v
			continue
		}

		// classBind: a business parameter, resolved from request data. Path
		// parameters take priority over query/body for scalar arguments too.
		if pathValue, ok := lookupString(c.params, name); ok {
			v, err := bindTextValue(t, []string{pathValue})
			if err != nil {
				return nil, fmt.Errorf("path parameter %q: %w", name, err)
			}
			params[i] = v
			continue
		}

		if isJSON {
			if len(plan.businessIndexes) == 1 {
				v, err := decodeJSON(t, body)
				if err != nil {
					return nil, fmt.Errorf(jsonParameterErrFormat, name, err)
				}
				params[i] = v
				continue
			}

			raw, ok := lookupRawJSON(jsonFields, name)
			if !ok {
				return nil, fmt.Errorf("JSON field %q not found", name)
			}
			v, err := decodeJSON(t, raw)
			if err != nil {
				return nil, fmt.Errorf(jsonParameterErrFormat, name, err)
			}
			params[i] = v
			continue
		}

		var (
			v   reflect.Value
			err error
		)
		if fields := plan.structFields[i]; fields != nil {
			v, err = bindStructFields(t, fields, form, c.params, files)
		} else {
			v, err = bindFormParameter(t, name, form, c.params, files)
		}
		if err != nil {
			return nil, fmt.Errorf("parameter %q: %w", name, err)
		}
		params[i] = v
	}

	for i, v := range params {
		name := paramNameAt(names, i)
		if !v.IsValid() {
			return nil, fmt.Errorf("parameter %q was not initialized", name)
		}
		if !v.Type().AssignableTo(plan.types[i]) {
			return nil, fmt.Errorf("parameter %q type mismatch: got %s, want %s", name, v.Type(), plan.types[i])
		}
	}
	return params, nil
}

// paramNameAt Bounds-safe lookup of a handler parameter name.
//
// generateRoute pads the recovered names to exactly one per parameter, so this
// normally just reads the slice. The fallback keeps a name list of any length
// from turning into an index-out-of-range panic, which the recover path would
// otherwise hand back to the client as a 200 response.
func paramNameAt(names []string, index int) string {
	if index < len(names) {
		return names[index]
	}
	return unnamedParamName(index)
}

// hasNonWhitespace reports whether data holds at least one byte beyond
// spaces, tabs and newlines. The scan is allocation-free; TrimSpace would
// copy the whole (up to multi-MiB) body just to test emptiness.
func hasNonWhitespace(data []byte) bool {
	return bytes.ContainsFunc(data, func(r rune) bool {
		return r != ' ' && r != '\t' && r != '\n' && r != '\r'
	})
}

// isJSONMediaType reports whether a parsed media type is JSON — the exact
// application/json or a structured suffix such as application/vnd.api+json.
func isJSONMediaType(mediaType string) bool {
	return mediaType == applicationJson ||
		(strings.HasPrefix(mediaType, applicationNoJson) && strings.HasSuffix(mediaType, plusSignJson))
}

// lookupRawJSON finds a JSON field by exact name first, then case-insensitively,
// mirroring encoding/json's own case-insensitive field matching.
func lookupRawJSON(fields map[string]json.RawMessage, name string) (json.RawMessage, bool) {
	if raw, ok := fields[name]; ok {
		return raw, true
	}
	for key, raw := range fields {
		if strings.EqualFold(key, name) {
			return raw, true
		}
	}
	return nil, false
}

// lookupString finds a map value by exact key first, then case-insensitively.
func lookupString(values map[string]string, name string) (string, bool) {
	if value, ok := values[name]; ok {
		return value, true
	}
	for key, value := range values {
		if strings.EqualFold(key, name) {
			return value, true
		}
	}
	return null, false
}

// lookupValues finds url.Values entries by exact key first, then case-insensitively.
func lookupValues(values url.Values, name string) ([]string, bool) {
	if v, ok := values[name]; ok {
		return v, true
	}
	for key, v := range values {
		if strings.EqualFold(key, name) {
			return v, true
		}
	}
	return nil, false
}

// bindFormParameter binds one non-JSON parameter of any kind: pointers stay nil
// when their source is absent, structs recurse over their fields, file header
// types pull from the uploaded files, slices/arrays consume repeated values,
// and scalars take the first value.
func bindFormParameter(t reflect.Type, name string, form url.Values, pathParams map[string]string, files filesMap) (reflect.Value, error) {
	if t.Kind() == reflect.Pointer {
		// Missing pointer values remain nil instead of allocating an empty value.
		if _, ok := lookupString(pathParams, name); !ok {
			if _, ok = lookupValues(form, name); !ok {
				if _, ok = lookupFiles(files, name); !ok && t.Elem().Kind() != reflect.Struct {
					return reflect.Zero(t), nil
				}
			}
		}
		value, err := bindFormParameter(t.Elem(), name, form, pathParams, files)
		if err != nil {
			return reflect.Value{}, err
		}
		result := reflect.New(t.Elem())
		result.Elem().Set(value)
		return result, nil
	}

	if t.Kind() == reflect.Struct {
		return bindStruct(t, form, pathParams, files)
	}

	if isFileHeaderType(t) {
		headers, _ := lookupFiles(files, name)
		return bindFileHeaders(t, headers)
	}

	values, _ := lookupValues(form, name)
	switch t.Kind() {
	case reflect.Array:
		if len(values) == 0 {
			return reflect.Zero(t), nil
		}
		if len(values) != t.Len() {
			return reflect.Value{}, fmt.Errorf("array requires %d values, got %d", t.Len(), len(values))
		}
		result := reflect.New(t).Elem()
		for i, text := range values {
			elem, err := bindTextValue(t.Elem(), []string{text})
			if err != nil {
				return reflect.Value{}, fmt.Errorf("array index %d: %w", i, err)
			}
			result.Index(i).Set(elem)
		}
		return result, nil

	case reflect.Slice:
		// []byte in a form field is treated as a string's raw bytes.
		if t.Elem().Kind() == reflect.Uint8 {
			if len(values) == 0 {
				return reflect.Zero(t), nil
			}
			return reflect.ValueOf([]byte(values[0])).Convert(t), nil
		}
		result := reflect.MakeSlice(t, len(values), len(values))
		for i, text := range values {
			elem, err := bindTextValue(t.Elem(), []string{text})
			if err != nil {
				return reflect.Value{}, fmt.Errorf("slice index %d: %w", i, err)
			}
			result.Index(i).Set(elem)
		}
		return result, nil

	default:
		return bindTextValue(t, values)
	}
}

// bindStruct fills an exported-field struct from form and path values, matching
// field names case-insensitively without requiring any struct tags. It is the
// general entry used for nested and pointer-to-struct values; a handler's direct
// struct parameter goes through bindStructFields with a cached field list.
func bindStruct(t reflect.Type, form url.Values, pathParams map[string]string, files filesMap) (reflect.Value, error) {
	return bindStructFields(t, precomputeStructFields(t), form, pathParams, files)
}

// bindStructFields populates a fresh struct of type t from a precomputed field
// list, so the request path skips reflect.Type field introspection entirely. The
// per-field precedence matches bindStruct: a path value wins over form/query, and
// everything else falls through to bindFormParameter.
func bindStructFields(t reflect.Type, fields []structField, form url.Values, pathParams map[string]string, files filesMap) (reflect.Value, error) {
	result := reflect.New(t).Elem()

	for _, field := range fields {
		dst := result.Field(field.index)

		// No tags are required: Go field names are matched case-insensitively.
		// Path wins over query/form values.
		if pathValue, ok := lookupString(pathParams, field.name); ok {
			value, err := bindTextValue(field.typ, []string{pathValue})
			if err != nil {
				return reflect.Value{}, fmt.Errorf("path field %q: %w", field.name, err)
			}
			dst.Set(value)
			continue
		}

		value, err := bindFormParameter(field.typ, field.name, form, pathParams, files)
		if err != nil {
			return reflect.Value{}, fmt.Errorf("field %q: %w", field.name, err)
		}
		dst.Set(value)
	}
	return result, nil
}

// lookupFiles finds uploaded file headers by field name, exact match first,
// then case-insensitively.
func lookupFiles(files filesMap, name string) ([]*multipart.FileHeader, bool) {
	if v, ok := files[name]; ok {
		return v, true
	}
	for key, v := range files {
		if strings.EqualFold(key, name) {
			return v, true
		}
	}
	return nil, false
}

// isFileHeaderType reports whether t is one of the four accepted file
// parameter shapes: multipart.FileHeader, *multipart.FileHeader,
// []*multipart.FileHeader or []multipart.FileHeader.
func isFileHeaderType(t reflect.Type) bool {
	return t == fileHeaderType || t == fileHeaderPtr || t == fileHeaderPs || t == fileHeaderSlice
}

// bindFileHeaders converts the looked-up file headers into the exact shape the
// handler parameter asks for, taking the first upload for single-value types.
func bindFileHeaders(t reflect.Type, headers []*multipart.FileHeader) (reflect.Value, error) {
	switch t {
	case fileHeaderPtr:
		if len(headers) == 0 {
			return reflect.Zero(t), nil
		}
		return reflect.ValueOf(headers[0]), nil
	case fileHeaderType:
		if len(headers) == 0 {
			return reflect.Zero(t), nil
		}
		return reflect.ValueOf(*headers[0]), nil
	case fileHeaderPs:
		return reflect.ValueOf(headers), nil
	case fileHeaderSlice:
		result := make([]multipart.FileHeader, len(headers))
		for i, header := range headers {
			if header != nil {
				result[i] = *header
			}
		}
		return reflect.ValueOf(result), nil
	default:
		return reflect.Value{}, fmt.Errorf("unsupported file parameter type: %s", t)
	}
}

// decodeJSON unmarshals data into a fresh value of type t and returns the
// populated value (not a pointer to it).
func decodeJSON(t reflect.Type, data []byte) (reflect.Value, error) {
	if t == nil {
		return reflect.Value{}, errors.New("nil JSON target type")
	}
	result := reflect.New(t)
	if err := json.Unmarshal(data, result.Interface()); err != nil {
		return reflect.Value{}, err
	}
	return result.Elem(), nil
}

// injectSpecialParameter supplies the framework-owned arguments a handler can
// declare: *http.Request, any http.ResponseWriter implementation, and the file
// header types, which are matched by parameter name against the uploads.
func injectSpecialParameter(
	t reflect.Type,
	w http.ResponseWriter,
	r *http.Request,
	files filesMap,
	name string,
) (reflect.Value, error) {
	if t == requestType {
		return reflect.ValueOf(r), nil
	}

	if t == writerType || t.Implements(writerType) {
		value := reflect.ValueOf(w)
		if value.Type().AssignableTo(t) {
			return value, nil
		}
		if t.Kind() == reflect.Interface && value.Type().Implements(t) {
			return value, nil
		}
		return reflect.Value{}, responseMismatch
	}

	headers, _ := lookupFiles(files, name)
	return bindFileHeaders(t, headers)
}

// bindTextValue converts raw request strings into a scalar (or pointer to
// scalar) of type t. Only the first value is used; repeated values should be
// bound to a slice or array parameter instead.
func bindTextValue(t reflect.Type, values []string) (reflect.Value, error) {
	if t.Kind() == reflect.Pointer {
		if len(values) == 0 {
			return reflect.Zero(t), nil
		}
		value, err := bindTextValue(t.Elem(), values)
		if err != nil {
			return reflect.Value{}, err
		}
		result := reflect.New(t.Elem())
		result.Elem().Set(value)
		return result, nil
	}

	result := reflect.New(t).Elem()
	if len(values) == 0 {
		return result, nil
	}

	// Scalar parameters use the first value. Repeated values should use slices/arrays.
	value := values[0]
	switch t.Kind() {
	case reflect.String:
		result.SetString(value)
	case reflect.Bool:
		v, err := strconv.ParseBool(value)
		if err != nil {
			return reflect.Value{}, err
		}
		result.SetBool(v)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v, err := strconv.ParseInt(value, 10, t.Bits())
		if err != nil {
			return reflect.Value{}, err
		}
		result.SetInt(v)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		v, err := strconv.ParseUint(value, 10, t.Bits())
		if err != nil {
			return reflect.Value{}, err
		}
		result.SetUint(v)
	case reflect.Float32, reflect.Float64:
		v, err := strconv.ParseFloat(value, t.Bits())
		if err != nil {
			return reflect.Value{}, err
		}
		result.SetFloat(v)
	case reflect.Complex64, reflect.Complex128:
		v, err := strconv.ParseComplex(value, t.Bits())
		if err != nil {
			return reflect.Value{}, err
		}
		result.SetComplex(v)
	default:
		return reflect.Value{}, fmt.Errorf("unsupported parameter type: %s", t)
	}
	return result, nil
}

// parseRequestParameters collects handler-bindable inputs for the request:
// form values, uploaded files, the raw JSON body and the parsed media type.
func parseRequestParameters(w http.ResponseWriter, r *http.Request, method httpMethod) (url.Values, filesMap, []byte, string, error) {
	// GBL deliberately applies a strict parameter-source policy by HTTP method.
	// GET, HEAD, DELETE and OPTIONS may read only URL path parameters (matched by
	// the router) and URL query parameters. Their request bodies are never parsed
	// or read, regardless of Content-Type.
	//
	// Only POST, PUT and PATCH may carry a body GBL reads; every other method
	// exposes query parameters and leaves the body untouched.
	if !method.isBodyMethod() {
		// No query string means nothing to bind from the URL: returning a nil
		// form avoids the empty url.Values map r.URL.Query() would allocate, and
		// every form lookup already treats a nil map as "not found".
		if r.URL.RawQuery == null {
			return nil, nil, nil, null, nil
		}
		return r.URL.Query(), nil, nil, null, nil
	}

	var (
		form      url.Values
		files     filesMap
		body      []byte
		mediaType string
	)

	// Apply one hard limit to the entire request body, including multipart files.
	// The fallback re-applies the default if the stored value was corrupted.
	limit := maxBodySize.Load()
	if limit <= 0 {
		limit = defaultMaxBodySize
	}
	if r.Body != nil {
		r.Body = http.MaxBytesReader(w, r.Body, limit)
	}

	contentTypeHeader := r.Header.Get(contentType)
	if contentTypeHeader != null {
		var err error
		mediaType, _, err = mime.ParseMediaType(contentTypeHeader)
		if err != nil {
			return nil, nil, nil, null, fmt.Errorf("invalid Content-Type: %w", err)
		}
	}

	switch {
	case isJSONMediaType(mediaType):
		var err error
		if r.Body != nil {
			body, err = io.ReadAll(r.Body)
		}
		if err != nil {
			if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
				return nil, nil, nil, null, requestBodyTooLargeError
			}
			return nil, nil, nil, null, err
		}

	case mediaType == multipartFormData:
		memory := maxFileMemory.Load()
		if memory <= 0 {
			memory = defaultMaxFileMemory
		}
		if err := r.ParseMultipartForm(memory); err != nil {
			if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
				return nil, nil, nil, null, requestBodyTooLargeError
			}
			return nil, nil, nil, null, err
		}
		// r.PostForm contains both URL query values and multipart text fields.
		form = r.PostForm
		if r.MultipartForm != nil {
			files = r.MultipartForm.File
		}
		// Per-file hard cap: any uploaded part larger than the configured
		// threshold aborts the request before the handler runs. Re-applied
		// defensively if the stored value has been corrupted to a non-positive one.
		maxUpload := maxUploadFileSize.Load()
		if maxUpload <= 0 {
			maxUpload = defaultMaxUploadFileSize
		}
		for _, headers := range files {
			for _, hdr := range headers {
				if hdr != nil && hdr.Size > maxUpload {
					return nil, nil, nil, null, requestBodyTooLargeError
				}
			}
		}

	case mediaType == xWwwFormUrlencoded || mediaType == null:
		if err := r.ParseForm(); err != nil {
			if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
				return nil, nil, nil, null, requestBodyTooLargeError
			}
			return nil, nil, nil, null, err
		}
		// r.PostForm contains both URL query values and URL-encoded body fields.
		form = r.PostForm

	default:
		// Unsupported body media types are not interpreted as parameters.
		// Query parameters are still valid for POST and PUT.
		form = r.URL.Query()
	}

	return form, files, body, mediaType, nil
}

// convertToByteArray renders a handler return value as the response body:
// strings and byte slices pass through, scalars are formatted, composites are
// JSON-marshaled, and nil pointers/interfaces become an empty body.
func convertToByteArray(val reflect.Value) ([]byte, error) {
	if !val.IsValid() {
		return nil, nil
	}

	switch val.Kind() {
	case reflect.Interface, reflect.Pointer:
		if val.IsNil() {
			return []byte(null), nil
		}
		return convertToByteArray(val.Elem())

	case reflect.String:
		return []byte(val.String()), nil
	case reflect.Bool:
		return strconv.AppendBool(nil, val.Bool()), nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.AppendInt(nil, val.Int(), 10), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return strconv.AppendUint(nil, val.Uint(), 10), nil
	case reflect.Float32, reflect.Float64:
		return strconv.AppendFloat(nil, val.Float(), 'f', -1, val.Type().Bits()), nil
	case reflect.Complex64, reflect.Complex128:
		return []byte(strconv.FormatComplex(val.Complex(), 'f', -1, val.Type().Bits())), nil
	case reflect.Slice:
		if val.Type().Elem().Kind() == reflect.Uint8 {
			return val.Bytes(), nil
		}
		return json.Marshal(val.Interface())
	case reflect.Array, reflect.Map, reflect.Struct:
		return json.Marshal(val.Interface())
	default:
		return nil, fmt.Errorf("unsupported response type: %s", val.Type())
	}
}
