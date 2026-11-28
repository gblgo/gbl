// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"unsafe"
)

// This file holds pooled query/form parsing and exact key selection.

// mergeSelectedFormInPlace adds query-only fields to the selected body map.
// Request.Form and Request.PostForm intentionally share this request-local view.
func mergeSelectedFormInPlace(query, body url.Values) url.Values {
	for key, values := range query {
		if _, exists := body[key]; !exists {
			body[key] = values
		}
	}
	return body
}

// mergeFormBodyWins merges multipart query and body values without mutating the
// standard library's PostForm map. The returned map and slices are independent.
func mergeFormBodyWins(query, body url.Values) url.Values {
	merged := make(url.Values, len(query)+len(body))
	for key, values := range query {
		merged[key] = slices.Clone(values)
	}
	for key, values := range body {
		merged[key] = slices.Clone(values)
	}
	return merged
}

// queryValues returns the parsed URL query, or nil when the request carries no
// query string. Returning nil avoids the empty map that URL query parsing allocates
// an empty RawQuery; form lookups and mergeFormBodyWins already treat nil as "no
// query values".
func queryValues(r *http.Request) url.Values {
	if r.URL.RawQuery == empty {
		return nil
	}
	return r.URL.Query()
}

// queryValuesFor parses only the exact keys a handler can bind. It preserves
// percent decoding, repeated values and the standard treatment of malformed
// pairs while avoiding a map entry for every unrelated query key.
func queryValuesFor(r *http.Request, keys []string) url.Values {
	return queryValuesForSet(r, newRequestKeySet(keys))
}

// queryValuesForSet is the request-path variant. Route handlers pass the lookup
// built by bindPlan.prepareKeys, avoiding both a key slice and a linear scan of
// that slice for every query pair.
func queryValuesForSet(r *http.Request, keys requestKeySet) url.Values {
	if len(keys) == 0 || r.URL.RawQuery == empty {
		return nil
	}
	var values url.Values
	for part := range strings.SplitSeq(r.URL.RawQuery, "&") {
		if part == empty {
			continue
		}
		rawKey, rawValue, _ := strings.Cut(part, "=")
		if strings.Contains(part, ";") {
			continue
		}
		key, err := url.QueryUnescape(rawKey)
		if err != nil {
			continue
		}
		if _, ok := keys[key]; !ok {
			continue
		}
		value, err := url.QueryUnescape(rawValue)
		if err != nil {
			continue
		}
		if values == nil {
			values = make(url.Values)
		}
		values[key] = append(values[key], value)
	}
	return values
}

// maxPooledFormKeys bounds retained map capacity for request-scoped form values.
const maxPooledFormKeys = 64

// maxPooledFormValues bounds each pooled field slice.
const maxPooledFormValues = 256

// maxPooledFormBytes bounds body buffers eligible for pooling and marks larger forms unpooled.
const maxPooledFormBytes = 64 << 10

// pooledFormValues owns one pooled field slice.
type pooledFormValues struct {
	values []string
}

// formValueSlicePool reuses field slices without pooling the exposed url.Values map.
var formValueSlicePool = sync.Pool{
	New: func() any { return &pooledFormValues{} },
}

// pooledForm owns request-scoped Form/PostForm maps and pooled field slices.
// It remains valid until Context.recycle releases it after middleware and handlers return.
type pooledForm struct {
	values       url.Values
	objects      map[string]*pooledFormValues
	sets         []*pooledFormValues
	body         []byte
	bodyRetained bool // selected strings alias body, so release drops instead of clearing it.
	large        bool
}

// formPool reuses ordinary form maps and field-slice bookkeeping.
var formPool = sync.Pool{
	New: func() any {
		return &pooledForm{
			values:  make(url.Values),
			objects: make(map[string]*pooledFormValues),
		}
	},
}

// acquirePooledForm clears a pooled form and records whether its capacity is bounded.
func acquirePooledForm(keyCount int) *pooledForm {
	form := formPool.Get().(*pooledForm)
	clear(form.values)
	clear(form.objects)
	clear(form.sets)
	form.sets = form.sets[:0]
	form.body = form.body[:0]
	form.bodyRetained = false
	form.large = keyCount > maxPooledFormKeys
	return form
}

// urlValues returns the request-scoped binding view.
func (form *pooledForm) urlValues() url.Values {
	if form == nil {
		return nil
	}
	return form.values
}

// appendValue stores one value in a pooled field slice.
func (form *pooledForm) appendValue(key, value string) {
	set := form.objects[key]
	if set == nil {
		set = formValueSlicePool.Get().(*pooledFormValues)
		set.values = set.values[:0]
		form.objects[key] = set
		form.sets = append(form.sets, set)
	}
	set.values = append(set.values, value)
	form.values[key] = set.values
}

// parseQuery selects request query values into the same pool used for form bodies.
func (form *pooledForm) parseQuery(r *http.Request, keys requestKeySet) {
	if len(keys) == 0 || r.URL.RawQuery == empty {
		return
	}
	for part := range strings.SplitSeq(r.URL.RawQuery, "&") {
		if part == empty {
			continue
		}
		rawKey, rawValue, _ := strings.Cut(part, "=")
		if strings.Contains(part, ";") {
			continue
		}
		key, err := url.QueryUnescape(rawKey)
		if err != nil {
			continue
		}
		if _, ok := keys[key]; !ok {
			continue
		}
		value, err := url.QueryUnescape(rawValue)
		if err != nil {
			continue
		}
		form.appendValue(key, value)
	}
}

// mergeQuery adds query-only fields while preserving body precedence.
func (form *pooledForm) mergeQuery(r *http.Request, keys requestKeySet) {
	if len(keys) == 0 || r.URL.RawQuery == empty {
		return
	}
	for part := range strings.SplitSeq(r.URL.RawQuery, "&") {
		if part == empty {
			continue
		}
		rawKey, rawValue, _ := strings.Cut(part, "=")
		if strings.Contains(part, ";") {
			continue
		}
		key, err := url.QueryUnescape(rawKey)
		if err != nil {
			continue
		}
		if _, ok := keys[key]; !ok {
			continue
		}
		if _, exists := form.objects[key]; exists {
			continue
		}
		value, err := url.QueryUnescape(rawValue)
		if err != nil {
			continue
		}
		form.appendValue(key, value)
	}
}

// releasePooledForm clears request references and pools only bounded storage.
func releasePooledForm(form *pooledForm) {
	if form == nil {
		return
	}
	for _, set := range form.sets {
		clear(set.values)
		if !form.large && cap(set.values) <= maxPooledFormValues {
			set.values = set.values[:0]
			formValueSlicePool.Put(set)
		}
	}
	clear(form.values)
	clear(form.objects)
	clear(form.sets)
	if form.bodyRetained {
		form.body = nil
		form.bodyRetained = false
	} else {
		clear(form.body)
	}
	if form.large || cap(form.sets) > maxPooledFormKeys || cap(form.body) > maxPooledFormBytes {
		return
	}
	form.sets = form.sets[:0]
	form.body = form.body[:0]
	formPool.Put(form)
}

// parse decodes selected urlencoded fields in one pass using pooled maps and field slices.
func (form *pooledForm) parse(data []byte, keys requestKeySet) error {
	if len(keys) == 0 || len(data) == 0 {
		return nil
	}
	if len(data) > maxPooledFormBytes {
		form.large = true
	}
	if cap(form.body) < len(data) {
		form.body = make([]byte, len(data))
	} else {
		form.body = form.body[:len(data)]
	}
	copy(form.body, data)
	text := unsafe.String(unsafe.SliceData(form.body), len(form.body))
	for part := range strings.SplitSeq(text, "&") {
		if part == empty {
			continue
		}
		rawKey, rawValue, _ := strings.Cut(part, "=")
		if strings.Contains(part, ";") {
			return errors.New("invalid semicolon separator in query")
		}
		key, err := url.QueryUnescape(rawKey)
		if err != nil {
			return err
		}
		if _, ok := keys[key]; !ok {
			continue
		}
		if !strings.ContainsAny(rawKey, "%+") {
			form.bodyRetained = true
		}
		value, err := url.QueryUnescape(rawValue)
		if err != nil {
			return err
		}
		if !strings.ContainsAny(rawValue, "%+") {
			form.bodyRetained = true
		}
		form.appendValue(key, value)
	}
	return nil
}

// formValuesForSet is the direct-call variant; callers must release the returned form.
func formValuesForSet(data []byte, keys requestKeySet) (*pooledForm, error) {
	form := acquirePooledForm(len(keys))
	if err := form.parse(data, keys); err != nil {
		releasePooledForm(form)
		return nil, err
	}
	return form, nil
}

// requestKeySource accepts the startup-prepared set used by route handlers and
// a plain slice used by direct parser callers.
func requestKeySource(source any) (int, requestKeySet, bool) {
	switch keys := source.(type) {
	case requestKeySet:
		return len(keys), keys, true
	case []string:
		if keys == nil {
			return 0, nil, false
		}
		return len(keys), newRequestKeySet(keys), true
	default:
		return 0, nil, false
	}
}
