// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"encoding/json/v2"
	"net/http"
	"reflect"
	"strconv"
)

type HttpStatus int

type ErrorHandlingReturnInfo any

type PanicProcessor func(err any) (ErrorHandlingReturnInfo, HttpStatus)

var panicProcessor PanicProcessor = nil

func RegisterPanicProcessor(p PanicProcessor) {
	panicProcessor = p
}

var typeRecognitionError = []byte("Reflection Type Identification Exception")

// errorWriteback Exception information is written back according to the reflection type
func errorWriteback(w http.ResponseWriter, err any) []byte {
	val := reflect.ValueOf(err)
	var bytes []byte
	switch val.Kind() {
	case reflect.Struct, reflect.Map:
		w.Header().Set(contentType, applicationJson)
		bytes, _ = json.Marshal(val.Interface())
	case reflect.Array, reflect.Slice:
		bytes, _ = json.Marshal(val.Interface())
	case reflect.String:
		bytes = []byte(val.String())
	case reflect.Pointer, reflect.Interface:
		errorWriteback(w, val.Elem())
	case reflect.Bool:
		bytes = strconv.AppendBool(nil, val.Bool())
	case reflect.Float32, reflect.Float64:
		bytes = strconv.AppendFloat(nil, val.Float(), 'f', -1, val.Type().Bits())
	case reflect.Int, reflect.Int8, reflect.Int16,
		reflect.Int32, reflect.Int64:
		bytes = strconv.AppendInt(nil, val.Int(), 10)
	case reflect.Uint, reflect.Uint8, reflect.Uint16,
		reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		bytes = strconv.AppendUint(nil, val.Uint(), 10)
	case reflect.Complex64, reflect.Complex128:
		bytes = []byte(strconv.FormatComplex(val.Complex(), 'f', -1, val.Type().Bits()))
	default:
		bytes = typeRecognitionError
	}
	return bytes
}

func catchRuntimePanic(rw ResponseWriter) {
	err := recover()
	if err == nil {
		return
	}

	if rw.written() {
		rw.writeHeader()
		return
	}

	if panicProcessor == nil {
		bytes := errorWriteback(rw, err)
		rw.writeHeader()
		_, _ = rw.Write(bytes)
		return
	}

	err, httpStatus := panicProcessor(err)
	bytes := errorWriteback(rw, err)
	rw.WriteHeader(int(httpStatus))
	rw.writeHeader()
	_, _ = rw.Write(bytes)
}
