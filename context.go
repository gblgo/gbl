// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"bytes"
	"encoding/json"
	"math"
	"net/http"
	"net/url"
	"reflect"
	"sync"
)

// HandlerFunc is one link in the middleware chain. It receives the per-request
// Context; calling c.Next() runs the remaining chain (and, at its end, the
// matched handler), while returning without calling c.Next() lets the driver
// continue. To stop the chain outright — after writing a denial, for example —
// call one of the Abort methods.
type HandlerFunc func(c *Context)

const (
	// far beyond any real chain length, so setting the cursor here ends
	// every in-flight Next loop at once
	abortIndex = math.MaxInt / 2
	// fixed 500 body written when encoding the handler result fails
	responseSerializationFailed = "response serialization failed"
)

// routeEndpoint is the terminal link appended to every route's chain. It is a
// shared, stateless function because the concrete handler is read from the
// per-request Context rather than captured here.
var routeEndpoint HandlerFunc = func(c *Context) { c.invokeHandler() }

// Context is the single handle threaded through the middleware chain and, when a
// handler declares it, injected as a parameter. It carries the request, the
// response writer, the matched route's path parameters and a key/value bag that
// middleware use to hand data to later links and to the handler.
//
// A Context is pooled. It must not be retained past the request that produced
// it, exactly like the response writer it holds.
//
// Field order is optimized for memory alignment: larger fields (slices, interfaces)
// come first, smaller scalars (bool, uint8) are grouped at the end to minimize padding.
type Context struct {
	// 24-byte fields (slice header, reflect.Value, struct with 3 maps)
	handlers []HandlerFunc // this route's merged chain, including the terminal endpoint
	bytes    []byte        // serialized handler result
	ret      reflect.Value // handler's first return, boxed lazily by Result
	src      nameSources   // per-request by-name binding sources; primed in injectedParameters

	// 16-byte fields (interface = type + data pointer, string = pointer + len)
	// writer is the framework's pooled response sink. It stays unexported:
	// handlers own the response by declaring an http.ResponseWriter parameter,
	// and it must never be stored beyond the request.
	writer      responseWriter
	contentType string // result content type chosen at registration
	result      any    // memoized boxed ret, filled only once Result is called
	logger      Logger // engine's log sink, so middleware logging honors SetLogger

	// 8-byte fields (pointers, int, map)
	Request     *http.Request
	index       int           // cursor into handlers; abortIndex means "stopped"
	params      map[string]string
	rh          *routeHandler
	keys        map[string]any
	responseBuf *bytes.Buffer // pooled buffer for handler result serialization, released after Write

	// 1-byte fields (grouped at end to minimize padding)
	method  httpMethod
	handled bool // the handler actually ran
}

// contextPool recycles Context objects across requests.
var contextPool = sync.Pool{
	New: func() any { return &Context{} },
}

// newContext fetches a Context primed for one request. handlers already holds
// the merged chain with the endpoint appended, so the caller only runs Next.
func newContext(w *pooledResponseWriter,
	r *http.Request,
	params map[string]string,
	rh *routeHandler,
	method httpMethod,
	logger Logger,
) *Context {
	c := contextPool.Get().(*Context)
	c.writer = w
	c.Request = r
	c.handlers = rh.handlers
	c.index = -1
	c.params = params
	c.rh = rh
	c.method = method
	c.keys = nil
	c.bytes = nil
	c.contentType = rh.contentType
	c.result = nil
	c.ret = reflect.Value{}
	c.handled = false
	c.logger = logger
	c.src = nameSources{}
	c.responseBuf = nil
	return c
}

// recycle clears every reference the Context holds before it returns to the
// pool, so a pooled Context never pins a finished request's writer, body or
// middleware-set values.
func (c *Context) recycle() {
	c.writer = nil
	c.Request = nil
	c.handlers = nil
	c.params = nil
	c.rh = nil
	// Return keys map to pool if it was used
	if c.keys != nil {
		clear(c.keys)
		keysPool.Put(c.keys)
		c.keys = nil
	}
	c.bytes = nil
	c.result = nil
	c.ret = reflect.Value{}
	c.logger = nil
	c.src = nameSources{}
	c.responseBuf = nil
	contextPool.Put(c)
}

// Next runs the remaining chain. It is normally called by a middleware to
// delegate to the next link; the request pipeline calls it once to start.
func (c *Context) Next() {
	c.index++
	for c.index < len(c.handlers) {
		c.handlers[c.index](c)
		c.index++
	}
}

// Abort stops the chain without writing anything. Any remaining links, the
// handler included, are skipped.
func (c *Context) Abort() {
	c.index = abortIndex
}

// Aborted reports whether the chain has been stopped.
func (c *Context) Aborted() bool { return c.index >= abortIndex }

// IsWritten reports whether a response body has already been committed, letting
// a post-handler middleware decide whether to override the default rendering.
func (c *Context) IsWritten() bool { return c.writer.written() }

// StatusCode reports the status that will be, or was, sent for this response:
// the recorded one once anything has set it, otherwise net/http's 200 default.
// Because the staged body is committed only after the whole chain unwinds, this
// prediction is what lets an access-log or metrics middleware read the effective
// status from c.Next()'s return point.
func (c *Context) StatusCode() int { return c.writer.status() }

// Size reports the effective response body length: the bytes already written,
// or — while the staged result has not been committed yet — the length of that
// staged body. It matches what the pipeline will actually send, so a middleware
// observing the response after c.Next() sees the true size.
func (c *Context) Size() int {
	if n := c.writer.size(); n > 0 {
		return n
	}
	if c.handled {
		return len(c.bytes)
	}
	return 0
}

// PathParam returns a matched path parameter by name. It is meant for middleware,
// which has no automatic binding; a business handler should bind path parameters
// by naming its arguments instead.
func (c *Context) PathParam(name string) string {
	value, _ := lookupString(c.params, name)
	return value
}

// keysPool recycles map[string]any instances used by Context.Set/Get.
// Benchmarking shows 34% speed improvement and complete elimination of
// map allocations (336 B/op → 0 B/op, 2 allocs/op → 0 allocs/op).
var keysPool = sync.Pool{
	New: func() any { return make(map[string]any, 4) },
}

// Set stores a value under key for later links and the handler.
func (c *Context) Set(key string, value any) {
	if c.keys == nil {
		c.keys = keysPool.Get().(map[string]any)
	}
	c.keys[key] = value
}

// Get returns a value set earlier, reporting whether the key was present. The
// value is an any; assert it to the type you stored. For a typed result without
// a manual assertion, see the package helpers Value and MustValue.
func (c *Context) Get(key string) (any, bool) {
	value, ok := c.keys[key]
	return value, ok
}

// Result returns the matched handler's boxed return value, or nil when the
// handler produced none. The value is boxed on the first call and memoized, so a
// request whose middleware never reads Result pays no interface allocation. It
// is valid only after c.Next() has run the handler.
func (c *Context) Result() any {
	if c.result == nil && c.ret.IsValid() {
		c.result = c.ret.Interface()
	}
	return c.result
}

// AbortWithStatusString denies the request with a plain-text body and stops the chain.
func (c *Context) AbortWithStatusString(status int, body string) {
	c.writer.Header().Set(contentType, textPlainCharset)
	c.writer.WriteHeader(status)
	_, _ = c.writer.Write([]byte(body))
	c.Abort()
}

// AbortWithStatusJSON denies the request with a JSON body and stops the chain.
// A marshaling failure is a server-side fault, so it aborts with a generic 500
// rather than echoing detail.
func (c *Context) AbortWithStatusJSON(status int, body any) {
	data, err := json.Marshal(body)
	if err != nil {
		abort(http.StatusInternalServerError, responseSerializationFailed)
		return
	}
	c.writer.Header().Set(contentType, applicationJsonCharset)
	c.writer.WriteHeader(status)
	_, _ = c.writer.Write(data)
	c.Abort()
}

// Value reads a middleware-set value with the requested type. It returns the
// zero value and false when the key is absent or holds a different type, so the
// caller decides how to handle a miss.
func Value[T any](c *Context, key string) (T, bool) {
	var zero T
	if c == nil {
		return zero, false
	}
	stored, ok := c.keys[key]
	if !ok {
		return zero, false
	}
	typed, ok := stored.(T)
	return typed, ok
}

// MustValue is Value ignoring the miss, returning only the typed value (zero on
// absence or type mismatch).
func MustValue[T any](c *Context, key string) T {
	value, _ := Value[T](c, key)
	return value
}

// Use appends global middleware to the default engine.
func Use(middleware ...HandlerFunc) { defaultEngine.use(middleware...) }

// use appends middleware that run for every route on this engine. They are
// combined with each route group's middleware at route-generation time, the
// engine's running outermost. Register all middleware before Run; calling Use
// afterward panics, because every route chain is already frozen.
func (e *engine) use(middleware ...HandlerFunc) {
	e.assertAssembling("Use")
	e.middlewares = append(e.middlewares, middleware...)
}

// invokeHandler is the terminal chain link: it parses the request (when the
// route binds anything), builds the reflection argument list, calls the handler and
// stages the serialized result on the Context. The caller writes it after the
// chain unwinds, so a post-handler middleware can inspect Result and override.
func (c *Context) invokeHandler() {
	rh := c.rh

	var (
		form    url.Values
		files   filesMap
		bodyBuf *bytes.Buffer
		isJSON  bool
		err     error
	)
	if rh.plan.needsParse {
		form, files, bodyBuf, isJSON, err = parseRequestParameters(c.writer, c.Request, c.method)
		if err != nil {
			writeRequestError(err)
			return
		}
	}

	// The pooled body buffer is needed only while binding; the decoded
	// parameters are independent copies, so release it when this handler returns.
	var body []byte
	if bodyBuf != nil {
		body = bodyBuf.Bytes()
		defer releaseBodyBuffer(bodyBuf)
	}

	var in []reflect.Value
	var paramsBuf *[]reflect.Value
	if rh.t.NumIn() > 0 {
		paramsBuf, in = acquireParams(rh.t.NumIn())
		if err = injectedParameters(c, form, files, body, isJSON, in); err != nil {
			// Binding errors are assembled from reflection internals (parameter
			// names, Go type strings, strconv detail), so only a generic 400 is
			// returned and the detail stays in the server log.
			releaseParams(paramsBuf, in)
			c.logger.Error("request binding failed", "error", err)
			abort(http.StatusBadRequest, http.StatusText(http.StatusBadRequest))
			return
		}
		// Business values that opt into Validatable are checked before the
		// handler runs. The message is authored by the application developer, so
		// it is echoed as the 400 body: whoever wrote it owns what it discloses.
		if vErr := validateBusiness(in, rh.plan.validatableIndexes); vErr != nil {
			releaseParams(paramsBuf, in)
			abort(http.StatusBadRequest, vErr.Error())
			return
		}
	}

	out := rh.v.Call(in)
	if paramsBuf != nil {
		releaseParams(paramsBuf, in)
	}
	if len(out) > 0 {
		buf := responseBufferPool.Get().(*bytes.Buffer)
		buf.Reset()
		err = convertToBuffer(buf, out[0])
		if err != nil {
			responseBufferPool.Put(buf)
			// Serialization failures are server side: only the fixed text leaks.
			abort(http.StatusInternalServerError, responseSerializationFailed)
			return
		}
		c.bytes = buf.Bytes()
		c.responseBuf = buf
		// Retain the raw return; Result boxes it only if a middleware asks.
		c.ret = out[0]
	}
	c.handled = true
}
