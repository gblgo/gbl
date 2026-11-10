// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"runtime"
	"strconv"
	"strings"
)

var (
	astFileMap = make(map[string]*ast.File)
	fest       = token.NewFileSet()
)

type Method uint8

// methodNames holds the wire spelling of every supported method, indexed by Method.
var methodNames = [methodCount]string{
	methodGet:     "GET",
	methodHead:    "HEAD",
	methodPost:    "POST",
	methodPut:     "PUT",
	methodPatch:   "PATCH",
	methodDelete:  "DELETE",
	methodOptions: "OPTIONS",
}

// allMethods lists every supported method, used by Any.
var allMethods = []Method{
	methodGet, methodHead, methodPost, methodPut, methodPatch, methodDelete, methodOptions,
}

// methodOf Resolves an HTTP method name, reporting false when GBL does not
// implement that verb at all.
//
// The upper-casing keeps the lenient behavior GBL has always had, so a
// lowercase "get" is still treated as "GET".
func methodOf(name string) (Method, bool) {
	upper := strings.ToUpper(name)
	for m, methodName := range methodNames {
		if methodName == upper {
			return Method(m), true
		}
	}
	return 0, false
}

// isBodyMethod Reports whether GBL parses a request body for this method.
//
// GET, HEAD, DELETE and OPTIONS never touch the body, whatever Content-Type the
// client sent. PATCH joins POST and PUT here because partial updates are its
// whole purpose.
func (m Method) isBodyMethod() bool {
	return m == methodPost || m == methodPut || m == methodPatch
}

type routeHandler struct {
	hct        string
	v          reflect.Value
	t          reflect.Type
	paramNames []string
}

func dealPath(path string) ([]segmentMeta, string) {

	path = strings.TrimSpace(path)

	if path == null {
		panic("when registering a route, the path cannot be empty")
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
				panic("when registering a route, the path cannot be '/:' or '/:***:***:' or '/::', etc")
			}
			meta.isVar = true
			meta.paramName = part[1:]
		}

		if strings.Contains(part, leftCurlyBrace) && (len(part) == 1 || strings.LastIndex(part, leftCurlyBrace) != 0) {
			panic("when registering a route, the path cannot be '/{' or '/{***{***{' or '/{{', etc")
		}

		if strings.Contains(part, rightCurlyBrace) && (len(part) == 1 || strings.Index(part, rightCurlyBrace)+1 != len(part)) {
			panic("when registering a route, the path cannot be '/}' or '/}***}***}' or '/}}', etc")
		}

		if strings.TrimSuffix(strings.TrimPrefix(part, leftCurlyBrace), rightCurlyBrace) == null {
			panic("when registering a route, the path cannot be '/{}'")
		}

		if strings.Contains(part, leftCurlyBrace) {
			meta.isVar = true
			meta.paramName = part[1 : len(part)-1]
		}

		segmentMetas = append(segmentMetas, meta)
		segments = append(segments, part)
	}

	if len(segmentMetas) == 0 {
		panic("when registering a route, the path cannot be empty or '/'")
	}

	return segmentMetas, slash + strings.Join(segments, slash)
}

func generateRoute() {
	for _, meta := range routeMetaSlice {
		segmentMetas, path := dealPath(meta.path)
		method := meta.method
		methodName := methodNames[method]

		v := reflect.ValueOf(meta.fn)

		if !v.IsValid() || v.Kind() != reflect.Func || v.IsNil() {
			panic(fmt.Errorf("invalid route handler: %s %s, the method is illegal", methodName, meta.path))
		}

		t := v.Type()
		if t.NumOut() > 1 {
			panic(fmt.Errorf("route handler must have at most one return value: %s", meta.path))
		}

		// A handler registered as variadic must be invoked with CallSlice. GBL keeps
		// the public handler contract simple and rejects variadic handlers here.
		if t.IsVariadic() {
			panic("variadic handlers are not supported")
		}

		handler := &routeHandler{
			hct: textPlain,
			v:   v,
			t:   t,
		}

		if t.NumOut() > 0 {
			switch t.Out(0).Kind() {
			case reflect.Bool,
				reflect.String,
				reflect.Float32, reflect.Float64,
				reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
				reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
				handler.hct = textPlain
			case reflect.Struct, reflect.Pointer, reflect.Interface:
				handler.hct = applicationJson
			default:
				handler.hct = textPlain
			}
		}

		if t.NumIn() > 0 {
			handler.paramNames = normalizeParamNames(resolveParamNames(handler.v), t.NumIn())
		}
		// Insert routing tree
		insertNode(segmentMetas, path, method, handler)
	}
	astFileMap = nil
	routeMetaSlice = nil
}

// resolveParamNames Recovers a handler's parameter names from its own source file.
//
// reflect reports a function's parameter types but never its parameter names, and
// GBL binds request values by name, so the source recorded in the binary is parsed
// once per file at startup. Two ordinary deployments break that assumption, and
// neither may abort the process: both fall back to positional placeholders.
//
//   - the source no longer lives where it was compiled, which is what a multi-stage
//     image, -trimpath or shipping a bare binary produces, so ParseFile fails
//   - the handler is not a plain package-level function: a func literal, a bound
//     method value or a generic instantiation all carry a symbol matching no FuncDecl
func resolveParamNames(v reflect.Value) []string {
	pc := v.Pointer()
	funPc := runtime.FuncForPC(pc)
	if funPc == nil {
		return nil
	}

	symbol := funPc.Name()
	file, _ := funPc.FileLine(pc)

	af := astFileMap[file]
	if af == nil {
		parsed, err := parser.ParseFile(fest, file, nil, parser.ParseComments)
		if err != nil {
			// A failed file is deliberately not cached: every handler in it should
			// still report its own warning, and a failed open costs microseconds.
			warnUnboundParams(symbol, "source unavailable: "+err.Error())
			return nil
		}
		astFileMap[file] = parsed
		af = parsed
	}

	var funcDecl *ast.FuncDecl
	ast.Inspect(af, func(n ast.Node) bool {
		fd, ok := n.(*ast.FuncDecl)
		if !ok {
			return true
		}
		if symbol == af.Name.Name+point+fd.Name.Name {
			funcDecl = fd
			return false
		}
		return true
	})

	if funcDecl == nil {
		warnUnboundParams(symbol, "no matching declaration in "+file)
		return nil
	}

	var paramNames []string
	for _, param := range funcDecl.Type.Params.List {
		for _, name := range param.Names {
			paramNames = append(paramNames, name.Name)
		}
	}

	return paramNames
}

// normalizeParamNames Pads names to exactly count entries, one per parameter.
//
// Unnamed parameters contribute nothing to the recovered list, so it can arrive
// short or empty. Every gap gets a placeholder that cannot collide with a real
// path, query or JSON key, which keeps binding deterministic rather than letting
// a later index lookup panic and surface to the client.
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

// warnUnboundParams Reports a handler whose parameter names could not be resolved.
//
// GBL has no logger yet, and a route that silently lost its names is far harder
// to diagnose than one line printed while the process is still starting up.
func warnUnboundParams(symbol, reason string) {
	_, _ = fmt.Fprintf(os.Stderr, "gbl: warning: handler %s parameters cannot be bound by name (%s)\n", symbol, reason)
}
