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

// This file holds response serialization: rendering a handler's return value
// into the bytes the pipeline writes. The pooled writer itself lives in
// responseWriter.go; the dispatch that stages this result lives in handler.go.

// responseBufferPool recycles the buffer used to serialize handler return values,
// so a request reuses one backing array instead of allocating fresh bytes every time.
var responseBufferPool = sync.Pool{
	New: func() any { return new(bytes.Buffer) },
}

// convertToBuffer renders a handler return value into buf: byte slices pass
// through, strings are converted and scalars formatted, composites are
// JSON-marshaled, and nil pointers/interfaces become an empty body.
func convertToBuffer(buf *bytes.Buffer, val reflect.Value) error {
	if !val.IsValid() {
		return nil
	}

	switch val.Kind() {
	case reflect.Interface, reflect.Pointer:
		if val.IsNil() {
			buf.WriteString(empty)
			return nil
		}
		return convertToBuffer(buf, val.Elem())

	case reflect.String:
		buf.WriteString(val.String())
		return nil
	case reflect.Bool:
		buf.Write(strconv.AppendBool(nil, val.Bool()))
		return nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		buf.Write(strconv.AppendInt(nil, val.Int(), 10))
		return nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		buf.Write(strconv.AppendUint(nil, val.Uint(), 10))
		return nil
	case reflect.Float32, reflect.Float64:
		buf.Write(strconv.AppendFloat(nil, val.Float(), 'f', -1, val.Type().Bits()))
		return nil
	case reflect.Complex64, reflect.Complex128:
		buf.WriteString(strconv.FormatComplex(val.Complex(), 'f', -1, val.Type().Bits()))
		return nil
	case reflect.Slice:
		if val.Type().Elem().Kind() == reflect.Uint8 {
			buf.Write(val.Bytes())
			return nil
		}
		data, err := json.Marshal(val.Interface())
		if err != nil {
			return err
		}
		buf.Write(data)
		return nil
	case reflect.Array, reflect.Map, reflect.Struct:
		data, err := json.Marshal(val.Interface())
		if err != nil {
			return err
		}
		buf.Write(data)
		return nil
	default:
		return fmt.Errorf("unsupported response type: %s", val.Type())
	}
}
