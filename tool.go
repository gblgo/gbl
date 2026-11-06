// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"encoding/json/v2"
	"reflect"
)

func convertToByteArray(val reflect.Value) []byte {
	switch val.Kind() {
	case reflect.Bool,
		reflect.Struct,
		reflect.Float32, reflect.Float64,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		bytes, _ := json.Marshal(val.Interface())
		return bytes
	case reflect.String:
		return []byte(val.String())
	case reflect.Pointer, reflect.Interface:
		return convertToByteArray(val.Elem())
	default:
		return []byte(typeRecognitionError)
	}
}
