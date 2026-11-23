// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"bytes"
	"compress/gzip"
	"io"
	"strconv"
	"strings"
	"sync"
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
	// legacy spelling of gzip some old clients still send
	encodingXGzip = "x-gzip"
)

// Compress returns a middleware that gzip-compresses the response body when
// beneficial. It checks three conditions before compressing:
//
//  1. The client sent Accept-Encoding: gzip (a q=0 refusal is honored)
//  2. The response Content-Type is compressible (text, JSON, XML, etc.)
//  3. The response body is at least minCompressSize bytes
//
// Compression happens after c.Next() returns, so the handler has already
// produced its result. The middleware replaces c.bytes with the compressed
// data and sets Content-Encoding: gzip. Every response passing through also
// gains Vary: Accept-Encoding, whether compressed or not: whether a cached
// representation fits depends on the client's encoding, so caches must key
// on it. An invalid level yields a middleware that never compresses.
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
	// Validate the level once here rather than per request, and let the pool
	// below create writers at the same (now known-valid) level. This bound is
	// exactly gzip.NewWriterLevel's own, so the pool's New can ignore its error.
	if level < gzip.HuffmanOnly || level > gzip.BestCompression {
		return func(c *Context) { c.Next() }
	}

	// gzip.Writers are heavy (a deflate window each), so one middleware
	// instance recycles its own across requests; Reset rebinds a writer to
	// the current response buffer.
	writerPool := sync.Pool{
		New: func() any {
			gw, _ := gzip.NewWriterLevel(io.Discard, level)
			return gw
		},
	}

	return func(c *Context) {
		c.writer.Header().Add(vary, acceptEncoding)

		c.Next()

		// Only compress if the handler produced a result and nothing wrote yet
		if !c.handled || c.writer.written() {
			return
		}

		// Check if client accepts gzip
		if !acceptsGzip(c.Request.Header.Get(acceptEncoding)) {
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

		// A stacked Compress must not deflate twice: an inner instance has
		// already stamped the encoding, and a second pass would corrupt the
		// body while both instances claim success.
		if c.writer.Header().Get(contentEncoding) != empty {
			return
		}

		// Compress the response body into a pooled buffer
		buf := responseBufferPool.Get().(*bytes.Buffer)
		buf.Reset()
		gw := writerPool.Get().(*gzip.Writer)
		gw.Reset(buf)

		_, writeErr := gw.Write(c.bytes)
		// Close flushes the deflate footer; a writer returns to the pool
		// either way since Reset discards its state on the next use.
		closeErr := gw.Close()
		writerPool.Put(gw)
		if writeErr != nil || closeErr != nil {
			responseBufferPool.Put(buf)
			return
		}

		// The compressed buffer takes over the pooled slot, so the Context's
		// recycle releases it once the response is written; the staged body is
		// done and returns now.
		if c.responseBuf != nil {
			responseBufferPool.Put(c.responseBuf)
		}
		c.bytes = buf.Bytes()
		c.responseBuf = buf
		c.writer.Header().Set(contentEncoding, encodingGzip)
	}
}

// acceptsGzip reports whether an Accept-Encoding header permits gzip: it lists
// "gzip", the legacy "x-gzip" or "*" at a non-zero weight. A substring scan
// would honor an explicit refusal like "gzip;q=0", so tokens are matched whole
// and a q parameter of zero drops its token.
func acceptsGzip(header string) bool {
	for part := range strings.SplitSeq(header, comma) {
		token, params, _ := strings.Cut(part, ";")
		switch strings.ToLower(strings.TrimSpace(token)) {
		case encodingGzip, encodingXGzip, asterisk:
		default:
			continue
		}
		if hasZeroWeight(params) {
			continue
		}
		return true
	}
	return false
}

// hasZeroWeight reports whether a media range's parameters pin its quality
// value to zero, the RFC's way of explicitly refusing an encoding.
func hasZeroWeight(params string) bool {
	for param := range strings.SplitSeq(params, ";") {
		key, value, ok := strings.Cut(param, "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(key), "q") {
			continue
		}
		weight, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		return err == nil && weight <= 0
	}
	return false
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
