// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
)

// This file holds response serialization: rendering a handler's return value
// into the bytes the pipeline writes. The pooled writer itself lives in
// responseWriter.go; the dispatch that stages this result lives in handler.go.

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
