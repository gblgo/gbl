// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"encoding/json"
	"net/http"
	"reflect"
	"runtime/debug"
	"strconv"
)

// HttpStatus is the status code a PanicProcessor returns alongside its payload.
// A distinct named type, it documents intent at the hook boundary.
type HttpStatus int

// ErrorHandlingReturnInfo is the payload a PanicProcessor may return; it accepts
// any value the response writer can render (string, error, struct, map, ...).
type ErrorHandlingReturnInfo any

// PanicProcessor is the user-installed hook that renders every recovered panic
// value into a response body and status. Once registered it owns rendering for
// all recovered values, genuine crashes included, so it must avoid leaking
// internal error details to the client.
type PanicProcessor func(err any) (ErrorHandlingReturnInfo, HttpStatus)

// RegisterPanicProcessor installs the panic renderer on the default engine. As
// soon as one is registered it owns the rendering of every recovered value,
// including unexpected crashes, so the caller takes on the responsibility of
// not echoing internal errors.
func RegisterPanicProcessor(p PanicProcessor) {
	defaultEngine.panicProcessor = p
}

// SetPanicProcessor installs the panic renderer on this engine.
func (e *Engine) SetPanicProcessor(p PanicProcessor) *Engine {
	e.panicProcessor = p
	return e
}

// responseAbort is the panic value used to end a request early with a specific
// status and a message that is safe to send to the client. The framework raises
// it for its own answers (404, 405, 413, 501, request-parse and serialization
// failures); Abort raises it on the user's behalf. Any other recovered value is
// treated as an unexpected crash and never echoed to the client.
type responseAbort struct {
	status int
	body   string
}

// Abort ends the current request immediately, returning the given status and a
// plain-text body to the client. It must be called from within a handler or
// middleware. Unlike a bare panic, the message is guaranteed to reach the
// client; a panic of any other value yields a generic 500.
func Abort(status int, msg string) {
	panic(responseAbort{status: status, body: msg})
}

// abort raises an internal responseAbort panic,
// the form every framework-generated halt uses so the value is recognized as displayable rather than as a crash.
func abort(status int, body string) {
	panic(responseAbort{status: status, body: body})
}

// typeRecognitionError is the fixed fallback body written when errorWriteback
// meets a reflected value of an unhandled kind.
var typeRecognitionError = []byte("Reflection Type Identification Exception")

// errorWriteback Exception information is written back according to the reflection type
func errorWriteback(w http.ResponseWriter, err any) []byte {
	// A value implementing error is written back as its message. Reflecting it
	// instead walks the concrete type, and the usual errors.New and fmt.Errorf
	// values consist entirely of unexported fields, so the client would receive
	// an empty "{}" with no information at all. Keeping the message plain also
	// matches the 404, 405 and 501 replies the framework generates itself.
	if e, ok := err.(error); ok {
		return []byte(e.Error())
	}

	val := reflect.ValueOf(err)
	var bytes []byte
	switch val.Kind() {
	case reflect.Struct, reflect.Map:
		w.Header().Set(contentType, applicationJsonCharset)
		bytes, _ = json.Marshal(val.Interface())
	case reflect.Array, reflect.Slice:
		bytes, _ = json.Marshal(val.Interface())
	case reflect.String:
		bytes = []byte(val.String())
	case reflect.Pointer, reflect.Interface:
		// A nil pointer/interface has no element to unwrap.
		if val.IsNil() {
			return []byte(null)
		}
		// Unwrap one layer and return the result, passing the element value
		// itself rather than the reflect.Value wrapper.
		return errorWriteback(w, val.Elem().Interface())
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

// detectContentType picks the Content-Type for a panic payload so it agrees with
// how errorWriteback renders it: plain text for errors and scalars, JSON for
// structs and maps.
func detectContentType(data any) string {
	if data == nil {
		return textPlainCharset
	}

	// errorWriteback renders an error as its plain message rather than as JSON,
	// so the declared content type has to agree with that body.
	if _, isError := data.(error); isError {
		return textPlainCharset
	}

	t := reflect.TypeOf(data)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Struct, reflect.Map:
		return applicationJsonCharset
	default:
		return textPlainCharset
	}
}

// catchRuntimePanic turns a recovered panic into the response. It implements
// the strict leak-prevention policy: only framework-generated aborts and values
// handed to a registered PanicProcessor reach the client verbatim; anything
// else is an unexpected crash, logged server-side and answered with a generic
// 500 so internal details never leak.
func (e *Engine) catchRuntimePanic(rw ResponseWriter) {
	err := recover()
	if err == nil {
		return
	}

	// A committed body can no longer be rewritten; only the recorded status
	// (or the default 200) can still be flushed.
	if rw.written() {
		rw.writeHeader()
		return
	}

	// Once a panic processor is registered it owns rendering entirely, for
	// displayable aborts and genuine crashes alike.
	if e.panicProcessor != nil {
		payload, httpStatus := e.panicProcessor(err)
		bytes := errorWriteback(rw, payload)
		rw.WriteHeader(int(httpStatus))
		rw.Header().Set(contentType, detectContentType(payload))
		rw.writeHeader()
		_, _ = rw.Write(bytes)
		return
	}

	// An explicit halt carries its own client-safe status and message.
	if halt, ok := err.(responseAbort); ok {
		rw.Header().Set(contentType, textPlainCharset)
		rw.WriteHeader(halt.status)
		rw.writeHeader()
		_, _ = rw.Write([]byte(halt.body))
		return
	}

	// Everything else is unexpected: log the full detail, hide it from the
	// client behind a generic 500.
	e.logRecoveredPanic(err)
	rw.Header().Set(contentType, textPlainCharset)
	rw.WriteHeader(http.StatusInternalServerError)
	rw.writeHeader()
	_, _ = rw.Write([]byte(http.StatusText(http.StatusInternalServerError)))
}

// logRecoveredPanic records an unexpected panic without exposing it.
//
// It routes through the engine's Logger so the sink honors SetLogger; the
// recovered value and the goroutine stack are recorded as structured fields so a
// leak-free 500 stays debuggable in any backend.
func (e *Engine) logRecoveredPanic(err any) {
	e.logger.Error("recovered panic", "error", err, "stack", string(debug.Stack()))
}
