// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"reflect"
	"runtime"
	"strconv"
)

type PanicProcessor func(err any) any

var panicProcessor PanicProcessor = nil

func RegisterPanicProcessor(p PanicProcessor) {
	panicProcessor = p
}

// errorWriteback Exception information is written back according to the reflection type
func errorWriteback(w http.ResponseWriter, err any) {
	val := reflect.ValueOf(err)
	switch val.Kind() {
	case reflect.Struct, reflect.Map:
		bytes, _ := json.Marshal(val.Interface())
		w.Header().Set(contentType, applicationJson)
		_, _ = w.Write(bytes)
	case reflect.Array, reflect.Slice:
		bytes, _ := json.Marshal(val.Interface())
		_, _ = w.Write(bytes)
	case reflect.String:
		_, _ = w.Write([]byte(val.String()))
	case reflect.Pointer, reflect.Interface:
		errorWriteback(w, val.Elem())
	case reflect.Bool:
		_, _ = w.Write(strconv.AppendBool(nil, val.Bool()))
	case reflect.Float32, reflect.Float64:
		_, _ = w.Write(strconv.AppendFloat(nil, val.Float(), 'f', -1, val.Type().Bits()))
	case reflect.Int, reflect.Int8, reflect.Int16,
		reflect.Int32, reflect.Int64:
		_, _ = w.Write(strconv.AppendInt(nil, val.Int(), 10))
	case reflect.Uint, reflect.Uint8, reflect.Uint16,
		reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		_, _ = w.Write(strconv.AppendUint(nil, val.Uint(), 10))
	case reflect.Complex64, reflect.Complex128:
		_, _ = w.Write([]byte(strconv.FormatComplex(val.Complex(), 'f', -1, val.Type().Bits())))
	default:
		_, _ = w.Write([]byte(typeRecognitionError))
	}
}

func catchRuntimePanic(w http.ResponseWriter) {
	err := recover()
	if err == nil {
		return
	}

	if panicProcessor == nil {
		http.Error(w, fmt.Errorf("%v", err).Error(), http.StatusInternalServerError)
		return
	}

	err = panicProcessor(err)

	switch err.(type) {
	case runtime.Error:
		http.Error(w, fmt.Errorf("%v", err).Error(), http.StatusInternalServerError)
	default:
		errorWriteback(w, err)
	}
}
