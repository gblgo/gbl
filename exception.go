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

// PanicProcessor is the user-installed hook that renders every recovered panic
// value into a response body and status. The body is any value the response
// writer can render (string, error, struct, map, ...); once registered it owns
// rendering for all recovered values, genuine crashes included, so it must avoid
// leaking internal error details to the client.
type PanicProcessor func(err any) (any, HttpStatus)

// RegisterPanicProcessor installs the panic renderer on the default engine. As
// soon as one is registered it owns the rendering of every recovered value,
// including unexpected crashes, so the caller takes on the responsibility of
// not echoing internal errors. It is an assembling decision and panics once
// the engine is serving.
func RegisterPanicProcessor(p PanicProcessor) {
	defaultEngine.setPanicProcessor(p)
}

// setPanicProcessor installs the panic renderer on this engine.
func (e *engine) setPanicProcessor(p PanicProcessor) {
	e.assertAssembling("RegisterPanicProcessor")
	e.panicProcessor = p
}

// responseAbort is the panic value used to end a request early with a specific
// status and a message that is safe to send to the client. The framework raises
// it for its own answers (404, 405, 413, 501, request-parse and serialization
// failures). Any other recovered value is treated as an unexpected crash and
// never echoed to the client.
type responseAbort struct {
	// status is the client-safe HTTP status code
	status int
	// body is the client-safe response text
	body string
}

// abort raises an internal responseAbort panic. Every framework-generated halt
// uses this form so the value is recognized as displayable rather than a crash.
func abort(status int, body string) {
	panic(responseAbort{status: status, body: body})
}

// typeRecognitionError is the fixed fallback body written when errorWriteback
// meets a reflected value of an unhandled kind.
var typeRecognitionError = []byte("Reflection Type Identification Exception")

// errorWriteback renders a panic payload as response bytes, choosing the form by
// its reflected kind.
func errorWriteback(err any) ([]byte, string) {
	return errorWritebackDepth(err, 0)
}

// errorWritebackDepth bounds recursive pointer/interface rendering.
func errorWritebackDepth(err any, depth int) ([]byte, string) {
	// A nil payload renders as an empty body: a PanicProcessor may deliberately
	// answer with no content, and reflection on nil would otherwise fall into
	// the unrecognized-kind default and emit the fixed error text.
	if err == nil {
		return []byte(empty), textPlainCharset
	}

	// A value implementing error is written back as its message. Reflection
	// instead walks the concrete type, and the usual errors.New and fmt.Errorf
	// values consist entirely of unexported fields, so the client would receive
	// an empty "{}" with no information at all. Keeping the message plain also
	// matches the 404, 405 and 501 replies the framework generates itself.
	if e, ok := err.(error); ok {
		return []byte(e.Error()), textPlainCharset
	}

	val := reflect.ValueOf(err)
	switch val.Kind() {
	case reflect.Struct, reflect.Map, reflect.Array, reflect.Slice:
		data, marshalErr := json.Marshal(val.Interface())
		if marshalErr != nil {
			return typeRecognitionError, textPlainCharset
		}
		return data, applicationJsonCharset
	case reflect.String:
		return []byte(val.String()), textPlainCharset
	case reflect.Pointer, reflect.Interface:
		if depth >= maxResponseDerefDepth {
			return typeRecognitionError, textPlainCharset
		}
		// A nil pointer/interface has no element to unwrap.
		if val.IsNil() {
			return []byte(empty), textPlainCharset
		}
		// Unwrap one layer and return the result, passing the element value
		// itself rather than the reflection wrapper around it.
		return errorWritebackDepth(val.Elem().Interface(), depth+1)
	case reflect.Bool:
		return strconv.AppendBool(nil, val.Bool()), textPlainCharset
	case reflect.Float32, reflect.Float64:
		return strconv.AppendFloat(nil, val.Float(), 'f', -1, val.Type().Bits()), textPlainCharset
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.AppendInt(nil, val.Int(), 10), textPlainCharset
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return strconv.AppendUint(nil, val.Uint(), 10), textPlainCharset
	case reflect.Complex64, reflect.Complex128:
		return []byte(strconv.FormatComplex(val.Complex(), 'f', -1, val.Type().Bits())), textPlainCharset
	default:
		return typeRecognitionError, textPlainCharset
	}
}

// writeResponseAbort renders a framework-controlled halt during request-level
// panic recovery. Middleware that recovers the halt itself owns the response.
func (e *engine) writeResponseAbort(rw responseWriter, halt responseAbort) {
	if rw.hijacked() || rw.flushed() || rw.written() {
		return
	}
	if rw.headed() {
		rw.writeHeader()
		return
	}
	rw.Header().Set(contentType, textPlainCharset)
	rw.WriteHeader(halt.status)
	rw.writeHeader()
	if bodyAllowedForStatus(halt.status) {
		_, _ = rw.Write([]byte(halt.body))
	}
}

// catchRuntimePanic turns a recovered panic into the response. It implements
// the strict leak-prevention policy: only framework-generated aborts and values
// handed to a registered PanicProcessor reach the client verbatim; anything
// else is an unexpected crash, logged server-side and answered with a generic
// 500 so internal details never leak.
func (e *engine) catchRuntimePanic(rw responseWriter) {
	err := recover()
	if err == nil {
		return
	}

	if rw.hijacked() || (rw.flushed() && !rw.written()) {
		if _, ok := err.(responseAbort); !ok {
			e.logRecoveredPanic(err)
		}
		return
	}

	// A committed body can no longer be rewritten; only the recorded status
	// (or the default 200) can still be flushed.
	if rw.written() {
		if _, ok := err.(responseAbort); !ok {
			e.logRecoveredPanic(err)
		}
		rw.writeHeader()
		return
	}

	// Once a panic processor is registered it owns rendering entirely, for
	// displayable aborts and genuine crashes alike.
	if e.panicProcessor != nil {
		payload, httpStatus := e.panicProcessor(err)
		// The hook is user code, so its status is not trusted: only final response
		// statuses are meaningful here. Protocol switches belong to an explicit
		// Upgrade/Hijack flow, not panic rendering.
		code := int(httpStatus)
		if code < http.StatusOK || code > 999 {
			code = http.StatusInternalServerError
		}
		body, mediaType := errorWriteback(payload)
		rw.Header().Set(contentType, mediaType)
		rw.replaceStatus(code)
		rw.writeHeader()
		if bodyAllowedForStatus(code) {
			_, _ = rw.Write(body)
		}
		return
	}

	// An explicit halt carries its own client-safe status and message.
	if halt, ok := err.(responseAbort); ok {
		e.writeResponseAbort(rw, halt)
		return
	}

	// Everything else is unexpected: log the full detail, hide it from the
	// client behind a generic 500.
	e.logRecoveredPanic(err)
	rw.Header().Set(contentType, textPlainCharset)
	rw.replaceStatus(http.StatusInternalServerError)
	rw.writeHeader()
	_, _ = rw.Write([]byte(http.StatusText(http.StatusInternalServerError)))
}

// logRecoveredPanic records an unexpected panic without exposing it.
//
// It routes through the engine Logger so the sink honors SetLogger; the
// recovered value and the goroutine stack are recorded as structured fields so a
// leak-free 500 stays debuggable in any backend.
func (e *engine) logRecoveredPanic(err any) {
	e.logger.Error("recovered panic", "error", err, "stack", string(debug.Stack()))
}
