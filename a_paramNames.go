// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"flag"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
)

// sourceParamSpan records one function declaration or literal and its parameter names.
type sourceParamSpan struct {
	start int
	end   int
	names []string
}

// sourceParamFile is the cached parse result for one source file.
type sourceParamFile struct {
	spans  []sourceParamSpan
	parsed bool
}

// sourceParamFileCache avoids parsing the same source file for multiple handlers.
var sourceParamFileCache sync.Map

// goTestBinaryOnce caches test-binary detection for the process lifetime.
var goTestBinaryOnce sync.Once

// goTestBinary records whether source-name recovery is allowed.
var goTestBinary bool

// runningUnderGoTest reports whether the current process is a Go test binary.
func runningUnderGoTest() bool {
	goTestBinaryOnce.Do(func() {
		goTestBinary = isGoTestBinary(flag.Lookup("test.v") != nil, os.Args)
	})
	return goTestBinary
}

// isGoTestBinary recognizes standard and custom-named Go test binaries.
func isGoTestBinary(testFlags bool, args []string) bool {
	if testFlags {
		return true
	}
	if len(args) == 0 {
		return false
	}
	name := args[0]
	if strings.HasSuffix(name, ".test") || strings.HasSuffix(name, ".test.exe") {
		return true
	}
	for _, arg := range args[1:] {
		if strings.HasPrefix(arg, "-test.") {
			return true
		}
	}
	return false
}

// resolveParamNames uses DWARF in production and permits source recovery only in tests.
func (e *engine) resolveParamNames(v reflect.Value) ([]string, bool, string, string) {
	if e.paramNameResolver != nil {
		names, resolved, ambiguous := e.paramNameResolver(v)
		return names, resolved, ambiguous, "DWARF"
	}
	names, resolved, ambiguous := e.lookupParamNamesDWARF(v)
	if resolved || !runningUnderGoTest() {
		return names, resolved, ambiguous, "DWARF"
	}
	names, resolved, ambiguous = resolveSourceParamNames(v)
	return names, resolved, ambiguous, "source"
}

// resolveSourceParamNames recovers names from the source file recorded in the binary.
func resolveSourceParamNames(v reflect.Value) ([]string, bool, string) {
	if !v.IsValid() || v.Kind() != reflect.Func || v.IsNil() {
		return nil, false, empty
	}
	pc := v.Pointer()
	fun := runtime.FuncForPC(pc)
	file, line := fun.FileLine(pc)
	spans, parsed := sourceSpansForFile(file)
	if !parsed {
		return nil, false, empty
	}

	span, ambiguous := selectSourceParamSpan(spans, line, file)
	if ambiguous != empty {
		return nil, false, ambiguous
	}
	if span == nil {
		return nil, false, empty
	}
	return span.names, true, empty
}

// selectSourceParamSpan finds the innermost function span for one source line.
func selectSourceParamSpan(spans []sourceParamSpan, line int, file string) (*sourceParamSpan, string) {
	best := -1
	bestLength := 0
	ambiguous := false
	starts := 0
	for i := range spans {
		if spans[i].start == line {
			starts++
		}
		if line < spans[i].start || line > spans[i].end {
			continue
		}
		length := spans[i].end - spans[i].start
		if best < 0 || length < bestLength {
			best = i
			bestLength = length
			ambiguous = false
		} else if length == bestLength {
			ambiguous = true
		}
	}
	if starts > 1 || ambiguous {
		return nil, file + colon + strconv.Itoa(line)
	}
	if best < 0 {
		return nil, empty
	}
	return &spans[best], empty
}

// sourceWorkingDir isolates the test-only source fallback's working-directory lookup.
var sourceWorkingDir = os.Getwd

// localSourcePath maps a module-relative file from a -trimpath build back to the local module tree.
func localSourcePath(file string) string {
	mainModule := modulePath
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Path != empty {
		mainModule = info.Main.Path
	}
	prefix := mainModule + slash
	if !strings.HasPrefix(file, prefix) {
		return file
	}
	relative := filepath.FromSlash(strings.TrimPrefix(file, prefix))
	dir, err := sourceWorkingDir()
	if err != nil {
		return file
	}
	for {
		candidate := filepath.Join(dir, relative)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return file
		}
		dir = parent
	}
}

// sourceSpansForFile parses and caches one source file, resolving module-relative paths from -trimpath builds locally when possible.
func sourceSpansForFile(file string) ([]sourceParamSpan, bool) {
	if cached, ok := sourceParamFileCache.Load(file); ok {
		result := cached.(sourceParamFile)
		return result.spans, result.parsed
	}
	fSet := token.NewFileSet()
	parsed, err := parser.ParseFile(fSet, localSourcePath(file), nil, 0)
	if err != nil {
		result := sourceParamFile{}
		actual, _ := sourceParamFileCache.LoadOrStore(file, result)
		return actual.(sourceParamFile).spans, false
	}

	var spans []sourceParamSpan
	ast.Inspect(parsed, func(node ast.Node) bool {
		var fnType *ast.FuncType
		switch fn := node.(type) {
		case *ast.FuncDecl:
			fnType = fn.Type
		case *ast.FuncLit:
			fnType = fn.Type
		default:
			return true
		}
		spans = append(spans, sourceParamSpan{
			start: fSet.Position(node.Pos()).Line,
			end:   fSet.Position(node.End()).Line,
			names: sourceParamNames(fnType),
		})
		return true
	})
	result := sourceParamFile{spans: spans, parsed: true}
	actual, _ := sourceParamFileCache.LoadOrStore(file, result)
	cached := actual.(sourceParamFile)
	return cached.spans, cached.parsed
}

// sourceParamNames preserves one slot per declared parameter.
func sourceParamNames(fnType *ast.FuncType) []string {
	if fnType.Params == nil {
		return nil
	}
	var names []string
	for _, field := range fnType.Params.List {
		if len(field.Names) == 0 {
			names = append(names, empty)
			continue
		}
		for _, name := range field.Names {
			names = append(names, name.Name)
		}
	}
	return names
}
