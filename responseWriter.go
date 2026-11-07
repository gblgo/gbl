// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import "net/http"

type ResponseWriter interface {
	http.ResponseWriter
	written() bool
	headed() bool
	writeHeader()
}

type gblResponseWriter struct {
	writtenStatus bool
	headedStatus  bool
	statusCode    int
	w             http.ResponseWriter
}

func (rw *gblResponseWriter) WriteHeader(code int) {
	if rw.headedStatus {
		return
	}

	rw.headedStatus = true
	rw.statusCode = code
}

func (rw *gblResponseWriter) Header() http.Header {
	return rw.w.Header()
}

func (rw *gblResponseWriter) Write(data []byte) (int, error) {
	rw.writtenStatus = true
	return rw.w.Write(data)
}

func (rw *gblResponseWriter) written() bool {
	return rw.writtenStatus
}

func (rw *gblResponseWriter) headed() bool {
	return rw.headedStatus
}

func (rw *gblResponseWriter) writeHeader() {
	if !rw.headedStatus {
		rw.w.WriteHeader(http.StatusOK)
		return
	}
	rw.w.WriteHeader(rw.statusCode)
}
