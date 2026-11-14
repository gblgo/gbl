// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"runtime"
	"strconv"
	"strings"
)

// httpMethod is the internal enum of supported HTTP verbs, indexed against
// methodNames and the per-method route trees. Its values are unexported, and it
// never appears in a public signature — callers always speak in plain strings.
type httpMethod uint8

// methodNames holds the wire spelling of every supported method, indexed by httpMethod.
var methodNames = [methodCount]string{
	methodGet:     "GET",
	methodHead:    "HEAD",
	methodPost:    "POST",
	methodPut:     "PUT",
	methodPatch:   "PATCH",
	methodDelete:  "DELETE",
	methodOptions: "OPTIONS",
}

// methodOf Resolves an HTTP method name, reporting false when GBL does not
// implement that verb at all.
//
// The upper-casing keeps the lenient behavior GBL has always had, so a
// lowercase "get" is still treated as "GET".
func methodOf(name string) (httpMethod, bool) {
	switch strings.ToUpper(name) {
	case "GET":
		return methodGet, true
	case "HEAD":
		return methodHead, true
	case "POST":
		return methodPost, true
	case "PUT":
		return methodPut, true
	case "PATCH":
		return methodPatch, true
	case "DELETE":
		return methodDelete, true
	case "OPTIONS":
		return methodOptions, true
	default:
		return 0, false
	}
}

// isBodyMethod Reports whether GBL parses a request body for this method.
//
// GET, HEAD, DELETE and OPTIONS never touch the body, whatever Content-Type the
// client sent. PATCH joins POST and PUT here because partial updates are its
// whole purpose.
func (m httpMethod) isBodyMethod() bool {
	return m == methodPost || m == methodPut || m == methodPatch
}

// routeHandler is a compiled route: the reflect handle to the user function,
// the content type chosen for its return value, the recovered parameter names,
// the precomputed bind plan and the frozen middleware chain. Built once in
// generateRoute and reused verbatim by every matching request.
type routeHandler struct {
	contentType string
	v           reflect.Value
	t           reflect.Type
	paramNames  []string
	plan        bindPlan
	// handlers are this route's frozen request chain: the engine's global
	// middleware, then the route group's, then the terminal routeEndpoint.
	// It is assembled once at startup and reused verbatim by every request.
	handlers []HandlerFunc
}

// dealPath splits path into per-segment metadata and a normalized route path.
//
// Grammar: a path is '/'-separated segments. A segment is either a literal or a
// variable written as ":name" or "{name}" — both forms bind identically. The
// marker must lead the segment and appear exactly once: no nesting ("::", "{{"),
// no marker in the middle ("a:b", "a{b"), and no empty name (":", "{}"). Any
// violation aborts startup; the reason is logged through the engine before the
// panic, so a bad registration stays explainable after deploy. In the error
// texts "***" stands for any offending characters.
func (e *engine) dealPath(path string) ([]segmentMeta, string) {

	path = strings.TrimSpace(path)

	if path == null {
		e.failStartup("when registering a route, the path cannot be empty")
	}

	parts := strings.Split(path, slash)
	segmentMetas := make([]segmentMeta, 0, len(parts))
	segments := make([]string, 0, len(parts))

	for _, part := range parts {
		if part == null {
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

		if strings.Contains(part, leftCurlyBrace) && (len(part) == 1 || strings.LastIndex(part, leftCurlyBrace) != 0) {
			e.failStartup("when registering a route, the path cannot be '/{' or '/{***{***{' or '/{{', etc")
		}

		if strings.Contains(part, rightCurlyBrace) && (len(part) == 1 || strings.Index(part, rightCurlyBrace)+1 != len(part)) {
			e.failStartup("when registering a route, the path cannot be '/}' or '/}***}***}' or '/}}', etc")
		}

		if strings.TrimSuffix(strings.TrimPrefix(part, leftCurlyBrace), rightCurlyBrace) == null {
			e.failStartup("when registering a route, the path cannot be '/{}'")
		}

		if strings.Contains(part, leftCurlyBrace) {
			meta.isVar = true
			meta.paramName = part[1 : len(part)-1]
		}

		segmentMetas = append(segmentMetas, meta)
		segments = append(segments, part)
	}

	if len(segmentMetas) == 0 {
		e.failStartup("when registering a route, the path cannot be empty or '/'")
	}

	return segmentMetas, slash + strings.Join(segments, slash)
}

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
			e.failStartup(fmt.Errorf("invalid route handler: %s %s, the method is illegal", methodName, meta.path))
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

		handler := &routeHandler{
			contentType: textPlainCharset,
			v:           v,
			t:           t,
		}

		if t.NumOut() > 0 {
			out := t.Out(0)
			switch out.Kind() {
			case reflect.Struct, reflect.Pointer, reflect.Interface, reflect.Map, reflect.Array:
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
			names, resolved := e.resolveParamNames(handler.v)
			handler.paramNames = normalizeParamNames(names, t.NumIn())

			// A name-dependent handler whose names could not be recovered would
			// otherwise bind path/query/JSON values to nothing and answer with wrong
			// data at request time. Fail loudly at startup instead, pointing at the
			// two fixes that keep the process honest: build without -trimpath, or
			// bind through a struct whose field names come from reflection.
			if handler.plan.needsNames && !resolved {
				e.failStartup(fmt.Errorf("gbl: handler for %s %s binds parameters by name, but the parameter names are unavailable at runtime (source not readable, e.g. built with -trimpath or shipped as a bare binary). Rebuild without -trimpath, or bind through a struct / *struct parameter whose field names come from reflection",
					methodName, meta.path))
			}
		}

		// Freeze the request chain: engine middleware run outermost, then the
		// group's, and the terminal endpoint invokes the business handler. This
		// slice is shared by every request routed here.
		chain := make([]HandlerFunc, 0, len(e.middlewares)+len(meta.mw)+1)
		chain = append(chain, e.middlewares...)
		chain = append(chain, meta.mw...)
		chain = append(chain, routeEndpoint)
		handler.handlers = chain

		// Insert routing tree
		e.insertNode(segmentMetas, path, method, handler)
	}
	e.astFileMap = nil
	e.routeMetaSlice = nil
	e.generated = true
}

// resolveParamNames Recovers a handler's parameter names from its own source file.
//
// reflect reports a function's parameter types but never its parameter names, and
// GBL binds request values by name, so the source recorded in the binary is parsed
// once per file at startup. The handler is located by SOURCE POSITION — the line
// the runtime reports for its entry point — rather than by symbol name: a runtime
// symbol is a full module-qualified path ("github.com/you/pkg.Handler") while the
// AST records only the package clause ("pkg"), so comparing them silently fails
// for every package except main and never matches a func literal. Position
// matching fixes both and additionally resolves closures passed as handlers.
//
// Names cannot be recovered when the source no longer lives where it was
// compiled (a multi-stage image, -trimpath or a bare binary) so the file cannot
// be parsed, or when the entry line belongs to generated code with no enclosing
// declaration (such as the wrapper thunk synthesized for a bound method value).
//
// The second return reports whether real names were recovered. The caller acts
// on it: a handler whose binding needs names aborts startup (failStartup), while
// a self-describing binding (a lone struct, whose fields come from reflection)
// falls back to positional placeholders and continues.
func (e *engine) resolveParamNames(v reflect.Value) ([]string, bool) {
	pc := v.Pointer()
	funPc := runtime.FuncForPC(pc)
	if funPc == nil {
		return nil, false
	}

	symbol := funPc.Name()
	file, line := funPc.FileLine(pc)

	af := e.astFileMap[file]
	if af == nil {
		parsed, err := parser.ParseFile(e.fest, file, nil, parser.ParseComments)
		if err != nil {
			// A failed file is deliberately not cached: every handler in it should
			// still report its own warning, and a failed open costs microseconds.
			e.warnUnboundParams(symbol, "source unavailable: "+err.Error())
			return nil, false
		}
		e.astFileMap[file] = parsed
		af = parsed
	}

	funcType := findFuncTypeAt(e.fest, af, line)
	if funcType == nil || funcType.Params == nil {
		e.warnUnboundParams(symbol, "no function enclosing line "+strconv.Itoa(line)+" of "+file)
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

// findFuncTypeAt returns the signature of the innermost function declaration or
// literal whose source span encloses the given line, or nil when none does.
// Preferring the smallest enclosing span makes a closure passed as a handler
// resolve to its own func literal rather than to the function that created it.
func findFuncTypeAt(fset *token.FileSet, file *ast.File, line int) *ast.FuncType {
	var (
		best     *ast.FuncType
		bestSpan = -1
	)
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

		start := fset.Position(n.Pos()).Line
		end := fset.Position(n.End()).Line
		if line < start || line > end {
			return true
		}
		if span := end - start; bestSpan < 0 || span < bestSpan {
			best, bestSpan = funcType, span
		}
		return true
	})
	return best
}

// normalizeParamNames Pads names to exactly count entries, one per parameter.
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
		if normalized[i] == null {
			normalized[i] = unnamedParamName(i)
		}
	}

	return normalized
}

// unnamedParamName Placeholder for a parameter the source did not name.
func unnamedParamName(index int) string {
	return "arg" + strconv.Itoa(index)
}

// warnUnboundParams reports a handler whose parameter names could not be
// resolved. It routes through the engine Logger at warn level, so the notice
// honors SetLogger and lands wherever the application's warnings are sent,
// a dedicated warn file included.
func (e *engine) warnUnboundParams(symbol, reason string) {
	e.logger.Warn("unbound handler parameters", "handler", symbol, "reason", reason)
}
