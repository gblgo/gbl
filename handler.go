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
	"sync/atomic"
)

type (
	Files          map[string][]*multipart.FileHeader
	defaultHandler struct{}
	byteSize       int
)

var (
	requestBodyTooLargeError = errors.New("request body too large")
	multipleRequireError     = errors.New("multiple handler parameters require a JSON object")
	responseMismatch         = errors.New("response writer type mismatch")
	maxBodySize              atomic.Int64
	maxFileMemory            atomic.Int64
	requestType              = reflect.TypeFor[*http.Request]()
	writerType               = reflect.TypeFor[http.ResponseWriter]()
	fileHeaderType           = reflect.TypeFor[multipart.FileHeader]()
	fileHeaderPtr            = reflect.TypeFor[*multipart.FileHeader]()
	fileHeaderPs             = reflect.TypeFor[[]*multipart.FileHeader]()
	fileHeaderSlice          = reflect.TypeFor[[]multipart.FileHeader]()
)

func init() {
	maxBodySize.Store(10 << 20)   // Maximum total request body size: 10 MiB.
	maxFileMemory.Store(32 << 20) // Multipart in-memory threshold, not total upload limit.
}

func SetMaxBodySize(size byteSize) {
	if size <= 0 {
		panic("gbl: max body size must be greater than zero")
	}
	maxBodySize.Store(int64(size))
}

func SetMaxFileSize(size byteSize) {
	if size <= 0 {
		panic("gbl: multipart memory threshold must be greater than zero")
	}
	maxFileMemory.Store(int64(size))
}

func (dh *defaultHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	serveHTTP(w, r)
}

func serveHTTP(w http.ResponseWriter, r *http.Request) {
	rw := &gblResponseWriter{w: w}
	defer catchRuntimePanic(rw)

	pathParams := make(map[string]string)
	rh := matchNode(parseMethod(r.Method), r.URL.Path, pathParams)
	if rh == nil {
		rw.WriteHeader(http.StatusNotFound)
		panic(http.StatusText(http.StatusNotFound))
	}

	if interceptor != nil {
		if !interceptor(rw, r) || rw.written() {
			return
		}
	}

	form, files, body, mediaType, err := parseRequestParameters(rw, r)
	if err != nil {
		writeRequestError(rw, err)
		return
	}
	if r.MultipartForm != nil {
		defer func(MultipartForm *multipart.Form) {
			err = MultipartForm.RemoveAll()
			if err != nil {
				rw.WriteHeader(http.StatusBadRequest)
				panic(err)
			}
		}(r.MultipartForm)
	}

	rt, rv, names := rh.t, rh.v, rh.paramNames

	in, err := injectedParameters(rw, r, pathParams, rt, names, form, files, body, mediaType)
	if err != nil {
		rw.WriteHeader(http.StatusBadRequest)
		panic(err)
	}

	out := rv.Call(in)
	result, err := convertToByteArray(out[0])
	if err != nil {
		rw.WriteHeader(http.StatusInternalServerError)
		panic(responseSerializationFailed)
	}
	doProcessor(rw, r, out[0].Interface())
	if rw.written() {
		return
	}

	w.Header().Set(contentType, rh.hct)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(result)
}

func writeRequestError(w http.ResponseWriter, err error) {
	if errors.Is(err, requestBodyTooLargeError) {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		panic(http.StatusText(http.StatusRequestEntityTooLarge))
		return
	}
	w.WriteHeader(http.StatusBadRequest)
	panic(err)
}

func isSpecialParameter(t reflect.Type) bool {
	if t == requestType || t == writerType || t.Implements(writerType) {
		return true
	}
	switch t {
	case fileHeaderType, fileHeaderPtr, fileHeaderPs, fileHeaderSlice:
		return true
	default:
		return false
	}
}

func injectedParameters(
	w http.ResponseWriter,
	r *http.Request,
	pathParams map[string]string,
	rt reflect.Type,
	paramNames []string,
	form url.Values,
	files Files,
	body []byte,
	mediaType string,
) ([]reflect.Value, error) {
	params := make([]reflect.Value, rt.NumIn())
	businessIndexes := make([]int, 0, rt.NumIn())

	for i := 0; i < rt.NumIn(); i++ {
		if !isSpecialParameter(rt.In(i)) {
			businessIndexes = append(businessIndexes, i)
		}
	}

	var jsonFields map[string]json.RawMessage
	isJSON := isJSONMediaType(mediaType) && len(bytes.TrimSpace(body)) > 0
	if isJSON && len(businessIndexes) > 1 {
		if err := json.Unmarshal(body, &jsonFields); err != nil {
			return nil, fmt.Errorf("invalid JSON body: %w", err)
		}
		if jsonFields == nil {
			return nil, multipleRequireError
		}
	}

	for i, name := range paramNames {
		t := rt.In(i)

		if isSpecialParameter(t) {
			v, err := injectSpecialParameter(t, w, r, files, name)
			if err != nil {
				return nil, fmt.Errorf("inject parameter %q: %w", name, err)
			}
			params[i] = v
			continue
		}

		// Path parameters have priority for scalar handler arguments too.
		if pathValue, ok := lookupString(pathParams, name); ok {
			v, err := bindTextValue(t, []string{pathValue})
			if err != nil {
				return nil, fmt.Errorf("path parameter %q: %w", name, err)
			}
			params[i] = v
			continue
		}

		if isJSON {
			if len(businessIndexes) == 1 {
				v, err := decodeJSON(t, body)
				if err != nil {
					return nil, fmt.Errorf("JSON parameter %q: %w", name, err)
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
				return nil, fmt.Errorf("JSON parameter %q: %w", name, err)
			}
			params[i] = v
			continue
		}

		v, err := bindFormParameter(t, name, form, pathParams, files)
		if err != nil {
			return nil, fmt.Errorf("parameter %q: %w", name, err)
		}
		params[i] = v
	}

	for i, v := range params {
		if !v.IsValid() {
			return nil, fmt.Errorf("parameter %q was not initialized", paramNames[i])
		}
		if !v.Type().AssignableTo(rt.In(i)) {
			return nil, fmt.Errorf("parameter %q type mismatch: got %s, want %s", paramNames[i], v.Type(), rt.In(i))
		}
	}
	return params, nil
}

func isJSONMediaType(mediaType string) bool {
	return mediaType == applicationJson ||
		(strings.HasPrefix(mediaType, applicationNoJson) && strings.HasSuffix(mediaType, plusSignJson))
}

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

func bindFormParameter(t reflect.Type, name string, form url.Values, pathParams map[string]string, files Files) (reflect.Value, error) {
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

func bindStruct(t reflect.Type, form url.Values, pathParams map[string]string, files Files) (reflect.Value, error) {
	result := reflect.New(t).Elem()

	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		dst := result.Field(i)
		if !dst.CanSet() || field.PkgPath != null {
			continue
		}

		// No tags are required: Go field names are matched case-insensitively.
		name := field.Name

		// Path wins over query/form values.
		if pathValue, ok := lookupString(pathParams, name); ok {
			value, err := bindTextValue(field.Type, []string{pathValue})
			if err != nil {
				return reflect.Value{}, fmt.Errorf("path field %q: %w", field.Name, err)
			}
			dst.Set(value)
			continue
		}

		value, err := bindFormParameter(field.Type, name, form, pathParams, files)
		if err != nil {
			return reflect.Value{}, fmt.Errorf("field %q: %w", field.Name, err)
		}
		dst.Set(value)
	}
	return result, nil
}

func lookupFiles(files Files, name string) ([]*multipart.FileHeader, bool) {
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

func isFileHeaderType(t reflect.Type) bool {
	return t == fileHeaderType || t == fileHeaderPtr || t == fileHeaderPs || t == fileHeaderSlice
}

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

func injectSpecialParameter(
	t reflect.Type,
	w http.ResponseWriter,
	r *http.Request,
	files Files,
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

func parseRequestParameters(w http.ResponseWriter, r *http.Request) (url.Values, Files, []byte, string, error) {
	// GBL deliberately applies a strict parameter-source policy by HTTP method.
	// GET and DELETE may read only URL path parameters (matched by the router)
	// and URL query parameters. Their request bodies are never parsed or read,
	// regardless of Content-Type.
	if r.Method == http.MethodGet || r.Method == http.MethodDelete {
		return r.URL.Query(), nil, nil, null, nil
	}

	// Only POST and PUT are allowed to read request bodies. For any other
	// method, expose query parameters only and leave the body untouched.
	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		return r.URL.Query(), nil, nil, null, nil
	}

	var (
		form      url.Values
		files     Files
		body      []byte
		mediaType string
	)

	// Apply one hard limit to the entire request body, including multipart files.
	limit := maxBodySize.Load()
	if limit <= 0 {
		limit = 10 << 20
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
			memory = 32 << 20
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

func doProcessor(prw ProcessorResponseWriter, r *http.Request, data any) {
	if processor == nil {
		return
	}
	processor(prw, r, data)
}
