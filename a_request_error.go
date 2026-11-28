// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"errors"
	"io"
	"net/http"
	"os"
)

// This file maps request parsing failures to client-safe responses.

// requestClientError marks malformed client input whose message may be returned
// with HTTP 400. requestInternalError marks a body-read failure; its details are
// for server logs only and must be answered with a generic HTTP 500.
type requestClientError struct {
	// err is the client-safe parse failure.
	err error
}

// Error returns the client-safe parse message.
func (e *requestClientError) Error() string { return e.err.Error() }

// Unwrap exposes the underlying parse error for errors.Is and errors.As.
func (e *requestClientError) Unwrap() error { return e.err }

// requestInternalError marks a server-side body read or storage failure.
type requestInternalError struct {
	// err is retained for server logs and hidden from the client.
	err error
}

// Error returns the internal failure detail for server-side logging.
func (e *requestInternalError) Error() string { return e.err.Error() }

// Unwrap exposes the underlying failure for errors.Is and errors.As.
func (e *requestInternalError) Unwrap() error { return e.err }

// requestErrorResponse is the handler boundary for parse failures. It preserves
// the established 413 and client-safe 400 messages while ensuring internal I/O
// details never reach the client.
func requestErrorResponse(err error) (status int, body string) {
	if errors.Is(err, requestBodyTooLargeError) {
		return http.StatusRequestEntityTooLarge, http.StatusText(http.StatusRequestEntityTooLarge)
	}
	if clientError, ok := errors.AsType[*requestClientError](err); ok {
		return http.StatusBadRequest, clientError.Error()
	}
	return http.StatusInternalServerError, http.StatusText(http.StatusInternalServerError)
}

// classifyBodyReadError separates client truncation and size violations from
// internal body I/O failures across every request parser.
func classifyBodyReadError(err error) error {
	if errors.Is(err, requestBodyTooLargeError) {
		return requestBodyTooLargeError
	}
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		return requestBodyTooLargeError
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return &requestClientError{err: err}
	}
	return &requestInternalError{err: err}
}

// classifyMultipartParseError keeps temporary-file and disk failures server-side.
func classifyMultipartParseError(err error) error {
	if _, ok := errors.AsType[*os.PathError](err); ok {
		return &requestInternalError{err: err}
	}
	return &requestClientError{err: err}
}
