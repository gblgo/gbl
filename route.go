// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"container/heap"
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"runtime"
	"sort"
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
// implement that verb at all. HTTP methods are ASCII tokens, so an ASCII fold
// preserves the lenient lowercase behavior without allocating an upper-cased
// copy on every request.
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

// equalMethodName compares an ASCII HTTP token case-insensitively without
// allocating. Non-ASCII bytes cannot match a supported method name.
func equalMethodName(name, upper string) bool {
	if len(name) != len(upper) {
		return false
	}
	for i := range len(name) {
		c := name[i]
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		if c != upper[i] {
			return false
		}
	}
	return true
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
// releases the registration queue and source-name caches.
func (e *engine) generateRoute() {
	// Collect every handler position before name resolution. The DWARF fallback
	// can then index only these file:line keys instead of the whole binary.
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

		// Three handler shapes can never yield recoverable parameter names, so they
		// are rejected here rather than at name lookup. A bound method value such
		// as hs.Lookup resolves to a synthesized forwarder reported at
		// <autogenerated>:1, invisible to the AST and DWARF paths alike. A generic
		// instantiation recovers names from source during development, but its
		// DWARF carries no entry at the reported position, so it would break the
		// first sourceless deploy. A method expression includes a receiver that
		// request binding cannot supply. All are fixed by a plain wrapper function.
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
			switch out.Kind() {
			case reflect.Struct, reflect.Interface, reflect.Map, reflect.Array:
				handler.contentType = applicationJsonCharset
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
				// Two independent sources for the parameter names, tried in order:
				//   1. the handler source file on disk — fresh, and the path used
				//      in development (`go run`, or a build run next to the source);
				//   2. DWARF debug info in the binary — the sourceless production
				//      path. The compiler keeps parameter names in DWARF even under
				//      -trimpath, so a bare binary still carries them.
				// Disk wins when both exist so a dev edit is never shadowed by the
				// DWARF baked into an older binary.
				names, resolved, ambiguous := e.resolveParamNames(handler.v)
				nameSource := "source"
				if ambiguous != empty {
					e.failStartup(fmt.Sprintf("gbl: handler for %s %s has ambiguous parameter names at %s; "+
						"put each handler on its own line or bind through a struct",
						methodName, meta.path, ambiguous))
				}
				if !resolved {
					names, resolved, ambiguous = e.lookupParamNamesDWARF(handler.v)
					nameSource = "DWARF"
					if ambiguous != empty {
						e.failStartup(fmt.Sprintf("gbl: handler for %s %s has ambiguous parameter names at %s; "+
							"put each handler on its own line or bind through a struct",
							methodName, meta.path, ambiguous))
					}
				}
				// Both recovery paths preserve one slot per input, including
				// unnamed parameters. A different count means the entry belongs
				// to another signature and must not be padded or truncated into
				// a plausible but wrong binding plan.
				if resolved && len(names) != t.NumIn() {
					e.failStartup(fmt.Sprintf("gbl: handler for %s %s has a %s input parameter count mismatch: "+
						"got %d names for %d handler parameters; rebuild with matching source or debug information",
						methodName, meta.path, nameSource, len(names), t.NumIn()))
				}
				handler.paramNames = normalizeParamNames(names, t.NumIn())
				if !resolved {
					e.failStartup(fmt.Errorf("gbl: handler for %s %s binds parameters by name, "+
						"but the parameter names are unavailable at runtime: no source on disk "+
						"and no DWARF debug info in the binary. The binary was likely built with "+
						"`-ldflags \"-w\"` or `\"-s\"`, which strip DWARF; rebuild without them so "+
						"gbl can read the names from the binary, or bind through a struct / "+
						"*struct parameter whose field names come from reflection",
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
	e.astFileMap = nil
	e.astSpans = nil
	e.astSpanIndex = nil
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

// resolveParamNames recovers handler parameter names from the source file.
//
// The reflection package reports function parameter types but never their
// names. GBL binds request values by name, so the source recorded in the binary is parsed
// once per file at startup. The handler is located by SOURCE POSITION, the line
// the runtime reports for its entry point, not by symbol name. A runtime symbol
// is a full module-qualified path ("github.com/you/pkg.Handler"). The AST records
// only the package clause ("pkg"). Comparing the two silently fails for every
// package except main, and never matches a func literal. Position matching fixes
// both and additionally resolves closures passed as handlers.
//
// Names cannot be recovered in two cases. First, the source no longer lives
// where it was compiled (a multi-stage image, -trimpath or a bare binary), so the
// file cannot be parsed. Second, the entry line belongs to generated code with no
// enclosing declaration, such as the wrapper thunk synthesized for a bound method
// value.
//
// The second return reports whether real names were recovered. The third return
// carries an ambiguous source position; the caller fails startup instead of
// guessing when multiple functions share that position.
func (e *engine) resolveParamNames(v reflect.Value) ([]string, bool, string) {
	pc := v.Pointer()
	funPc := runtime.FuncForPC(pc)
	if funPc == nil {
		return nil, false, empty
	}

	file, line := funPc.FileLine(pc)

	af := e.astFileMap[file]
	if af == nil {
		parsed, err := parser.ParseFile(e.fest, file, nil, parser.ParseComments)
		if err != nil {
			// A failed file is deliberately not cached so every handler in it
			// retries on its own; a failed open costs microseconds.
			return nil, false, empty
		}
		e.astFileMap[file] = parsed
		af = parsed
	}

	funcType, ambiguous := e.funcTypeAt(af, line)
	if ambiguous != empty {
		return nil, false, ambiguous
	}
	if funcType == nil || funcType.Params == nil {
		return nil, false, empty
	}

	paramNames := make([]string, 0, len(funcType.Params.List))
	for _, param := range funcType.Params.List {
		if len(param.Names) == 0 {
			// An unnamed field still occupies one input slot. Keeping an empty
			// placeholder prevents a later named parameter from shifting left.
			paramNames = append(paramNames, empty)
			continue
		}
		for _, name := range param.Names {
			paramNames = append(paramNames, name.Name)
		}
	}

	return paramNames, true, empty
}

// lookupParamNamesDWARF recovers handler parameter names from DWARF debug info
// in the binary when the source on disk is unavailable.
//
// It is the production half of name binding: a sourceless image (multi-stage
// build, -trimpath, bare binary) has no .go file to parse, but the compiler still
// emits parameter names into DWARF by default. The index is keyed by the SAME
// source position the runtime reports — "<file>:<line>" — so lookup needs no
// symbol name and resolves closures and func literals exactly as the disk path
// does. Position keys are also immune to Go 1.27 merging and renaming of
// identical function literals, which makes symbol-keyed lookups unsafe. During
// route generation the index is restricted to the precollected handler positions;
// direct callers without targets retain the full-index fallback.
//
// The second return reports whether DWARF yielded real names. The third return
// marks duplicate file:line entries whose handler identity is ambiguous.
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

// funcSpan is the inclusive source line range of one function declaration or
// literal, paired with its signature.
type funcSpan struct {
	// first line the enclosing function occupies
	start int
	// last line the enclosing function occupies
	end int
	// signature declared across this span
	funcType *ast.FuncType
}

// collectFuncSpans walks file once and records the line span of every function
// declaration and literal. A file hosting many handlers is then traversed a
// single time instead of once per handler.
func collectFuncSpans(fSet *token.FileSet, file *ast.File) []funcSpan {
	var spans []funcSpan
	ast.Inspect(file, func(n ast.Node) bool {
		var funcType *ast.FuncType
		switch fn := n.(type) {
		case *ast.FuncDecl:
			funcType = fn.Type
		case *ast.FuncLit:
			funcType = fn.Type
		default:
			return true
		}
		spans = append(spans, funcSpan{
			start:    fSet.Position(n.Pos()).Line,
			end:      fSet.Position(n.End()).Line,
			funcType: funcType,
		})
		return true
	})
	return spans
}

// funcSpanIndex is a sweep-line index over function spans. Between consecutive
// start/end events the innermost enclosing signature is constant, so a lookup
// is one binary search rather than a scan of every handler in the file.
type funcSpanIndex struct {
	// lines holds event boundaries for binary search.
	lines []int
	// funcTypes is parallel to lines with the innermost active signature.
	funcTypes []*ast.FuncType
	// ambiguous is parallel to lines and marks equal-sized active spans.
	ambiguous []bool
	// starts counts declarations beginning on each source line.
	starts map[int]int
}

// funcSpanEvent is one start or end boundary in the sweep-line index.
type funcSpanEvent struct {
	// line is the event source line.
	line int
	// index identifies the source span.
	index int
	// add distinguishes a start event from an end event.
	add bool
}

// funcSpanHeap keeps the shortest active span at the top.
type funcSpanHeap struct {
	// spans is the shared source span table.
	spans []funcSpan
	// ids holds the heap indexes into spans.
	ids []int
}

// Len returns the number of active heap entries.
func (h *funcSpanHeap) Len() int { return len(h.ids) }

// Less indicates whether the left span sorts first, preferring shorter spans and source order for ties.
func (h *funcSpanHeap) Less(i, j int) bool {
	left := h.spans[h.ids[i]]
	right := h.spans[h.ids[j]]
	leftLen, rightLen := left.end-left.start, right.end-right.start
	if leftLen == rightLen {
		return h.ids[i] < h.ids[j]
	}
	return leftLen < rightLen
}

// Swap exchanges two heap entries.
func (h *funcSpanHeap) Swap(i, j int) { h.ids[i], h.ids[j] = h.ids[j], h.ids[i] }

// Push adds a span index to the heap.
func (h *funcSpanHeap) Push(value any) {
	h.ids = append(h.ids, value.(int))
}

// Pop removes and returns the shortest active span index.
func (h *funcSpanHeap) Pop() any {
	last := len(h.ids) - 1
	id := h.ids[last]
	h.ids = h.ids[:last]
	return id
}

// newFuncSpanIndex builds the index in O(H log H), where H is the number of
// functions in the file, regardless of how many handlers later query it.
func newFuncSpanIndex(spans []funcSpan) *funcSpanIndex {
	if len(spans) == 0 {
		return &funcSpanIndex{starts: make(map[int]int)}
	}
	events := make([]funcSpanEvent, 0, len(spans)*2)
	starts := make(map[int]int, len(spans))
	for i, span := range spans {
		events = append(events,
			funcSpanEvent{line: span.start, index: i, add: true},
			funcSpanEvent{line: span.end + 1, index: i},
		)
		starts[span.start]++
	}
	sort.Slice(events, func(i, j int) bool { return events[i].line < events[j].line })

	index := &funcSpanIndex{starts: starts}
	active := make([]bool, len(spans))
	lengthCounts := make(map[int]int, len(spans))
	queue := &funcSpanHeap{spans: spans}

	for pos := 0; pos < len(events); {
		line := events[pos].line
		for pos < len(events) && events[pos].line == line {
			event := events[pos]
			length := spans[event.index].end - spans[event.index].start
			if event.add {
				active[event.index] = true
				lengthCounts[length]++
				heap.Push(queue, event.index)
			} else if active[event.index] {
				active[event.index] = false
				if lengthCounts[length] == 1 {
					delete(lengthCounts, length)
				} else {
					lengthCounts[length]--
				}
			}
			pos++
		}
		for queue.Len() > 0 && !active[queue.ids[0]] {
			heap.Pop(queue)
		}

		index.lines = append(index.lines, line)
		if queue.Len() == 0 {
			index.funcTypes = append(index.funcTypes, nil)
			index.ambiguous = append(index.ambiguous, false)
			continue
		}
		best := queue.ids[0]
		length := spans[best].end - spans[best].start
		index.funcTypes = append(index.funcTypes, spans[best].funcType)
		index.ambiguous = append(index.ambiguous, lengthCounts[length] > 1)
	}
	return index
}

// at returns the innermost signature for line and whether the position is ambiguous.
func (index *funcSpanIndex) at(line int) (*ast.FuncType, bool) {
	if index == nil || len(index.lines) == 0 || line < index.lines[0] {
		return nil, false
	}
	// Multiple declarations beginning on one line are ambiguous even when one
	// span is strictly smaller, matching findFuncTypeAt's historical contract.
	if index.starts[line] > 1 {
		return nil, true
	}
	pos := sort.Search(len(index.lines), func(i int) bool { return index.lines[i] > line }) - 1
	return index.funcTypes[pos], index.ambiguous[pos]
}

// funcTypeAt returns the spans of a parsed file, collecting them on first use,
// and resolves the line through a per-file index. Building the index once keeps
// H handler lookups in one file at O(H log H), not O(H^2).
func (e *engine) funcTypeAt(file *ast.File, line int) (*ast.FuncType, string) {
	spans, ok := e.astSpans[file]
	if !ok {
		spans = collectFuncSpans(e.fest, file)
		e.astSpans[file] = spans
	}
	index := e.astSpanIndex[file]
	if index == nil {
		index = newFuncSpanIndex(spans)
		e.astSpanIndex[file] = index
	}
	funcType, ambiguous := index.at(line)
	if ambiguous {
		pos := e.fest.Position(file.Pos())
		return nil, pos.Filename + colon + strconv.Itoa(line)
	}
	return funcType, empty
}

// findFuncTypeAt returns the signature of the innermost span enclosing the
// given line, or nil when none does. Preferring the smallest enclosing span
// makes a closure passed as a handler resolve to its own func literal rather
// than to the function that created it. A tie is ambiguous and must fail startup.
func findFuncTypeAt(spans []funcSpan, line int) (*ast.FuncType, bool) {
	var (
		best      *ast.FuncType
		bestSpan  = -1
		ambiguous bool
		starts    int
	)
	for _, s := range spans {
		if s.start == line {
			starts++
		}
		if line < s.start || line > s.end {
			continue
		}
		span := s.end - s.start
		switch {
		case bestSpan < 0 || span < bestSpan:
			best, bestSpan, ambiguous = s.funcType, span, false
		case span == bestSpan:
			ambiguous = true
		}
	}
	if starts > 1 {
		return nil, true
	}
	return best, ambiguous
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

// unnamedParamName is the placeholder for a parameter the source did not name.
func unnamedParamName(index int) string {
	return "arg" + strconv.Itoa(index)
}
