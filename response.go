// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"sync"
)

// This file holds response serialization: rendering a handler return value
// into the bytes the pipeline writes. The pooled writer itself lives in
// responseWriter.go; the dispatch that stages this result lives in handler.go.

// responseBufferPool recycles the buffer used to serialize handler return values,
// so a request reuses one backing array instead of allocating fresh bytes every time.
var responseBufferPool = sync.Pool{
	New: func() any { return new(bytes.Buffer) },
}

// largest serialized response buffer retained by the pool
const maxPooledResponseCapacity = 1 << 20

// releaseResponseBuffer resets and pools a buffer unless its capacity is too
// large to retain safely.
func releaseResponseBuffer(buf *bytes.Buffer) {
	if buf.Cap() > maxPooledResponseCapacity {
		return
	}
	buf.Reset()
	responseBufferPool.Put(buf)
}

// responseErrorType identifies values rendered through their Error message.
var responseErrorType = reflect.TypeFor[error]()

// convertToBuffer renders a handler return value into buf and returns the media
// type that matches the rendered bytes. Byte slices pass through, strings and
// scalars are plain text, errors use their message, composites are JSON, and nil
// pointers or interfaces become an empty plain-text body.
func convertToBuffer(buf *bytes.Buffer, val reflect.Value) (string, error) {
	if !val.IsValid() {
		return textPlainCharset, nil
	}

	for val.Kind() == reflect.Interface || val.Kind() == reflect.Pointer {
		if val.IsNil() {
			return textPlainCharset, nil
		}
		if val.Type().Implements(responseErrorType) {
			buf.WriteString(val.Interface().(error).Error())
			return textPlainCharset, nil
		}
		val = val.Elem()
	}

	if val.Type().Implements(responseErrorType) {
		buf.WriteString(val.Interface().(error).Error())
		return textPlainCharset, nil
	}

	// Reuse stack storage for scalar formatting; buf.Write copies it immediately.
	var number [64]byte
	switch val.Kind() {
	case reflect.String:
		buf.WriteString(val.String())
		return textPlainCharset, nil
	case reflect.Bool:
		buf.Write(strconv.AppendBool(number[:0], val.Bool()))
		return textPlainCharset, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		buf.Write(strconv.AppendInt(number[:0], val.Int(), 10))
		return textPlainCharset, nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		buf.Write(strconv.AppendUint(number[:0], val.Uint(), 10))
		return textPlainCharset, nil
	case reflect.Float32, reflect.Float64:
		buf.Write(strconv.AppendFloat(number[:0], val.Float(), 'f', -1, val.Type().Bits()))
		return textPlainCharset, nil
	case reflect.Complex64, reflect.Complex128:
		buf.WriteString(strconv.FormatComplex(val.Complex(), 'f', -1, val.Type().Bits()))
		return textPlainCharset, nil
	case reflect.Slice:
		if val.Type().Elem().Kind() == reflect.Uint8 {
			buf.Write(val.Bytes())
			return textPlainCharset, nil
		}
		data, err := json.Marshal(val.Interface())
		if err != nil {
			return empty, err
		}
		buf.Write(data)
		return applicationJsonCharset, nil
	case reflect.Array, reflect.Map, reflect.Struct:
		data, err := json.Marshal(val.Interface())
		if err != nil {
			return empty, err
		}
		buf.Write(data)
		return applicationJsonCharset, nil
	default:
		return empty, fmt.Errorf("unsupported response type: %s", val.Type())
	}
}
