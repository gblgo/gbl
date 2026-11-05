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
	"strings"
)

var (
	astFileMap = make(map[string]*ast.File)
	fest       = token.NewFileSet()
)

type Method uint8

func parseMethod(method string) Method {
	switch strings.ToUpper(method) {
	case "GET":
		return methodGet
	case "POST":
		return methodPost
	case "PUT":
		return methodPut
	case "DELETE":
		return methodDelete
	default:
		panic("unsupported http method")
	}
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
		method := parseMethod(meta.method)

		v := reflect.ValueOf(meta.fn)

		if !v.IsValid() || v.Kind() != reflect.Func || v.IsNil() {
			panic(fmt.Errorf("invalid route handler: %s %s, the method is illegal", meta.method, meta.path))
		}

		t := v.Type()
		if t.NumOut() > 1 {
			panic(fmt.Errorf("route handler must have at most one return value: %s", meta.path))
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
			pc := handler.v.Pointer()
			funPc := runtime.FuncForPC(pc)
			file, _ := funPc.FileLine(pc)
			var af *ast.File
			if f, ok := astFileMap[file]; ok {
				af = f
			} else {
				af, _ = parser.ParseFile(fest, file, nil, parser.ParseComments)
				astFileMap[file] = af
			}
			var funcDecl *ast.FuncDecl
			ast.Inspect(af, func(n ast.Node) bool {
				if fd, ok := n.(*ast.FuncDecl); ok && funPc.Name() == af.Name.Name+point+fd.Name.Name {
					funcDecl = fd
					return false
				}
				return true
			})
			var paramNames []string
			for _, param := range funcDecl.Type.Params.List {
				for _, name := range param.Names {
					paramNames = append(paramNames, name.Name)
				}
			}
			handler.paramNames = paramNames
		}
		// Insert routing tree
		insertNode(segmentMetas, path, method, handler)
	}
	println(roots)
	astFileMap = nil
	routeMetaSlice = nil
}
