// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"bytes"
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"sync"
)

// This file holds per-request parameter injection orchestration. Binding
// plans, JSON decoding, and form/text conversion live in sibling files.

// Media-type fragments and error formats used by request-time injection.
const (
	// media-type family carrying JSON payloads for body binding
	applicationPrefix = "application/"
	// structured-suffix marker identifying JSON subtypes
	plusSignJson = "+json"
	// error wrapper naming the parameter being injected
	injectParameterErrFormat = "inject parameter %q: %w"
	// error wrapper naming the key decoded from the JSON body
	jsonParameterErrFormat = "JSON parameter %q: %w"
)

// paramsPool recycles the reflection argument slice built per request. A pointer
// to the slice is stored so Get/Put do not box the slice header.
var paramsPool = sync.Pool{
	New: func() any { s := make([]reflect.Value, 0, 8); return &s },
}

// maxPooledParamsCapacity bounds retained reflection argument storage.
const maxPooledParamsCapacity = 256

// acquireParams returns a zeroed reflection argument slice of length n, reusing a
// pooled backing array when it is large enough.
func acquireParams(n int) (*[]reflect.Value, []reflect.Value) {
	bufPool := paramsPool.Get().(*[]reflect.Value)
	params := *bufPool
	if cap(params) < n {
		params = make([]reflect.Value, n)
	}
	params = params[:n]
	clear(params)
	return bufPool, params
}

// releaseParams zeroes the argument slice (so it pins no request value) and
// returns it to the pool.
func releaseParams(bufPool *[]reflect.Value, params []reflect.Value) {
	clear(params)
	if cap(params) > maxPooledParamsCapacity {
		return
	}
	*bufPool = params
	paramsPool.Put(bufPool)
}

// injectedParameters fills the reflection argument list for a handler call, driven
// by the handler precomputed bind plan. Each parameter is injected directly
// (request, writer, file headers) or bound by name from path parameters, a JSON
// body, or form values, in that precedence order. Type classification already
// happened in generateRoute, so nothing here re-inspects the signature with
// reflection. params is the scratch slice owned by the caller, already sized to
// parameter count; the caller recycles it. The jsonMedia flag already carries
// the empty-body check, so the body is never re-scanned here.
func injectedParameters(
	c *Context,
	form url.Values,
	files filesMap,
	body []byte,
	jsonMedia bool,
	params []reflect.Value,
) error {
	rh := c.rh
	plan := &rh.plan
	names := rh.paramNames

	c.src = nameSources{path: c.params, form: form, files: files}
	src := &c.src

	var jsonFields *pooledJSONFields
	if jsonMedia && len(plan.businessIndexes) > 1 {
		defer func() {
			if jsonFields != nil {
				releaseJSONFields(jsonFields)
			}
		}()
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
			if !plan.writerOK[i] {
				return fmt.Errorf(injectParameterErrFormat, name, responseMismatch)
			}
			params[i] = reflect.ValueOf(c.writer)
			continue
		case classSSE:
			params[i] = reflect.ValueOf(c.sse)
			continue
		case classFiles:
			headers, _ := src.filesGet(name)
			v, err := bindFileHeaders(t, headers)
			if err != nil {
				return fmt.Errorf(injectParameterErrFormat, name, err)
			}
			params[i] = v
			continue
		}

		// classBind: a business parameter, resolved from request data. Path
		// parameters take priority over query/body for scalar arguments too.
		// Self-describing structs bind their fields and ignore an outer path value
		// that happens to share the handler parameter name.
		if plan.pathBindable[i] {
			if pathValue, ok := src.pathGet(name); ok {
				v, err := bindTextValue(t, []string{pathValue})
				if err != nil {
					return fmt.Errorf("path parameter %q: %w", name, err)
				}
				params[i] = v
				continue
			}
		}

		if jsonMedia {
			// A lone business parameter decodes the whole body; several split
			// the JSON object by parameter name. Decode only after path precedence
			// has failed for the first parameter that actually needs JSON.
			if len(plan.businessIndexes) > 1 && jsonFields == nil {
				jsonFields = acquireJSONFields(len(plan.jsonKeySet))
				if err := decodeJSONFieldsInto(body, plan.jsonKeys, plan.jsonKeyTable, jsonFields.values); err != nil {
					return err
				}
			}
			raw := body
			found := true
			if len(plan.businessIndexes) > 1 {
				raw, found = jsonFields.values[name]
			}
			var v reflect.Value
			if found {
				var decoded reflect.Value
				var err error
				if plan.jsonExact[i] {
					decoded, err = decodeJSONExactStruct(t, raw)
				} else {
					decoded, err = decodeJSON(t, raw)
				}
				if err != nil {
					return fmt.Errorf(jsonParameterErrFormat, name, err)
				}
				v = decoded
			} else {
				v = reflect.New(t).Elem()
			}
			params[i] = v
			continue
		}

		var (
			v   reflect.Value
			err error
		)
		if fields := plan.structFields[i]; fields != nil {
			v, err = bindStructFields(t, fields, src)
		} else {
			v, err = bindFormParameter(t, name, src)
		}
		if err != nil {
			return fmt.Errorf("parameter %q: %w", name, err)
		}
		params[i] = v
	}

	return nil
}

// paramNameAt is a bounds-safe lookup of a handler parameter name.
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

// hasNonWhitespace reports whether data holds at least one byte beyond JSON
// whitespace (space, tab, carriage return or newline). Like TrimSpace, the scan
// is allocation-free and does not copy data; the difference is that TrimSpace
// also treats Unicode whitespace as blank, while JSON does not.
func hasNonWhitespace(data []byte) bool {
	return bytes.ContainsFunc(data, func(r rune) bool {
		return r != ' ' && r != '\t' && r != '\n' && r != '\r'
	})
}

// isJSONMediaType reports whether a parsed media type is JSON — the exact
// application/json or a structured suffix such as application/vnd.api+json.
func isJSONMediaType(mediaType string) bool {
	return mediaType == applicationJson ||
		(strings.HasPrefix(mediaType, applicationPrefix) && strings.HasSuffix(mediaType, plusSignJson))
}
