// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

type ctxTestPayload struct {
	Name string `json:"name"`
}

type ctxTestValidatable struct {
	Name string `json:"name"`
}

func (v *ctxTestValidatable) Validate() error {
	if v.Name == "invalid" {
		return errors.New("ctxTest invalid name")
	}
	return nil
}

type ctxTestValueValidatable struct {
	Calls *int
	Err   error
}

func (v ctxTestValueValidatable) Validate() error {
	(*v.Calls)++
	return v.Err
}

type ctxTestValidationProbe struct {
	Name  string
	Calls *[]string
	Err   error
}

func (v *ctxTestValidationProbe) Validate() error {
	*v.Calls = append(*v.Calls, v.Name)
	return v.Err
}

type ctxTestPanicValidatable struct{}

func (ctxTestPanicValidatable) Validate() error {
	panic("ctxTest validation secret")
}

func ctxTestNewContext(handlers ...HandlerFunc) (*Context, *responseTestWriter) {
	sink := responseTestNewWriter()
	rw := &pooledResponseWriter{w: sink}
	c := &Context{
		handlers: handlers,
		index:    -1,
		writer:   rw,
		Request:  httptest.NewRequest(http.MethodGet, "/", nil),
	}
	return c, sink
}

func TestContextNextRunsOnionChainOnce(t *testing.T) {
	var order []string
	endpoint := func(c *Context) {
		order = append(order, "endpoint")
		c.handled = true
	}
	c, _ := ctxTestNewContext(
		func(c *Context) {
			order = append(order, "outer:before")
			c.Next()
			order = append(order, "outer:after")
		},
		func(c *Context) {
			order = append(order, "inner:before")
			c.Next()
			order = append(order, "inner:after")
		},
		endpoint,
	)

	c.Next()
	c.Next()

	want := []string{"outer:before", "inner:before", "endpoint", "inner:after", "outer:after"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("Next order = %v, want %v", order, want)
	}
	if !c.handled {
		t.Fatal("terminal handler did not run")
	}
}

func TestContextAbortSkipsRemainingChain(t *testing.T) {
	var order []string
	c, _ := ctxTestNewContext(
		func(c *Context) {
			order = append(order, "outer:before")
			c.Next()
			order = append(order, "outer:after")
		},
		func(c *Context) {
			order = append(order, "aborter")
			c.Abort()
			order = append(order, "aborter:after")
		},
		func(c *Context) { order = append(order, "skipped") },
	)

	if c.Aborted() {
		t.Fatal("fresh context is aborted")
	}
	c.Next()
	c.Next()

	want := []string{"outer:before", "aborter", "aborter:after", "outer:after"}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("abort order = %v, want %v", order, want)
	}
	if !c.Aborted() {
		t.Fatal("Aborted = false after Abort")
	}
}

func TestContextAbortBeforeNextIsTerminal(t *testing.T) {
	called := false
	c, _ := ctxTestNewContext(func(*Context) { called = true })
	c.Abort()
	c.Next()
	c.Abort()
	c.Next()

	if called {
		t.Fatal("handler ran after Abort")
	}
	if !c.Aborted() {
		t.Fatal("repeated Abort cleared the stopped state")
	}
}

func TestContextSetGetAndValue(t *testing.T) {
	c, _ := ctxTestNewContext()

	c.Set("count", 7)
	if got, ok := c.Get[int]("count"); !ok || got != 7 {
		t.Fatalf("Get[int] = (%d, %v), want (7, true)", got, ok)
	}
	if got := c.Value[int]("count"); got != 7 {
		t.Fatalf("Value[int] = %d, want 7", got)
	}
	if got, ok := c.Get[any]("count"); !ok || got != 7 {
		t.Fatalf("Get[any] = (%v, %v), want (7, true)", got, ok)
	}

	c.Set("count", "seven")
	if got, ok := c.Get[int]("count"); ok || got != 0 {
		t.Fatalf("mismatched Get[int] = (%d, %v), want (0, false)", got, ok)
	}
	if got := c.Value[int]("count"); got != 0 {
		t.Fatalf("mismatched Value[int] = %d, want 0", got)
	}
	if got, ok := c.Get[string]("missing"); ok || got != "" {
		t.Fatalf("missing Get[string] = (%q, %v), want (\"\", false)", got, ok)
	}
	if got := c.Value[string]("missing"); got != "" {
		t.Fatalf("missing Value[string] = %q, want empty", got)
	}
}

func TestContextGetNilValues(t *testing.T) {
	c, _ := ctxTestNewContext()
	c.Set("nil", nil)
	var pointer *ctxTestPayload
	c.Set("pointer", pointer)

	if got, ok := c.Get[any]("nil"); !ok || got != nil {
		t.Fatalf("Get[any] for stored nil = (%v, %v), want (nil, true)", got, ok)
	}
	if got, ok := c.Get[*ctxTestPayload]("pointer"); !ok || got != nil {
		t.Fatalf("Get[*ctxTestPayload] = (%v, %v), want (nil, true)", got, ok)
	}
	if got, ok := c.Get[string]("nil"); ok || got != "" {
		t.Fatalf("Get[string] for stored nil = (%q, %v), want (\"\", false)", got, ok)
	}
	if got := c.Value[any]("nil"); got != nil {
		t.Fatalf("Value[any] for stored nil = %v, want nil", got)
	}
}

func TestContextGetOnNilContext(t *testing.T) {
	var c *Context
	if got, ok := c.Get[int]("key"); ok || got != 0 {
		t.Fatalf("nil Context Get = (%d, %v), want (0, false)", got, ok)
	}
	if got := c.Value[string]("key"); got != "" {
		t.Fatalf("nil Context Value = %q, want empty", got)
	}
}

func TestContextRecycleClearsRequestReferences(t *testing.T) {
	logger := &responseTestLogger{}
	c, _ := ctxTestNewContext()
	c.Set("secret", "request-value")
	c.params = map[string]string{"id": "42"}
	c.rh = &routeHandler{}
	c.bytes = []byte("staged")
	c.result = ctxTestPayload{Name: "staged"}
	c.ret = reflect.ValueOf(c.result)
	c.logger = logger
	c.responseBuf = new(bytes.Buffer)
	keys := c.keys

	c.recycle()

	if c.writer != nil || c.Request != nil || c.handlers != nil || c.params != nil || c.rh != nil {
		t.Fatalf("recycle retained request state: %+v", c)
	}
	if c.keys != nil || len(keys) != 0 {
		t.Fatalf("recycle did not clear and detach keys: %v", keys)
	}
	if c.bytes != nil || c.result != nil || c.ret.IsValid() || c.logger != nil || c.responseBuf != nil || c.sse != nil {
		t.Fatalf("recycle retained result or collaborator references: %+v", c)
	}
}

func TestContextResult(t *testing.T) {
	t.Run("no return", func(t *testing.T) {
		c, _ := ctxTestNewContext()
		if got := c.Result(); got != nil {
			t.Fatalf("Result = %v, want nil", got)
		}
	})

	t.Run("value is memoized", func(t *testing.T) {
		c, _ := ctxTestNewContext()
		payload := &ctxTestPayload{Name: "first"}
		c.ret = reflect.ValueOf(payload)
		first := c.Result()
		second := c.Result()
		if first != payload || second != payload {
			t.Fatalf("Result calls = (%v, %v), want original pointer", first, second)
		}
	})

	t.Run("typed nil remains a result", func(t *testing.T) {
		c, _ := ctxTestNewContext()
		var payload *ctxTestPayload
		c.ret = reflect.ValueOf(payload)
		got, ok := c.Result().(*ctxTestPayload)
		if !ok || got != nil {
			t.Fatalf("Result = (%v, %v), want typed nil pointer", got, ok)
		}
	})
}

func TestContextStatusAndStagedSize(t *testing.T) {
	c, _ := ctxTestNewContext()
	if got := c.StatusCode(); got != http.StatusOK {
		t.Fatalf("default StatusCode = %d, want 200", got)
	}
	if c.IsWritten() || c.Size() != 0 {
		t.Fatalf("fresh IsWritten/Size = (%v, %d), want (false, 0)", c.IsWritten(), c.Size())
	}

	c.bytes = []byte("staged")
	if c.Size() != 0 {
		t.Fatalf("Size before handler = %d, want 0", c.Size())
	}
	c.handled = true
	c.writer.WriteHeader(http.StatusCreated)
	if got := c.StatusCode(); got != http.StatusCreated {
		t.Fatalf("StatusCode = %d, want 201", got)
	}
	if c.IsWritten() {
		t.Fatal("WriteHeader marked the response written")
	}
	if got := c.Size(); got != len("staged") {
		t.Fatalf("staged Size = %d, want %d", got, len("staged"))
	}
}

func TestContextSizeAfterWrite(t *testing.T) {
	t.Run("zero length write suppresses staged size", func(t *testing.T) {
		c, _ := ctxTestNewContext()
		c.handled = true
		c.bytes = []byte("staged")
		if n, err := c.writer.Write(nil); n != 0 || err != nil {
			t.Fatalf("Write(nil) = (%d, %v), want (0, nil)", n, err)
		}
		if !c.IsWritten() || c.Size() != 0 {
			t.Fatalf("IsWritten/Size = (%v, %d), want (true, 0)", c.IsWritten(), c.Size())
		}
	})

	t.Run("partial write is the effective size", func(t *testing.T) {
		c, sink := ctxTestNewContext()
		c.handled = true
		c.bytes = []byte("much longer staged body")
		sink.writeN = 3
		sink.writeErr = errors.New("ctxTest short write")
		n, err := c.writer.Write([]byte("abcdef"))
		if n != 3 || !errors.Is(err, sink.writeErr) {
			t.Fatalf("Write = (%d, %v), want (3, short-write error)", n, err)
		}
		if got := c.Size(); got != 3 {
			t.Fatalf("Size = %d, want 3", got)
		}
	})
}

func TestContextSizeForBodyForbiddenStatuses(t *testing.T) {
	for _, status := range []int{http.StatusNoContent, http.StatusResetContent, http.StatusNotModified} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			c, _ := ctxTestNewContext()
			c.writer.WriteHeader(status)
			c.handled = true
			c.bytes = []byte("must not be sent")
			if got := c.Size(); got != 0 {
				t.Fatalf("Size for status %d = %d, want 0", status, got)
			}
		})
	}
}

func TestContextCommitPendingResponse(t *testing.T) {
	t.Run("unheaded is untouched", func(t *testing.T) {
		c, sink := ctxTestNewContext()
		c.commitPendingResponse()
		if len(sink.statuses) != 0 || c.writer.flushed() {
			t.Fatalf("unheaded response was committed: statuses=%v", sink.statuses)
		}
	})

	t.Run("staged final status is sent once", func(t *testing.T) {
		c, sink := ctxTestNewContext()
		c.writer.WriteHeader(http.StatusAccepted)
		c.commitPendingResponse()
		c.commitPendingResponse()
		if !reflect.DeepEqual(sink.statuses, []int{http.StatusAccepted}) {
			t.Fatalf("statuses = %v, want [202]", sink.statuses)
		}
	})

	t.Run("written response is not recommitted", func(t *testing.T) {
		c, sink := ctxTestNewContext()
		_, _ = c.writer.Write([]byte("body"))
		c.commitPendingResponse()
		if !reflect.DeepEqual(sink.statuses, []int{http.StatusOK}) {
			t.Fatalf("statuses = %v, want [200]", sink.statuses)
		}
	})

	t.Run("hijacked response is untouched", func(t *testing.T) {
		c, sink := ctxTestNewContext()
		c.writer.WriteHeader(http.StatusAccepted)
		c.writer.(*pooledResponseWriter).hijackedStatus = true
		c.commitPendingResponse()
		if len(sink.statuses) != 0 || !c.writer.headed() || c.writer.flushed() {
			t.Fatalf("hijacked response changed: statuses=%v", sink.statuses)
		}
	})
}

func TestContextAbortWithStatusString(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{name: "ok", status: http.StatusOK, body: "yes", want: "yes"},
		{name: "created", status: http.StatusCreated, body: "created", want: "created"},
		{name: "denied", status: http.StatusForbidden, body: "denied", want: "denied"},
		{name: "no content", status: http.StatusNoContent, body: "hidden", want: ""},
		{name: "reset content", status: http.StatusResetContent, body: "hidden", want: ""},
		{name: "not modified", status: http.StatusNotModified, body: "hidden", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			c, sink := ctxTestNewContext(
				func(c *Context) { c.AbortWithStatusString(tc.status, tc.body) },
				func(*Context) { called = true },
			)
			c.Next()
			if called {
				t.Fatal("handler ran after AbortWithStatusString")
			}
			if !c.Aborted() {
				t.Fatal("AbortWithStatusString did not abort the chain")
			}
			if got := c.StatusCode(); got != tc.status {
				t.Fatalf("StatusCode = %d, want %d", got, tc.status)
			}
			if got := sink.body.String(); got != tc.want {
				t.Fatalf("body = %q, want %q", got, tc.want)
			}
			if got := sink.header.Get(contentType); got != textPlainCharset {
				t.Fatalf("Content-Type = %q, want %q", got, textPlainCharset)
			}
		})
	}
}

func TestContextAbortWithStatusJSON(t *testing.T) {
	t.Run("object", func(t *testing.T) {
		c, sink := ctxTestNewContext(func(c *Context) {
			c.AbortWithStatusJSON(http.StatusBadRequest, map[string]string{"reason": "bad"})
		})
		c.Next()
		if !c.Aborted() || c.StatusCode() != http.StatusBadRequest {
			t.Fatalf("aborted/status = (%v, %d), want (true, 400)", c.Aborted(), c.StatusCode())
		}
		if got, want := sink.body.String(), `{"reason":"bad"}`; got != want {
			t.Fatalf("body = %q, want %q", got, want)
		}
		if got := sink.header.Get(contentType); got != applicationJsonCharset {
			t.Fatalf("Content-Type = %q, want %q", got, applicationJsonCharset)
		}
	})

	t.Run("nil is JSON null", func(t *testing.T) {
		c, sink := ctxTestNewContext(func(c *Context) {
			c.AbortWithStatusJSON(http.StatusBadRequest, nil)
		})
		c.Next()
		if got := sink.body.String(); got != "null" {
			t.Fatalf("body = %q, want null", got)
		}
	})

	t.Run("body forbidden statuses", func(t *testing.T) {
		for _, status := range []int{http.StatusNoContent, http.StatusResetContent, http.StatusNotModified} {
			c, sink := ctxTestNewContext(func(c *Context) {
				c.AbortWithStatusJSON(status, map[string]bool{"hidden": true})
			})
			c.Next()
			if sink.body.Len() != 0 || c.StatusCode() != status || !c.Aborted() {
				t.Fatalf("status %d: body=%q status=%d aborted=%v", status, sink.body.String(), c.StatusCode(), c.Aborted())
			}
		}
	})
}

func TestContextAbortWithStatusJSONSerializationFailure(t *testing.T) {
	e := responseTestNewEngine(t)
	var captured *Context
	handlerCalled := false
	e.use(func(c *Context) {
		captured = c
		c.AbortWithStatusJSON(http.StatusBadRequest, make(chan int))
	})
	e.register(methodGet, "/ctxTest", func() { handlerCalled = true })
	e.generateRoute()

	rec, escaped := responseTestServe(t, e, http.MethodGet, "/ctxTest", "", "")
	if escaped != nil {
		t.Fatalf("pipeline leaked a panic: %v", escaped)
	}
	responseTestCheckResponse(t, rec, http.StatusInternalServerError, responseSerializationFailed, textPlainCharset)
	if captured == nil {
		t.Fatal("middleware context was not captured")
	}
	if !captured.Aborted() {
		t.Error("serialization failure did not abort the context")
	}
	if handlerCalled {
		t.Error("handler ran after JSON serialization failed")
	}
}

func TestContextAbortForbiddenStatusSkipsJSONSerialization(t *testing.T) {
	c, sink := ctxTestNewContext(func(c *Context) {
		c.AbortWithStatusJSON(http.StatusNoContent, make(chan int))
	})
	if recovered := responseTestCapturePanic(c.Next); recovered != nil {
		t.Fatalf("AbortWithStatusJSON escaped panic for forbidden body: %v", recovered)
	}

	if !c.Aborted() {
		t.Fatal("AbortWithStatusJSON did not abort the chain")
	}
	if got := c.StatusCode(); got != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", got)
	}
	if sink.body.Len() != 0 {
		t.Fatalf("forbidden status body = %q, want empty", sink.body.String())
	}
}

func TestContextAbortRejectsInformationalStatus(t *testing.T) {
	for _, status := range []int{http.StatusEarlyHints, 99, 1000} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			c, _ := ctxTestNewContext(func(c *Context) {
				c.AbortWithStatusString(status, "not final")
			})
			if recovered := responseTestCapturePanic(c.Next); recovered == nil {
				t.Fatalf("AbortWithStatusString(%d) accepted a non-final status", status)
			}
		})
	}
}

func TestContextAbortMethodsPreservePriorState(t *testing.T) {
	states := []string{"staged status", "zero length write", "flushed", "hijacked"}
	for _, method := range []string{"string", "json"} {
		for _, state := range states {
			t.Run(method+"/"+state, func(t *testing.T) {
				c, sink := ctxTestNewContext(func(c *Context) {
					switch state {
					case "staged status":
						c.writer.Header().Set(contentType, "application/x-ctxTest")
						c.writer.WriteHeader(http.StatusAccepted)
					case "zero length write":
						_, _ = c.writer.Write(nil)
					case "flushed":
						c.writer.WriteHeader(http.StatusNoContent)
						c.writer.writeHeader()
					case "hijacked":
						c.writer.(*pooledResponseWriter).hijackedStatus = true
					}
					if method == "string" {
						c.AbortWithStatusString(http.StatusConflict, "replacement")
					} else {
						c.AbortWithStatusJSON(http.StatusConflict, make(chan int))
					}
				})
				c.Next()
				if !c.Aborted() {
					t.Fatal("abort did not stop the chain")
				}

				wantStatus := http.StatusConflict
				switch state {
				case "staged status":
					wantStatus = http.StatusAccepted
				case "zero length write":
					wantStatus = http.StatusOK
				case "flushed":
					wantStatus = http.StatusNoContent
				case "hijacked":
					wantStatus = http.StatusOK
				}
				if got := c.StatusCode(); got != wantStatus {
					t.Errorf("StatusCode = %d, want %d", got, wantStatus)
				}
				if sink.body.Len() != 0 {
					t.Errorf("body = %q, want empty", sink.body.String())
				}
				if state == "staged status" && sink.header.Get(contentType) != "application/x-ctxTest" {
					t.Errorf("staged Content-Type was replaced: %q", sink.header.Get(contentType))
				}
				if state == "hijacked" && len(sink.statuses) != 0 {
					t.Errorf("hijacked response was committed: %v", sink.statuses)
				}
			})
		}
	}
}

func TestContextAsValidatable(t *testing.T) {
	value := ctxTestValidatable{Name: "value"}
	if target, ok := asValidatable(reflect.ValueOf(value)); ok || target != nil {
		t.Fatalf("unaddressable pointer-receiver value = (%v, %v), want (nil, false)", target, ok)
	}
	addressable := reflect.ValueOf(&value).Elem()
	if target, ok := asValidatable(addressable); !ok || target != &value {
		t.Fatalf("addressable pointer-receiver value = (%v, %v), want original pointer", target, ok)
	}

	calls := 0
	valueReceiver := ctxTestValueValidatable{Calls: &calls}
	if target, ok := asValidatable(reflect.ValueOf(valueReceiver)); !ok || target.(ctxTestValueValidatable).Calls != &calls {
		t.Fatalf("value receiver = (%v, %v), want validatable value", target, ok)
	}

	var nilPointer *ctxTestValidatable
	if target, ok := asValidatable(reflect.ValueOf(nilPointer)); ok || target != nil {
		t.Fatalf("typed nil = (%v, %v), want (nil, false)", target, ok)
	}
	if target, ok := asValidatable(reflect.Value{}); ok || target != nil {
		t.Fatalf("invalid value = (%v, %v), want (nil, false)", target, ok)
	}
	if target, ok := asValidatable(reflect.ValueOf(struct{ Name string }{Name: "plain"})); ok || target != nil {
		t.Fatalf("plain struct = (%v, %v), want (nil, false)", target, ok)
	}
	secret := struct{ name string }{name: "hidden"}
	if target, ok := asValidatable(reflect.ValueOf(&secret).Elem().Field(0)); ok || target != nil {
		t.Fatalf("unexported field = (%v, %v), want (nil, false)", target, ok)
	}
}

func TestContextValidateBusinessOrderAndNilSkip(t *testing.T) {
	firstErr := errors.New("ctxTest first validation error")
	var calls []string
	first := &ctxTestValidationProbe{Name: "first", Calls: &calls, Err: firstErr}
	var nilProbe *ctxTestValidationProbe
	last := &ctxTestValidationProbe{Name: "last", Calls: &calls}
	values := []reflect.Value{reflect.ValueOf(first), reflect.ValueOf(nilProbe), reflect.ValueOf(last)}

	err := validateBusiness(values, []int{0, 1, 2}, []bool{false, false, false})
	if !errors.Is(err, firstErr) {
		t.Fatalf("validateBusiness error = %v, want %v", err, firstErr)
	}
	if !reflect.DeepEqual(calls, []string{"first"}) {
		t.Fatalf("validation calls = %v, want [first]", calls)
	}

	calls = nil
	first.Err = nil
	if err := validateBusiness(values, []int{0, 1, 2}, []bool{false, false, false}); err != nil {
		t.Fatalf("valid validateBusiness returned %v", err)
	}
	if !reflect.DeepEqual(calls, []string{"first", "last"}) {
		t.Fatalf("validation calls = %v, want [first last]", calls)
	}

	invalid := []reflect.Value{reflect.Value{}, reflect.ValueOf(last)}
	if err := validateBusiness(invalid, []int{0, 1}, []bool{false, false}); err != nil {
		t.Fatalf("invalid reflected slot returned %v", err)
	}
}

func TestContextValidatablePipeline(t *testing.T) {
	t.Run("pointer receiver validates value parameter before handler", func(t *testing.T) {
		for _, tc := range []struct {
			name        string
			body        string
			wantStatus  int
			wantBody    string
			wantHandler bool
		}{
			{name: "valid", body: `{"name":"valid"}`, wantStatus: http.StatusOK, wantBody: "valid", wantHandler: true},
			{name: "invalid", body: `{"name":"invalid"}`, wantStatus: http.StatusBadRequest, wantBody: "ctxTest invalid name"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				e := responseTestNewEngine(t)
				called := false
				e.register(methodPost, "/ctxTest", func(v ctxTestValidatable) string {
					called = true
					return v.Name
				})
				e.generateRoute()
				rec, escaped := responseTestServe(t, e, http.MethodPost, "/ctxTest", tc.body, applicationJsonCharset)
				if escaped != nil {
					t.Fatalf("pipeline leaked a panic: %v", escaped)
				}
				responseTestCheckResponse(t, rec, tc.wantStatus, tc.wantBody, textPlainCharset)
				if called != tc.wantHandler {
					t.Fatalf("handler called = %v, want %v", called, tc.wantHandler)
				}
			})
		}
	})

	t.Run("nil pointer is passed without validation", func(t *testing.T) {
		e := responseTestNewEngine(t)
		e.register(methodPost, "/ctxTest", func(v *ctxTestValidatable) string {
			if v == nil {
				return "nil"
			}
			return v.Name
		})
		e.generateRoute()
		rec, escaped := responseTestServe(t, e, http.MethodPost, "/ctxTest", "null", applicationJsonCharset)
		if escaped != nil {
			t.Fatalf("pipeline leaked a panic: %v", escaped)
		}
		responseTestCheckResponse(t, rec, http.StatusOK, "nil", textPlainCharset)
	})

	t.Run("validation panic is contained and logged", func(t *testing.T) {
		e := responseTestNewEngine(t)
		logger := &responseTestLogger{}
		e.setLogger(logger)
		called := false
		e.register(methodPost, "/ctxTest", func(ctxTestPanicValidatable) string {
			called = true
			return "unreachable"
		})
		e.generateRoute()
		rec, escaped := responseTestServe(t, e, http.MethodPost, "/ctxTest", `{}`, applicationJsonCharset)
		if escaped != nil {
			t.Fatalf("pipeline leaked a panic: %v", escaped)
		}
		responseTestCheckResponse(t, rec, http.StatusInternalServerError, http.StatusText(http.StatusInternalServerError), textPlainCharset)
		if called {
			t.Fatal("handler ran after validation panic")
		}
		if strings.Contains(rec.Body.String(), "ctxTest validation secret") {
			t.Fatal("validation panic leaked into the response")
		}
		entries := logger.snapshot()
		if len(entries) != 1 || entries[0].msg != "recovered panic" {
			t.Fatalf("log entries = %+v, want one recovered-panic entry", entries)
		}
	})
}
