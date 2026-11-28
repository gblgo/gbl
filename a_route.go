// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"context"
	"fmt"
	"go/token"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// isGoIdentifier reports whether s is a valid, non-keyword Go identifier: a
// letter or underscore first, then letters, digits or underscores. Path
// parameter names bind handler arguments by name, so anything else could never
// match. Go keywords are lexically identifier-shaped but cannot be parameter
// names, so they are rejected here as well.
func isGoIdentifier(s string) bool {
	if s == empty || token.Lookup(s).IsKeyword() {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_' || unicode.IsLetter(r):
		case i > 0 && unicode.IsDigit(r):
		default:
			return false
		}
	}
	return true
}

// httpMethod is the internal enum of supported HTTP verbs, indexed against
// methodNames and the per-method route trees. Its values are unexported, and it
// never appears in a public signature — callers always speak in plain strings.
type httpMethod uint8

// Constants used only by this file: the wire spelling of each method, shared
// between the names table and methodOf switch so a new verb is written once,
// and the path parameter braces.
const (
	// wire spelling of GET, shared by the names table and methodOf
	nameMethodGet = "GET"
	// wire spelling of HEAD
	nameMethodHead = "HEAD"
	// wire spelling of POST
	nameMethodPost = "POST"
	// wire spelling of PUT
	nameMethodPut = "PUT"
	// wire spelling of PATCH
	nameMethodPatch = "PATCH"
	// wire spelling of DELETE
	nameMethodDelete = "DELETE"
	// wire spelling of OPTIONS
	nameMethodOptions = "OPTIONS"
	// opens a named path parameter, as in /user/{id}
	leftCurlyBrace = "{"
	// closes a named path parameter
	rightCurlyBrace = "}"
)

// methodNames holds the wire spelling of every supported method, indexed by httpMethod.
var methodNames = [methodCount]string{
	methodGet:     nameMethodGet,
	methodHead:    nameMethodHead,
	methodPost:    nameMethodPost,
	methodPut:     nameMethodPut,
	methodPatch:   nameMethodPatch,
	methodDelete:  nameMethodDelete,
	methodOptions: nameMethodOptions,
}

// methodOf resolves an HTTP method name, reporting false when GBL does not
// implement that verb at all. HTTP methods are case-sensitive tokens, so only
// their exact wire spelling is accepted.
func methodOf(name string) (httpMethod, bool) {
	switch len(name) {
	case len(nameMethodGet):
		if equalMethodName(name, nameMethodGet) {
			return methodGet, true
		}
		if equalMethodName(name, nameMethodPut) {
			return methodPut, true
		}
	case len(nameMethodHead):
		if equalMethodName(name, nameMethodHead) {
			return methodHead, true
		}
		if equalMethodName(name, nameMethodPost) {
			return methodPost, true
		}
	case len(nameMethodPatch):
		if equalMethodName(name, nameMethodPatch) {
			return methodPatch, true
		}
	case len(nameMethodDelete):
		if equalMethodName(name, nameMethodDelete) {
			return methodDelete, true
		}
	case len(nameMethodOptions):
		if equalMethodName(name, nameMethodOptions) {
			return methodOptions, true
		}
	}
	return 0, false
}

// equalMethodName compares the case-sensitive HTTP method token without allocating.
func equalMethodName(name, upper string) bool {
	return name == upper
}

// isBodyMethod reports whether GBL parses a request body for this method.
//
// GET, HEAD, DELETE and OPTIONS never touch the body, whatever Content-Type the
// client sent. PATCH joins POST and PUT here because partial updates are its
// whole purpose.
func (m httpMethod) isBodyMethod() bool {
	return m == methodPost || m == methodPut || m == methodPatch
}

// routeHandler is a compiled route: the reflection handle to the user function,
// the content type chosen for its return value, the recovered parameter names,
// the precomputed bind plan and the frozen middleware chain. Built once in
// generateRoute and reused verbatim by every matching request.
type routeHandler struct {
	// v is the reflected handler function
	v reflect.Value
	// paramNames is parallel to the handler parameter list
	paramNames []string
	// pathParams records each dynamic segment position and its route-specific
	// name, allowing structurally shared variable nodes to bind different names
	pathParams []pathParam
	// this route frozen request chain: the engine global middleware,
	// then the route group, then the terminal routeEndpoint. It is
	// assembled once at startup and reused verbatim by every request.
	handlers []HandlerFunc

	// plan is the immutable parameter-binding blueprint.
	plan bindPlan

	// contentType is the provisional media type derived from the return type.
	contentType string
	// t is the reflected handler function type
	t reflect.Type
	// engine lifetime used to cancel SSE streams during shutdown
	streamCtx context.Context
	// server write timeout used by SSE deadline renewal
	writeTimeout time.Duration
}

// dealPath splits path into per-segment metadata and a normalized route path.
//
// Grammar: a path is '/'-separated segments. A segment is either a literal or a
// variable written as ":name" or "{name}" — both forms bind identically. The
// marker must lead the segment and appear exactly once: no nesting ("::", "{{"),
// no marker in the middle ("a:b", "a{b"), no empty name (":", "{}"), and braces
// must be paired. Variable names must be non-keyword Go identifiers and unique
// within the path, because one params map key cannot represent two positions.
// Any violation aborts startup; the reason is logged through the engine before
// the panic, so a bad registration stays explainable after deploy. In the error
// texts `***` stands for any offending characters.
func (e *engine) dealPath(path string) ([]segmentMeta, string) {
	path = strings.TrimSpace(path)
	if path == empty {
		e.failStartup("when registering a route, the path cannot be empty")
	}

	parts := strings.Split(path, slash)
	segmentMetas := make([]segmentMeta, 0, len(parts))
	segments := make([]string, 0, len(parts))
	var paramNames map[string]struct{}

	for _, part := range parts {
		if part == empty {
			continue
		}
		meta := segmentMeta{segment: part}
		switch {
		case strings.Contains(part, colon):
			if len(part) == 1 || part[0] != ':' ||
				strings.ContainsAny(part[1:], colon+leftCurlyBrace+rightCurlyBrace) {
				e.failStartup("when registering a route, the path cannot be '/:' " +
					"or '/:***:***:' or '/::', etc")
			}
			meta.isVar = true
			meta.paramName = part[1:]
		case strings.ContainsAny(part, leftCurlyBrace+rightCurlyBrace):
			if len(part) < 3 || part[0] != '{' || part[len(part)-1] != '}' ||
				strings.ContainsAny(part[1:len(part)-1], leftCurlyBrace+rightCurlyBrace) {
				e.failStartup("when registering a route, the path cannot be '/{' or '/{***{***{' " +
					"or '/{{', an unclosed '/{id', or an unpaired '}', etc")
			}
			meta.isVar = true
			meta.paramName = part[1 : len(part)-1]
		}

		// The name has to be usable as a Go parameter name, or it could never
		// match any handler argument: a stray character (":id}") would register
		// silently and bind the zero value on every request.
		if meta.isVar {
			if !isGoIdentifier(meta.paramName) {
				e.failStartup("when registering a route, the parameter name in '/" + part +
					"' must be a valid Go identifier, like ':id' or '{id}'; Go keywords are not allowed")
			}
			if paramNames == nil {
				paramNames = make(map[string]struct{})
			}
			if _, exists := paramNames[meta.paramName]; exists {
				e.failStartup("when registering a route, the parameter name '" + meta.paramName +
					"' appears more than once; each path position needs a distinct name")
			}
			paramNames[meta.paramName] = struct{}{}
		}

		segmentMetas = append(segmentMetas, meta)
		segments = append(segments, part)
	}

	if len(segmentMetas) == 0 {
		e.failStartup("when registering a route, the path cannot be empty or '/'")
	}

	return segmentMetas, slash + strings.Join(segments, slash)
}

// Name signals from runtime.FuncForPC that identify an unsupported handler
// shape: the suffix on the synthesized forwarder behind a bound method value,
// and the type-argument mask the runtime reports for a generic instantiation.
const (
	// suffix marking a bound method value such as hs.Lookup
	methodValueSuffix = "-fm"
	// type-argument mask in the name of a generic instantiation such as echo[int]
	genericArgsMask = "[...]"
)

// generateRoute turns every queued registration into live routing: it validates
// the handler, chooses a response content type from its return kind, builds the
// bind plan and recovers parameter names (aborting at startup if name-based
// binding would be unreliable), freezes the request middleware chain, and
// inserts the route into the method tree. It runs once, at startup, then
// releases the registration queue and DWARF cache.
func (e *engine) generateRoute() {
	// Collect every handler position so DWARF indexes only these file:line keys.
	e.dwarfTargets = make(map[string]struct{}, len(e.routeMetaSlice))
	for _, meta := range e.routeMetaSlice {
		v := reflect.ValueOf(meta.fn)
		if !v.IsValid() || v.Kind() != reflect.Func || v.IsNil() {
			continue
		}
		pc := v.Pointer()
		if fun := runtime.FuncForPC(pc); fun != nil {
			file, line := fun.FileLine(pc)
			if file != empty && line > 0 {
				e.dwarfTargets[file+colon+strconv.Itoa(line)] = struct{}{}
			}
		}
	}

	for _, meta := range e.routeMetaSlice {
		segmentMetas, path := e.dealPath(meta.path)
		method := meta.method
		methodName := methodNames[method]

		v := reflect.ValueOf(meta.fn)

		if !v.IsValid() || v.Kind() != reflect.Func || v.IsNil() {
			e.failStartup(fmt.Errorf("invalid route handler: %s %s, the method is illegal",
				methodName, meta.path))
		}

		t := v.Type()
		if t.NumOut() > 1 {
			e.failStartup(fmt.Errorf("route handler must have at most one return value: %s", meta.path))
		}

		// A handler registered as variadic must be invoked with CallSlice. GBL keeps
		// the public handler contract simple and rejects variadic handlers here.
		if t.IsVariadic() {
			e.failStartup("variadic handlers are not supported")
		}

		// Three handler shapes cannot yield reliable DWARF parameter names. A bound
		// method value resolves to a synthesized forwarder at <autogenerated>:1,
		// and a generic instantiation has no DWARF entry at its reported position.
		// A method expression includes a receiver that request binding cannot supply.
		// All are fixed by a plain wrapper function.
		if fun := runtime.FuncForPC(v.Pointer()); fun != nil {
			name := fun.Name()
			switch {
			case strings.HasSuffix(name, methodValueSuffix):
				e.failStartup(fmt.Errorf("gbl: handler for %s %s is not supported: method value",
					methodName, meta.path))
			case strings.Contains(name, genericArgsMask):
				e.failStartup(fmt.Errorf("gbl: handler for %s %s is not supported: generic instantiation",
					methodName, meta.path))
			}
		}
		if isMethodExpression(v, t) {
			e.failStartup(fmt.Errorf("gbl: handler for %s %s is not supported: method expression",
				methodName, meta.path))
		}

		handler := &routeHandler{
			contentType:  textPlainCharset,
			v:            v,
			t:            t,
			streamCtx:    e.streamCtx,
			writeTimeout: e.cfg.WriteTimeout,
		}

		if t.NumOut() > 0 {
			out, ok := derefType(t.Out(0))
			if !ok {
				e.failStartup(fmt.Sprintf("recursive pointer return type for %s %s: %s",
					methodName, meta.path, t.Out(0)))
			}
			if t.Out(0).Implements(responseErrorType) {
				handler.contentType = textPlainCharset
			} else {
				// A pointer may be nil; do not commit JSON before the runtime value is known.
				pointerComposite := t.Out(0).Kind() == reflect.Pointer &&
					(out.Kind() == reflect.Struct || out.Kind() == reflect.Map || out.Kind() == reflect.Array ||
						(out.Kind() == reflect.Slice && out.Elem().Kind() != reflect.Uint8))
				if pointerComposite {
					handler.contentType = empty
				} else {
					switch out.Kind() {
					case reflect.Struct, reflect.Map, reflect.Array:
						handler.contentType = applicationJsonCharset
					case reflect.Interface:
						// The runtime value determines the media type. Committing JSON before
						// the handler returns can mislabel a flushed scalar response.
						handler.contentType = empty
					case reflect.Slice:
						// []byte is written verbatim; every other slice is JSON-marshaled.
						if out.Elem().Kind() == reflect.Uint8 {
							handler.contentType = textPlainCharset
						} else {
							handler.contentType = applicationJsonCharset
						}
					default:
						handler.contentType = textPlainCharset
					}
				}
			}
		}

		if t.NumIn() > 0 {
			plan, err := buildBindPlan(t)
			if err != nil {
				e.failStartup(fmt.Sprintf("invalid handler parameter type for %s %s: %v",
					methodName, meta.path, err))
			}
			handler.plan = plan
			for i, class := range handler.plan.classes {
				if class == classWriter && !handler.plan.writerOK[i] {
					e.failStartup(fmt.Sprintf("response writer type is not supported: %s %s: %s",
						methodName, meta.path, handler.plan.types[i]))
				}
			}
			if handler.plan.hasSSE {
				if method != methodGet {
					e.failStartup(fmt.Sprintf("SSEWriter is only supported on GET routes: %s %s",
						methodName, meta.path))
				}
				if t.NumOut() > 0 {
					e.failStartup(fmt.Sprintf("SSE handler must not return a value: %s", meta.path))
				}
			}

			if handler.plan.needsNames {
				// Production uses DWARF; Go test binaries may recover from source when stripped.
				names, resolved, ambiguous, nameSource := e.resolveParamNames(handler.v)
				if ambiguous != empty {
					e.failStartup(fmt.Sprintf("gbl: handler for %s %s has ambiguous parameter names at %s; "+
						"put each handler on its own line or bind through a struct",
						methodName, meta.path, ambiguous))
				}
				// Both recovery paths preserve one slot per input, including
				// unnamed parameters. A different count means the entry belongs
				// to another signature and must not be padded or truncated into
				// a plausible but wrong binding plan.
				if resolved && len(names) != t.NumIn() {
					e.failStartup(fmt.Sprintf("gbl: handler for %s %s has a %s "+
						"input parameter count mismatch: got %d names for %d handler parameters; "+
						"rebuild with matching debug information or source",
						methodName, meta.path, nameSource, len(names), t.NumIn()))
				}
				handler.paramNames = normalizeParamNames(names, t.NumIn())
				if !resolved {
					e.failStartup(fmt.Errorf("gbl: handler for %s %s binds parameters by name, "+
						"but the binary has no usable DWARF parameter names and test source recovery "+
						"is unavailable. Production builds with `-ldflags \"-w\"` or `\"-s\"` are "+
						"unsupported because they strip DWARF; rebuild without them, or bind through "+
						"a struct / *struct parameter whose field names come from reflection",
						methodName, meta.path))
				}
			} else {
				handler.paramNames = normalizeParamNames(nil, t.NumIn())
			}
			handler.plan.prepareKeys(handler.paramNames)
		}

		// Freeze the request chain: engine middleware run outermost, then the
		// group, and the terminal endpoint invokes the business handler. This
		// slice is shared by every request routed here.
		chain := make([]HandlerFunc, 0, len(e.middlewares)+len(meta.mw)+1)
		chain = append(chain, e.middlewares...)
		chain = append(chain, meta.mw...)
		chain = append(chain, routeEndpoint)
		handler.handlers = chain

		// Determine if this is a static route (no path parameters)
		isStatic := true
		for _, seg := range segmentMetas {
			if seg.isVar {
				isStatic = false
				break
			}
		}

		if isStatic {
			// Static route: store in map for O(1) lookup. A repeat registration
			// would silently replace the first handler, so it aborts startup
			// exactly like a conflicting dynamic route does.
			statics := e.staticRoutes[method]
			if statics == nil {
				statics = make(map[string]*routeHandler)
				e.staticRoutes[method] = statics
			}
			if _, ok := statics[path]; ok {
				e.failStartup(fmt.Errorf("static route already exists: [%s %s]", methodName, path))
			}
			statics[path] = handler
		} else {
			// Dynamic route: insert into the segment trie
			e.insertNode(segmentMetas, path, method, handler)
		}
	}
	e.dwarfIndex = nil
	e.dwarfTargets = nil
	e.routeMetaSlice = nil
}

// isMethodExpression reports whether v is a method expression such as T.M. Its
// runtime name ends in Type.Method and its reflected first parameter is the
// receiver, unlike a normal function whose name has only the package/function
// separator.
func isMethodExpression(v reflect.Value, t reflect.Type) bool {
	if t.NumIn() == 0 {
		return false
	}
	fun := runtime.FuncForPC(v.Pointer())
	if fun == nil {
		return false
	}
	return isMethodExpressionName(fun.Name(), t.In(0))
}

// isMethodExpressionName parses a runtime function name independently of its
// reflected value. Unqualified names, such as a builtin's spelling, have no
// receiver and therefore cannot be method expressions.
func isMethodExpressionName(name string, receiverType reflect.Type) bool {
	lastDot := strings.LastIndex(name, ".")
	if lastDot < 0 {
		return false
	}
	prefix := name[:lastDot]
	receiverDot := strings.LastIndex(prefix, ".")
	if receiverDot < 0 {
		return false
	}
	receiver, ok := derefType(receiverType)
	if !ok {
		return false
	}
	runtimeReceiver := prefix[receiverDot+1:]
	runtimeReceiver = strings.TrimPrefix(runtimeReceiver, "(")
	runtimeReceiver = strings.TrimSuffix(runtimeReceiver, ")")
	runtimeReceiver = strings.TrimPrefix(runtimeReceiver, "*")
	return runtimeReceiver == receiver.Name()
}

// lookupParamNamesDWARF recovers handler parameter names only from binary DWARF.
// The index uses the runtime source position as a stable key for closures and
// function literals. Missing or stripped debug information fails route generation.
//
// The second return reports whether DWARF yielded names. The third return marks
// duplicate positions whose handler identity is ambiguous.
func (e *engine) lookupParamNamesDWARF(v reflect.Value) ([]string, bool, string) {
	if e.dwarfIndex == nil {
		index := buildDWARFIndexFor(e.dwarfTargets)
		if index == nil {
			// No readable DWARF: cache an empty map so later handlers do not reparse
			// a binary that will never yield names.
			index = map[string]dwarfParamNames{}
		}
		e.dwarfIndex = index
	}
	if len(e.dwarfIndex) == 0 {
		return nil, false, empty
	}

	pc := v.Pointer()
	funPc := runtime.FuncForPC(pc)
	if funPc == nil {
		return nil, false, empty
	}

	file, line := funPc.FileLine(pc)
	key := file + colon + strconv.Itoa(line)
	entry, ok := e.dwarfIndex[key]
	if !ok {
		return nil, false, empty
	}
	if entry.ambiguous {
		return nil, false, key
	}
	if len(entry.names) == 0 {
		return nil, false, empty
	}
	return entry.names, true, empty
}

// normalizeParamNames pads names to exactly count entries, one per parameter.
//
// Position-aware recovery preserves unnamed inputs as empty slots; callers that
// have no positional information may still supply a short or empty list. Every
// gap gets a positional placeholder ("arg0", "arg1", ...), keeping binding
// deterministic and avoiding a later index-out-of-range panic. The placeholder
// is an ordinary string, so a request key literally named "arg0" could still
// match it; name your parameters to avoid that.
func normalizeParamNames(names []string, count int) []string {
	normalized := make([]string, count)

	for i := range normalized {
		if i < len(names) {
			normalized[i] = names[i]
		}
		if normalized[i] == empty {
			normalized[i] = unnamedParamName(i)
		}
	}

	return normalized
}

// unnamedParamName is the placeholder for a parameter DWARF did not name.
func unnamedParamName(index int) string {
	return "arg" + strconv.Itoa(index)
}
