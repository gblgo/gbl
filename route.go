// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// isGoIdentifier reports whether s is a valid Go identifier: a letter or
// underscore first, then letters, digits or underscores. Path parameter names
// bind handler arguments by name, so anything else could never match.
func isGoIdentifier(s string) bool {
	if s == empty {
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
// between the names table and methodOf's switch so a new verb is written once,
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
// implement that verb at all.
//
// The upper-casing keeps the lenient behavior GBL has always had, so a
// lowercase "get" is still treated as "GET".
func methodOf(name string) (httpMethod, bool) {
	switch strings.ToUpper(name) {
	case nameMethodGet:
		return methodGet, true
	case nameMethodHead:
		return methodHead, true
	case nameMethodPost:
		return methodPost, true
	case nameMethodPut:
		return methodPut, true
	case nameMethodPatch:
		return methodPatch, true
	case nameMethodDelete:
		return methodDelete, true
	case nameMethodOptions:
		return methodOptions, true
	default:
		return 0, false
	}
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
	// 24-byte fields (reflect.Value, slices)
	v          reflect.Value
	paramNames []string
	// this route's frozen request chain: the engine's global middleware,
	// then the route group's, then the terminal routeEndpoint. It is
	// assembled once at startup and reused verbatim by every request.
	handlers []HandlerFunc

	// bindPlan (152 bytes)
	plan bindPlan

	// 16-byte fields (string, interface)
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
// no marker in the middle ("a:b", "a{b"), no empty name (":", "{}"), and a "{"
// must be closed by a "}". Any
// violation aborts startup; the reason is logged through the engine before the
// panic, so a bad registration stays explainable after deploy. In the error
// texts `***` stands for any offending characters.
func (e *engine) dealPath(path string) ([]segmentMeta, string) {

	path = strings.TrimSpace(path)

	if path == empty {
		e.failStartup("when registering a route, the path cannot be empty")
	}

	parts := strings.Split(path, slash)
	segmentMetas := make([]segmentMeta, 0, len(parts))
	segments := make([]string, 0, len(parts))

	for _, part := range parts {
		if part == empty {
			continue
		}
		meta := segmentMeta{segment: part, isVar: false}
		if strings.Contains(part, colon) {
			if len(part) == 1 || strings.LastIndex(part, colon) != 0 {
				e.failStartup("when registering a route, the path cannot be '/:' or '/:***:***:' or '/::', etc")
			}
			meta.isVar = true
			meta.paramName = part[1:]
		}

		if strings.Contains(part, leftCurlyBrace) &&
			(len(part) == 1 || strings.LastIndex(part, leftCurlyBrace) != 0 ||
				!strings.HasSuffix(part, rightCurlyBrace)) {
			e.failStartup("when registering a route, the path cannot be '/{' or '/{***{***{' " +
				"or '/{{' or an unclosed '/{id', etc")
		}

		if strings.Contains(part, rightCurlyBrace) &&
			(len(part) == 1 || strings.Index(part, rightCurlyBrace)+1 != len(part)) {
			e.failStartup("when registering a route, the path cannot be '/}' or '/}***}***}' or '/}}', etc")
		}

		if strings.TrimSuffix(strings.TrimPrefix(part, leftCurlyBrace), rightCurlyBrace) == empty {
			e.failStartup("when registering a route, the path cannot be '/{}'")
		}

		if strings.Contains(part, leftCurlyBrace) {
			meta.isVar = true
			meta.paramName = part[1 : len(part)-1]
		}

		// The name has to be usable as a Go parameter name, or it could never
		// match any handler argument: a stray character (":id}") would register
		// silently and bind the zero value on every request.
		if meta.isVar && !isGoIdentifier(meta.paramName) {
			e.failStartup("when registering a route, the parameter name in '/" + part +
				"' must be a valid Go identifier, like ':id' or '{id}'")
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
// inserts the route into the method's tree. It runs once, at startup, then
// releases the registration queue and AST cache.
func (e *engine) generateRoute() {
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

		// Two handler shapes can never yield recoverable parameter names, so they
		// are rejected here rather than at name lookup. A bound method value such
		// as hs.Lookup resolves to a synthesized forwarder reported at
		// <autogenerated>:1, invisible to the AST and DWARF paths alike. A generic
		// instantiation recovers names from source during development, but its
		// DWARF carries no entry at the reported position, so it would break the
		// first sourceless deploy. Both are fixed by a plain wrapper function.
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

		handler := &routeHandler{
			contentType:  textPlainCharset,
			v:            v,
			t:            t,
			streamCtx:    e.streamCtx,
			writeTimeout: e.cfg.WriteTimeout,
		}

		if t.NumOut() > 0 {
			out := t.Out(0)
			for out.Kind() == reflect.Pointer {
				out = out.Elem()
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
			handler.plan = buildBindPlan(t)
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
				names, resolved := e.resolveParamNames(handler.v)
				if !resolved {
					names, resolved = e.lookupParamNamesDWARF(handler.v)
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
			// Dynamic route: insert into radix tree
			e.insertNode(segmentMetas, path, method, handler)
		}
	}
	e.astFileMap = nil
	e.astSpans = nil
	e.dwarfIndex = nil
	e.routeMetaSlice = nil
	e.generated = true
}

// resolveParamNames recovers a handler's parameter names from its own source file.
//
// The reflection package reports a function's parameter types but never its
// parameter names, and
// GBL binds request values by name, so the source recorded in the binary is parsed
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
// The second return reports whether real names were recovered. The caller acts
// on it: a handler whose binding needs names first falls back to the binary's
// DWARF, and only aborts startup (failStartup) if that is missing too, while a
// self-describing binding (a lone struct, whose fields come from reflection)
// falls back to positional placeholders and continues.
//
// A miss here is silent by design: in a sourceless production image EVERY
// handler misses on disk and resolves from DWARF instead, so a per-handler
// warning would be pure noise on the normal path.
func (e *engine) resolveParamNames(v reflect.Value) ([]string, bool) {
	pc := v.Pointer()
	funPc := runtime.FuncForPC(pc)
	if funPc == nil {
		return nil, false
	}

	file, line := funPc.FileLine(pc)

	af := e.astFileMap[file]
	if af == nil {
		parsed, err := parser.ParseFile(e.fest, file, nil, parser.ParseComments)
		if err != nil {
			// A failed file is deliberately not cached so every handler in it
			// retries on its own; a failed open costs microseconds.
			return nil, false
		}
		e.astFileMap[file] = parsed
		af = parsed
	}

	funcType := e.funcTypeAt(af, line)
	if funcType == nil || funcType.Params == nil {
		return nil, false
	}

	var paramNames []string
	for _, param := range funcType.Params.List {
		for _, name := range param.Names {
			paramNames = append(paramNames, name.Name)
		}
	}

	return paramNames, true
}

// lookupParamNamesDWARF recovers a handler's parameter names from the binary's
// own DWARF debug info, when the source on disk is unavailable.
//
// It is the production half of name binding: a sourceless image (multi-stage
// build, -trimpath, bare binary) has no .go file to parse, but the compiler still
// emits parameter names into DWARF by default. The index is keyed by the SAME
// source position the runtime reports — "<file>:<line>" — so lookup needs no
// symbol name and resolves closures and func literals exactly as the disk path
// does. Position keys are also immune to Go 1.27's merging and renaming of
// identical function literals, which makes symbol-keyed lookups unsafe.
//
// The index is built lazily on the first handler that misses on disk and cached
// on the engine, so a development process (source present) never pays for it. The
// second return reports whether DWARF yielded real names; it is false when the
// binary carries no DWARF (built with `-ldflags "-w"`/`"-s"`), letting the caller
// fail startup with guidance.
func (e *engine) lookupParamNamesDWARF(v reflect.Value) ([]string, bool) {
	if e.dwarfIndex == nil {
		index := buildDWARFIndex()
		if index == nil {
			// No readable DWARF: cache an empty map so later handlers do not reparse
			// a binary that will never yield names.
			index = map[string][]string{}
		}
		e.dwarfIndex = index
	}
	if len(e.dwarfIndex) == 0 {
		return nil, false
	}

	pc := v.Pointer()
	funPc := runtime.FuncForPC(pc)
	if funPc == nil {
		return nil, false
	}

	file, line := funPc.FileLine(pc)
	names, ok := e.dwarfIndex[file+colon+strconv.Itoa(line)]
	if !ok || len(names) == 0 {
		return nil, false
	}
	return names, true
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

// funcTypeAt returns the spans of a parsed file, collecting them on first use,
// and resolves the line against them.
func (e *engine) funcTypeAt(file *ast.File, line int) *ast.FuncType {
	spans, ok := e.astSpans[file]
	if !ok {
		spans = collectFuncSpans(e.fest, file)
		e.astSpans[file] = spans
	}
	return findFuncTypeAt(spans, line)
}

// findFuncTypeAt returns the signature of the innermost span enclosing the
// given line, or nil when none does. Preferring the smallest enclosing span
// makes a closure passed as a handler resolve to its own func literal rather
// than to the function that created it.
func findFuncTypeAt(spans []funcSpan, line int) *ast.FuncType {
	var (
		best     *ast.FuncType
		bestSpan = -1
	)
	for _, s := range spans {
		if line < s.start || line > s.end {
			continue
		}
		if span := s.end - s.start; bestSpan < 0 || span < bestSpan {
			best, bestSpan = s.funcType, span
		}
	}
	return best
}

// normalizeParamNames pads names to exactly count entries, one per parameter.
//
// Unnamed parameters contribute nothing to the recovered list, so it can arrive
// short or empty. Every gap gets a positional placeholder ("arg0", "arg1", ...),
// keeping binding deterministic and avoiding a later index-out-of-range panic.
// The placeholder is an ordinary string, so a request key literally named "arg0"
// could still match it; name your parameters to avoid that.
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
