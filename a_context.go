// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
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

// routeEndpoint is the terminal link appended to every route chain. It is a
// shared, stateless function because the concrete handler is read from the
// per-request Context rather than captured here.
var routeEndpoint HandlerFunc = func(c *Context) { c.invokeHandler() }

// Context is the single handle threaded through the middleware chain and, when a
// handler declares it, injected as a parameter. It carries the request, the
// response writer, the matched route path parameters and a key/value bag that
// middleware use to hand data to later links and to the handler.
//
// A Context is pooled. It must not be retained past the request that produced
// it, exactly like the response writer it holds.
type Context struct {
	// this route merged chain, including the terminal endpoint
	handlers []HandlerFunc
	// serialized handler result
	bytes []byte
	// handler first return, boxed lazily by Result
	ret reflect.Value
	// per-request by-name binding sources; primed in injectedParameters
	src nameSources

	// writer is the framework pooled response sink. It stays unexported:
	// handlers own the response by declaring an http.ResponseWriter parameter,
	// and it must never be stored beyond the request.
	writer responseWriter
	// runtime media type chosen after the handler result is serialized
	contentType string
	// memoized boxed ret, filled only once Result is called
	result any
	// engine log sink, so middleware logging honors SetLogger
	logger Logger

	// Request is the active request visible to middleware and handlers.
	Request *http.Request
	// originalRequest retains the request whose body the framework parsed, even
	// when middleware replaces the public Request pointer.
	originalRequest *http.Request
	// cursor into handlers; abortIndex means "stopped"
	index int
	// params holds matched path parameters for this request
	params map[string]string
	// rh is the immutable-matched route handler
	rh *routeHandler
	// keys stores middleware request-scoped values
	keys map[string]any
	// pooled buffer for handler result serialization, released after Write
	responseBuf *bytes.Buffer
	// form owns request-scoped Form/PostForm maps until the request fully unwinds.
	form *pooledForm
	// sse is non-nil while an SSE handler is binding or running.
	sse *sseWriter

	// method is the normalized HTTP method used for routing and body policy.
	method httpMethod
	// the handler actually ran
	handled bool
	// SSE routes own their response and skip automatic return-value writeback.
	skipWriteback bool
	// compressTried prevents stacked middleware from repeating deflate work.
	compressTried bool
}

// maxPooledContextKeys bounds retained map capacity after unusually large requests.
const maxPooledContextKeys = 64

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
	c.sse = nil
	c.Request = r
	c.originalRequest = r
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
	c.skipWriteback = false
	c.compressTried = false
	c.logger = logger
	c.src = nameSources{}
	c.responseBuf = nil
	c.form = nil
	return c
}

// recycle clears every reference the Context holds before it returns to the
// pool, so a pooled Context never pins a finished request writer, body or
// middleware-set values.
func (c *Context) recycle() {
	cleanupMultipart(c.originalRequest)
	c.writer = nil
	c.sse = nil
	c.Request = nil
	c.originalRequest = nil
	c.handlers = nil
	c.params = nil
	c.rh = nil
	// Return ordinary keys maps to the pool; drop unusually large buckets.
	if c.keys != nil {
		keyCount := len(c.keys)
		clear(c.keys)
		if keyCount <= maxPooledContextKeys {
			keysPool.Put(c.keys)
		}
		c.keys = nil
	}
	c.bytes = nil
	c.result = nil
	c.ret = reflect.Value{}
	c.handled = false
	c.skipWriteback = false
	c.compressTried = false
	c.logger = nil
	c.src = nameSources{}
	// A panic between staging the result and the pipeline write would
	// otherwise drop the pooled buffer instead of recycling it.
	if c.responseBuf != nil {
		releaseResponseBuffer(c.responseBuf)
		c.responseBuf = nil
	}
	if c.form != nil {
		releasePooledForm(c.form)
		c.form = nil
	}
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

// Abort stops the chain and suppresses automatic result writeback. Any remaining
// links, the handler included, are skipped.
func (c *Context) Abort() {
	c.index = abortIndex
}

// Aborted reports whether the chain has been stopped.
func (c *Context) Aborted() bool { return c.index >= abortIndex }

// IsWritten reports whether Write has been called, including a zero-length write,
// letting post-handler middleware decide whether to override the default rendering.
func (c *Context) IsWritten() bool { return c.writer.written() }

// StatusCode reports the status that will be, or was, sent for this response:
// the recorded one once anything has set it, otherwise net/http 200 default.
// Because the staged body is committed only after the whole chain unwinds (even
// when a handler flushes its headers earlier), this prediction lets an access-log
// or metrics middleware read the effective status from c.Next() return point.
func (c *Context) StatusCode() int { return c.writer.status() }

// Size reports the logical response size: bytes already written or, while the
// staged result is pending, its length. HEAD can report a non-zero logical size
// even though net/http suppresses the body on the wire.
func (c *Context) Size() int {
	if n := c.writer.size(); n > 0 {
		return n
	}
	if c.writer.written() || !bodyAllowedForStatus(c.writer.status()) {
		return 0
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

// Get returns a value set earlier, typed by the caller: the type argument
// asserts the stored value, and a miss (absent key or different type) yields
// T zero value and false, so the caller decides how to handle it. Get[any]
// reads the value without asserting. For the value alone, see Value.
func (c *Context) Get[T any](key string) (T, bool) {
	var zero T
	if c == nil {
		return zero, false
	}
	stored, ok := c.keys[key]
	if !ok {
		return zero, false
	}
	if stored == nil && reflect.TypeFor[T]().Kind() == reflect.Interface {
		return zero, true
	}
	typed, ok := stored.(T)
	return typed, ok
}

// Value is Get ignoring the miss: it returns only the typed value, the zero
// value of T when the key is absent or holds a different type.
func (c *Context) Value[T any](key string) T {
	value, _ := c.Get[T](key)
	return value
}

// Result returns the matched handler boxed return value, or nil when the
// handler produced none. The value is boxed on the first call and memoized, so a
// request whose middleware never reads Result pays no interface allocation. It
// is valid only after c.Next() has run the handler.
func (c *Context) Result() any {
	if c.result == nil && c.ret.IsValid() {
		c.result = c.ret.Interface()
	}
	return c.result
}

// commitPendingResponse preserves a status recorded before an abort without
// allowing the abort to replace it.
func (c *Context) commitPendingResponse() {
	if c.writer.headed() && !c.writer.flushed() && !c.writer.written() && !c.writer.hijacked() {
		c.writer.writeHeader()
	}
}

// validateFinalAbortStatus rejects informational and out-of-range statuses before
// an abort can serialize or commit a response.
func validateFinalAbortStatus(status int) {
	if status < http.StatusOK || status > 999 {
		panic("gbl: abort status must be a final HTTP status between 200 and 999")
	}
}

// AbortWithStatusString denies the request with a plain-text body and stops the chain.
func (c *Context) AbortWithStatusString(status int, body string) {
	if c.writer.headed() || c.writer.flushed() || c.writer.written() || c.writer.hijacked() {
		c.commitPendingResponse()
		c.Abort()
		return
	}
	validateFinalAbortStatus(status)
	c.writer.Header().Set(contentType, textPlainCharset)
	c.writer.WriteHeader(status)
	c.writer.writeHeader()
	if bodyAllowedForStatus(status) {
		_, _ = c.writer.Write([]byte(body))
	}
	c.Abort()
}

// AbortWithStatusJSON denies the request with a JSON body and stops the chain.
// A marshaling failure is a server-side fault, so it aborts with a generic 500
// rather than echoing detail.
func (c *Context) AbortWithStatusJSON(status int, body any) {
	if c.writer.headed() || c.writer.flushed() || c.writer.written() || c.writer.hijacked() {
		c.commitPendingResponse()
		c.Abort()
		return
	}
	validateFinalAbortStatus(status)
	if !bodyAllowedForStatus(status) {
		c.writer.Header().Set(contentType, applicationJsonCharset)
		c.writer.WriteHeader(status)
		c.writer.writeHeader()
		c.Abort()
		return
	}
	data, err := json.Marshal(body)
	if err != nil {
		c.Abort()
		abort(http.StatusInternalServerError, responseSerializationFailed)
		return
	}
	c.writer.Header().Set(contentType, applicationJsonCharset)
	c.writer.WriteHeader(status)
	c.writer.writeHeader()
	if bodyAllowedForStatus(status) {
		_, _ = c.writer.Write(data)
	}
	c.Abort()
}

// Use appends global middleware to the default engine.
func Use(middleware ...HandlerFunc) { defaultEngine.use(middleware...) }

// use appends middleware that run for every route on this engine. They are
// combined with each route group middleware at route-generation time, the
// engine running outermost. Register all middleware before Run; calling Use
// after Run is claimed panics, because route chains are no longer editable.
func (e *engine) use(middleware ...HandlerFunc) {
	defer e.assertAssembling("Use")()
	e.middlewares = append(e.middlewares, middleware...)
}

// invokeHandler is the terminal chain link: it parses the request (when the
// route binds anything), builds the reflection argument list, calls the handler and
// stages the serialized result on the Context. The caller writes it after the
// chain unwinds, so a post-handler middleware can inspect Result and override.
func (c *Context) invokeHandler() {
	rh := c.rh

	if rh.plan.hasSSE {
		c.skipWriteback = true
		sw := newSSEWriter(c.writer, c.Request, rh.writeTimeout, c.logger, rh.streamCtx)
		c.sse = sw
		defer sw.complete()
	}

	var (
		form    *pooledForm
		files   filesMap
		bodyBuf *bytes.Buffer
		isJSON  bool
		err     error
	)
	if rh.plan.needsParse {
		// Anchor cleanup to the request actually being parsed, not a pointer that
		// post-handler middleware may later replace.
		c.originalRequest = c.Request
		form, files, bodyBuf, isJSON, err = parseRequestParameters(
			c.writer, c.Request, c.method, rh.plan.queryKeys,
		)
		c.form = form
		if err != nil {
			writeRequestError(err, c.logger)
			return
		}
	}

	// The pooled body buffer is needed only while binding; the decoded
	// parameters are independent copies, so release it before the handler runs.
	var body []byte
	if bodyBuf != nil {
		body = bodyBuf.Bytes()
		defer func() {
			if bodyBuf != nil {
				releaseBodyBuffer(bodyBuf)
			}
		}()
	}

	var in []reflect.Value
	if rh.t.NumIn() > 0 {
		var paramsBuf *[]reflect.Value
		paramsBuf, in = acquireParams(rh.t.NumIn())
		// Released on every exit path — error aborts included, and a panicking
		// handler, which the manual releases after Call could not cover.
		defer releaseParams(paramsBuf, in)
		if err = injectedParameters(c, form.urlValues(), files, body, isJSON, in); err != nil {
			// Binding errors are assembled from reflection internals (parameter
			// names, Go type strings, numeric parsing detail), so only a generic 400 is
			// returned and the detail stays in the server log.
			safeLog(func() { c.logger.Error("request binding failed", "error", err) })
			abort(http.StatusBadRequest, http.StatusText(http.StatusBadRequest))
			return
		}
		// Business values that opt into Validatable are checked before the
		// handler runs. The message is authored by the application developer, so
		// it is echoed as the 400 body: whoever wrote it owns what it discloses.
		if vErr := validateBusiness(in, rh.plan.validatableIndexes,
			rh.plan.validatablePointers); vErr != nil {
			abort(http.StatusBadRequest, vErr.Error())
			return
		}
	}

	if bodyBuf != nil {
		releaseBodyBuffer(bodyBuf)
		bodyBuf = nil
		body = nil
	}

	if rh.plan.hasSSE {
		if c.method == methodHead {
			c.sse.setHeaders()
			c.writer.WriteHeader(http.StatusOK)
			c.writer.writeHeader()
			c.handled = true
			return
		}
		stopHeartbeat, err := c.startSSE()
		if err != nil {
			safeLog(func() { c.logger.Error("SSE startup failed", "error", err) })
			if c.writer.flushed() {
				return
			}
			abort(http.StatusInternalServerError, http.StatusText(http.StatusInternalServerError))
		}
		if stopHeartbeat == nil {
			return
		}
		defer stopHeartbeat()
	}

	out := rh.v.Call(in)
	if len(out) > 0 {
		// Retain the raw return; Result boxes it only if a middleware asks.
		c.ret = out[0]
		if c.writer.hijacked() || !bodyAllowedForStatus(c.writer.status()) {
			c.writer.writeHeader()
			c.handled = true
			return
		}
		buf := responseBufferPool.Get().(*bytes.Buffer)
		buf.Reset()
		mediaType, convertErr := convertToBuffer(buf, out[0])
		if convertErr != nil {
			releaseResponseBuffer(buf)
			// Serialization failures are server side: only the fixed text leaks.
			abort(http.StatusInternalServerError, responseSerializationFailed)
			return
		}
		c.contentType = mediaType
		if !c.writer.flushed() && !c.writer.hijacked() &&
			c.writer.Header().Get(contentType) != mediaType {
			c.writer.Header().Set(contentType, mediaType)
		}
		c.bytes = buf.Bytes()
		c.responseBuf = buf
	}
	c.handled = true
}

// startSSE commits the stream and starts its heartbeat after binding succeeds.
// A stream canceled before its first write is a normal exit; the caller skips the
// business handler and deferred completion still runs.
func (c *Context) startSSE() (func(), error) {
	if err := c.sse.start(); err != nil {
		if cause := context.Cause(c.sse.ctx); cause != nil && errors.Is(err, cause) {
			return nil, nil
		}
		return nil, c.sse.fail(err)
	}
	return c.sse.startHeartbeat(), nil
}
