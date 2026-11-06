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
)

type PanicProcessor func(err any) any

var panicProcessor PanicProcessor = nil

func RegisterPanicProcessor(p PanicProcessor) {
	panicProcessor = p
}

func errorWriteback(w http.ResponseWriter, err any) {
	val := reflect.ValueOf(err)
	switch val.Kind() {
	case reflect.Bool,
		reflect.Struct,
		reflect.Float32, reflect.Float64,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		bytes, _ := json.Marshal(val.Interface())
		w.Header().Set(contentType, applicationJson)
		_, _ = w.Write(bytes)
	case reflect.String:
		_, _ = w.Write([]byte(val.String()))
	case reflect.Pointer, reflect.Interface:
		errorWriteback(w, val.Elem())
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
