// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"bytes"
	"compress/gzip"
	"strings"
)

// Compression levels for the Compress middleware. These mirror the constants
// of compress/gzip but are re-exported so callers do not need to import that
// package directly.
const (
	// NoCompression store-only: framing bytes, no deflate work
	NoCompression = gzip.NoCompression
	// BestSpeed level 1: fastest, smallest ratio
	BestSpeed = gzip.BestSpeed
	// BestCompression level 9: slowest, best ratio
	BestCompression = gzip.BestCompression
	// DefaultCompression the balanced default of gzip (level 6)
	DefaultCompression = gzip.DefaultCompression
	// HuffmanOnly entropy coding only: slight reduction, near-zero cost
	HuffmanOnly = gzip.HuffmanOnly
)

// minCompressSize is the threshold below which compression is skipped. Small
// responses gain little from compression and the CPU cost is not worth it. The
// rest are the wire fragments this middleware negotiates with clients.
const (
	// smallest body worth deflating, in bytes
	minCompressSize = 1024
	// request header listing encodings the client supports
	acceptEncoding = "Accept-Encoding"
	// response header declaring the encoding applied
	contentEncoding = "Content-Encoding"
	// the one encoding token this middleware speaks
	encodingGzip = "gzip"
)

// Compress returns a middleware that gzip-compresses the response body when
// beneficial. It checks three conditions before compressing:
//
//  1. The client sent Accept-Encoding: gzip
//  2. The response Content-Type is compressible (text, JSON, XML, etc.)
//  3. The response body is at least minCompressSize bytes
//
// Compression happens after c.Next() returns, so the handler has already
// produced its result. The middleware replaces c.bytes with the compressed
// data and sets Content-Encoding: gzip.
//
// Usage:
//
//	gbl.Use(gbl.Compress(gbl.DefaultCompression))
//
// Or for a specific group:
//
//	api := gbl.Group("/api")
//	api.GroupUse(gbl.Compress(gbl.BestSpeed))
func Compress(level int) HandlerFunc {
	return func(c *Context) {
		c.Next()

		// Only compress if the handler produced a result and nothing wrote yet
		if !c.handled || c.writer.written() {
			return
		}

		// Check if client accepts gzip
		if !strings.Contains(c.Request.Header.Get(acceptEncoding), encodingGzip) {
			return
		}

		// Check if content type is compressible
		if !isCompressible(c.contentType) {
			return
		}

		// Check if response is large enough to benefit from compression
		if len(c.bytes) < minCompressSize {
			return
		}

		// Compress the response body
		var buf bytes.Buffer
		gw, err := gzip.NewWriterLevel(&buf, level)
		if err != nil {
			// Invalid compression level, skip compression
			return
		}

		if _, err := gw.Write(c.bytes); err != nil {
			_ = gw.Close() // Best-effort cleanup; write already failed
			return
		}

		if err := gw.Close(); err != nil {
			return
		}

		// Release the original pooled buffer before replacing c.bytes
		if c.responseBuf != nil {
			responseBufferPool.Put(c.responseBuf)
			c.responseBuf = nil
		}

		// Replace with compressed data
		c.bytes = buf.Bytes()
		c.writer.Header().Set(contentEncoding, encodingGzip)
		c.writer.Header().Set(vary, acceptEncoding)
	}
}

// isCompressible reports whether the given Content-Type benefits from
// compression. Text-based formats (JSON, HTML, XML, plain text, CSS, JS) are
// compressible; binary formats (images, video, audio, already-compressed files)
// are not.
func isCompressible(contentType string) bool {
	if contentType == empty {
		return false
	}

	// Extract the MIME type without parameters (e.g., "application/json;
	// charset=utf-8" becomes "application/json")
	mime := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))

	// Compressible MIME types
	switch mime {
	case applicationJson,
		"application/xml",
		"text/html",
		"text/plain",
		"text/xml",
		"text/css",
		"text/javascript",
		"application/javascript",
		"application/x-javascript",
		"application/xhtml+xml",
		"application/rss+xml",
		"application/atom+xml",
		"application/svg+xml",
		"image/svg+xml":
		return true
	}

	// Compress any text/* type not explicitly listed
	if strings.HasPrefix(mime, "text/") {
		return true
	}

	// Do not compress binary types (images, video, audio, PDF, etc.)
	return false
}
