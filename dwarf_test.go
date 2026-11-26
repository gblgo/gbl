// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"bytes"
	"context"
	"debug/dwarf"
	"debug/elf"
	"debug/macho"
	"debug/pe"
	"encoding/binary"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"
)

const dwarfFixtureSource = `package main

//go:noinline
func named(first string, _ int, third bool) (string, error) { return "", nil }

//go:noinline
func unnamed(string, int) string { return "" }

//go:noinline
func blank(_ string, count int) int { return count }

//go:noinline
func results(input string) (total int, err error) { total = len(input); return }

//go:noinline
func closure() func(string, int) string {
	return func(id string, count int) string { return id }
}

//go:noinline
func duplicate() (func(string) string, func(int) string) {
	return func(first string) string { return first }, func(second int) string { return "" }
}

func main() {
	_, _ = named("", 0, false)
	_ = unnamed("", 0)
	_ = blank("", 0)
	_, _ = results("")
	_ = closure()("", 0)
	first, second := duplicate()
	_ = first("")
	_ = second(0)
}
`

func TestBuildDWARFIndexCompatibilityEntryPoints(t *testing.T) {
	// These entry points necessarily inspect the test executable. Their debug
	// sections may be stripped, so only consistency is asserted here; the real
	// fixture tests below validate parsing independently of the test binary.
	full := buildDWARFIndex()
	targeted := buildDWARFIndexFor(map[string]struct{}{"not-a-position": {}})
	if full == nil {
		if targeted != nil {
			t.Fatalf("targeted index = %v, want nil when the test binary has no DWARF", targeted)
		}
		return
	}
	if targeted == nil || len(targeted) != 0 {
		t.Fatalf("targeted index = %v, want a non-nil empty map", targeted)
	}
}

func TestBuildDWARFIndexFromDataFixture(t *testing.T) {
	data, positions := dwarfTestBuildFixture(t)

	t.Run("full index", func(t *testing.T) {
		index := buildDWARFIndexFromData(data, nil)
		dwarfTestCheckEntry(t, index, positions.functions["named"], []string{"first", "", "third"}, false)
		dwarfTestCheckEntry(t, index, positions.functions["unnamed"], []string{"", ""}, false)
		dwarfTestCheckEntry(t, index, positions.functions["blank"], []string{"", "count"}, false)
		dwarfTestCheckEntry(t, index, positions.functions["results"], []string{"input"}, false)
		dwarfTestCheckEntry(t, index, positions.literals["closure"][0], []string{"id", "count"}, false)
		dwarfTestCheckEntry(t, index, positions.literals["duplicate"][0], nil, true)
	})

	t.Run("targeted index", func(t *testing.T) {
		targets := map[string]struct{}{
			positions.functions["named"]:       {},
			positions.functions["blank"]:       {},
			positions.literals["closure"][0]:   {},
			positions.literals["duplicate"][0]: {},
		}
		index := buildDWARFIndexFromData(data, targets)
		if len(index) != len(targets) {
			t.Fatalf("targeted index has %d entries, want %d: %v", len(index), len(targets), index)
		}
		dwarfTestCheckEntry(t, index, positions.functions["named"], []string{"first", "", "third"}, false)
		dwarfTestCheckEntry(t, index, positions.functions["blank"], []string{"", "count"}, false)
		dwarfTestCheckEntry(t, index, positions.literals["closure"][0], []string{"id", "count"}, false)
		dwarfTestCheckEntry(t, index, positions.literals["duplicate"][0], nil, true)
		if _, ok := index[positions.functions["unnamed"]]; ok {
			t.Fatal("targeted index retained a non-target subprogram")
		}
	})

	t.Run("subprogram skip", func(t *testing.T) {
		key := positions.functions["named"]
		index := buildDWARFIndexFromData(data, map[string]struct{}{key: {}})
		if len(index) != 1 {
			t.Fatalf("single-target index has %d entries, want 1: %v", len(index), index)
		}
		dwarfTestCheckEntry(t, index, key, []string{"first", "", "third"}, false)
	})

	t.Run("compile unit skip", func(t *testing.T) {
		key := filepath.Join(t.TempDir(), "not-in-the-fixture.go") + ":1"
		index := buildDWARFIndexFromData(data, map[string]struct{}{key: {}})
		if index == nil || len(index) != 0 {
			t.Fatalf("missing-file target index = %v, want a non-nil empty map", index)
		}
	})

	t.Run("target without file separator", func(t *testing.T) {
		index := buildDWARFIndexFromData(data, map[string]struct{}{"not-a-position": {}})
		if index == nil || len(index) != 0 {
			t.Fatalf("malformed-target index = %v, want a non-nil empty map", index)
		}
	})
}

func TestBuildDWARFIndexFromDataDWARFFileIndexes(t *testing.T) {
	for _, version := range []int{4, 5} {
		t.Run("DWARF"+strconv.Itoa(version), func(t *testing.T) {
			file := dwarfTestAbsoluteName(t, "file-index.go")
			validIndex := int64(1)
			invalidIndex := int64(0) // DWARF 4 reserves line-table file index 0.
			if version == 5 {
				validIndex = 0 // DWARF 5 allows index 0 to name a real file.
				invalidIndex = 1
			}
			data := dwarfTestData(t, dwarfTestUnit{
				version: version,
				files:   []string{file},
				dies: []*dwarfTestDIE{
					dwarfTestSubprogram(validIndex, 42, dwarfTestParameter(dwarfTestName("value"))),
					dwarfTestSubprogram(invalidIndex, 43, dwarfTestParameter(dwarfTestName("ignored"))),
				},
			})
			key := file + ":42"
			index := buildDWARFIndexFromData(data, nil)
			if len(index) != 1 {
				t.Fatalf("full index has %d entries, want only the valid file slot: %v", len(index), index)
			}
			dwarfTestCheckEntry(t, index, key, []string{"value"}, false)

			index = buildDWARFIndexFromData(data, map[string]struct{}{key: {}})
			if len(index) != 1 {
				t.Fatalf("targeted index has %d entries, want 1: %v", len(index), index)
			}
			dwarfTestCheckEntry(t, index, key, []string{"value"}, false)
		})
	}
}

func TestBuildDWARFIndexFromDataScopesAndDuplicates(t *testing.T) {
	file := dwarfTestAbsoluteName(t, "scopes.go")
	lexicalBlock := &dwarfTestDIE{
		tag: dwarf.TagLexDwarfBlock,
		dies: []*dwarfTestDIE{
			dwarfTestParameter(dwarfTestName("hiddenLexicalParameter")),
		},
	}
	outer := dwarfTestSubprogram(1, 10,
		dwarfTestParameter(dwarfTestName("direct")),
		lexicalBlock,
		dwarfTestParameter(),
		dwarfTestParameter(dwarfTestName("_")),
		dwarfTestParameter(dwarfTestName("~p4")),
		dwarfTestParameter(dwarfTestName("~r9")),
		dwarfTestParameter(dwarfTestName("result"), dwarfTestAttr{
			attr: dwarf.AttrVarParam, form: dwarfTestFormFlagPresent,
		}),
		dwarfTestParameter(dwarfTestName("falseResult"), dwarfTestAttr{
			attr: dwarf.AttrVarParam, form: dwarfTestFormFlag, val: false,
		}),
		dwarfTestParameter(dwarfTestName("wrongVarParamType"), dwarfTestAttr{
			attr: dwarf.AttrVarParam, form: dwarfTestFormData1, val: int64(1),
		}),
		dwarfTestParameter(dwarfTestAttr{
			attr: dwarf.AttrName, form: dwarfTestFormData1, val: int64(7),
		}),
	)
	data := dwarfTestData(t, dwarfTestUnit{
		version: 4,
		files:   []string{file},
		dies: []*dwarfTestDIE{
			outer,
			dwarfTestSubprogram(1, 10, dwarfTestParameter(dwarfTestName("differentSignature"))),
			dwarfTestSubprogram(1, 10, dwarfTestParameter(dwarfTestName("thirdSignature"))),
			dwarfTestSubprogram(1, 11, dwarfTestParameter(dwarfTestName("other"))),
		},
	})

	key := file + ":10"
	index := buildDWARFIndexFromData(data, nil)
	dwarfTestCheckEntry(t, index, key, nil, true)
	dwarfTestCheckEntry(t, index, file+":11", []string{"other"}, false)

	// Use a separate, non-duplicate entry to inspect every direct input slot.
	slotData := dwarfTestData(t, dwarfTestUnit{
		version: 4,
		files:   []string{file},
		dies:    []*dwarfTestDIE{outer},
	})
	index = buildDWARFIndexFromData(slotData, nil)
	dwarfTestCheckEntry(t, index, key, []string{
		"direct", "", "", "", "falseResult", "wrongVarParamType", "",
	}, false)

	index = buildDWARFIndexFromData(data, map[string]struct{}{key: {}})
	if len(index) != 1 {
		t.Fatalf("duplicate-target index has %d entries, want 1: %v", len(index), index)
	}
	dwarfTestCheckEntry(t, index, key, nil, true)

	otherKey := file + ":11"
	index = buildDWARFIndexFromData(data, map[string]struct{}{otherKey: {}})
	if len(index) != 1 {
		t.Fatalf("single-target index has %d entries, want 1: %v", len(index), index)
	}
	dwarfTestCheckEntry(t, index, otherKey, []string{"other"}, false)
}

func TestBuildDWARFIndexFromDataMalformed(t *testing.T) {
	file := dwarfTestAbsoluteName(t, "malformed.go")
	parameter := dwarfTestParameter(dwarfTestName("mustNotBeIndexed"))
	data := dwarfTestData(t,
		dwarfTestUnit{
			version: 4,
			files:   []string{file},
			dies: []*dwarfTestDIE{
				&dwarfTestDIE{
					tag: dwarf.TagSubprogram,
					attrs: []dwarfTestAttr{
						{attr: dwarf.AttrDeclLine, form: dwarfTestFormData1, val: int64(20)},
					},
					dies: []*dwarfTestDIE{parameter},
				},
				dwarfTestSubprogramWithAttrs([]dwarfTestAttr{
					{attr: dwarf.AttrDeclFile, form: dwarfTestFormString, val: "1"},
					{attr: dwarf.AttrDeclLine, form: dwarfTestFormData1, val: int64(20)},
				}, parameter),
				dwarfTestSubprogramWithAttrs([]dwarfTestAttr{
					{attr: dwarf.AttrDeclFile, form: dwarfTestFormSdata, val: int64(-1)},
					{attr: dwarf.AttrDeclLine, form: dwarfTestFormData1, val: int64(20)},
				}, parameter),
				dwarfTestSubprogramWithAttrs([]dwarfTestAttr{
					{attr: dwarf.AttrDeclFile, form: dwarfTestFormData1, val: int64(99)},
					{attr: dwarf.AttrDeclLine, form: dwarfTestFormData1, val: int64(20)},
				}, parameter),
				dwarfTestSubprogramWithAttrs([]dwarfTestAttr{
					{attr: dwarf.AttrDeclFile, form: dwarfTestFormData1, val: int64(1)},
					{attr: dwarf.AttrDeclLine, form: dwarfTestFormString, val: "20"},
				}, parameter),
				dwarfTestSubprogram(1, 0, parameter),
			},
		},
		dwarfTestUnit{
			version: 4,
			noLine:  true,
			dies: []*dwarfTestDIE{
				dwarfTestSubprogram(0, 1, parameter),
			},
		},
		dwarfTestUnit{
			version: 4,
			badLine: true,
			dies: []*dwarfTestDIE{
				dwarfTestSubprogram(0, 1, parameter),
			},
		},
	)

	for _, targets := range []map[string]struct{}{
		nil,
		{file + ":20": {}},
	} {
		index := buildDWARFIndexFromData(data, targets)
		if index == nil || len(index) != 0 {
			t.Fatalf("malformed DWARF index = %v, want a non-nil empty map", index)
		}
	}

	t.Run("nil data", func(t *testing.T) {
		if index := buildDWARFIndexFromData(nil, nil); index != nil {
			t.Fatalf("nil DWARF index = %v, want nil", index)
		}
	})

	t.Run("reader decode error", func(t *testing.T) {
		// The unit header is valid enough for dwarf.New, but its first DIE uses
		// an undefined abbreviation code and makes Reader.Next fail.
		info := []byte{8, 0, 0, 0, 4, 0, 0, 0, 0, 0, 8, 0x7f}
		abbrev := []byte{1, byte(dwarf.TagCompileUnit), 0, 0, 0, 0}
		malformed, err := dwarf.New(abbrev, nil, nil, info, nil, nil, nil, nil)
		if err != nil {
			t.Fatalf("construct malformed DWARF data: %v", err)
		}
		index := buildDWARFIndexFromData(malformed, nil)
		if index == nil || len(index) != 0 {
			t.Fatalf("decode-error index = %v, want a non-nil empty map", index)
		}
	})
}

func TestOpenSelfDWARFContainerErrors(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.exe")
	if data := openSelfDWARF(missing); data != nil {
		t.Fatal("missing executable unexpectedly produced DWARF data")
	}

	malformed := filepath.Join(dir, "malformed.exe")
	if err := os.WriteFile(malformed, []byte("not an executable"), 0o600); err != nil {
		t.Fatalf("write malformed executable: %v", err)
	}
	if data := openSelfDWARF(malformed); data != nil {
		t.Fatal("malformed executable unexpectedly produced DWARF data")
	}

	containers := []struct {
		name string
		data []byte
	}{
		{name: "no-dwarf.pe", data: dwarfTestPEWithoutDWARF(t)},
		{name: "no-dwarf.elf", data: dwarfTestELFWithoutDWARF(t)},
		{name: "no-dwarf.macho", data: dwarfTestMachOWithoutDWARF(t)},
	}
	for _, container := range containers {
		t.Run(container.name, func(t *testing.T) {
			path := filepath.Join(dir, container.name)
			if err := os.WriteFile(path, container.data, 0o600); err != nil {
				t.Fatalf("write container fixture: %v", err)
			}
			if data := openSelfDWARF(path); data != nil {
				t.Fatal("valid container without debug_info unexpectedly produced DWARF data")
			}
		})
	}
}

type dwarfFixturePositions struct {
	functions map[string]string
	literals  map[string][]string
}

// dwarfTestFixture builds the executable fixture once per test process. The
// cached DWARF data and source positions are read-only across all fixture tests.
var dwarfTestFixture = struct {
	once      sync.Once
	data      *dwarf.Data
	positions dwarfFixturePositions
	err       error
}{}

func dwarfTestBuildFixture(t *testing.T) (*dwarf.Data, dwarfFixturePositions) {
	t.Helper()
	dwarfTestFixture.once.Do(func() {
		dwarfTestFixture.data, dwarfTestFixture.positions, dwarfTestFixture.err =
			dwarfTestBuildFixtureData(t.Context())
	})
	if dwarfTestFixture.err != nil {
		t.Fatalf("build DWARF fixture: %v", dwarfTestFixture.err)
	}
	return dwarfTestFixture.data, dwarfTestFixture.positions
}

func dwarfTestBuildFixtureData(ctx context.Context) (*dwarf.Data, dwarfFixturePositions, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	dir, err := os.MkdirTemp("", "gbl-dwarf-fixture-")
	if err != nil {
		return nil, dwarfFixturePositions{}, fmt.Errorf("create temporary fixture directory: %w", err)
	}
	defer os.RemoveAll(dir)

	source := filepath.Join(dir, "main.go")
	if err := os.WriteFile(source, []byte(dwarfFixtureSource), 0o600); err != nil {
		return nil, dwarfFixturePositions{}, fmt.Errorf("write Go fixture: %w", err)
	}
	executable := filepath.Join(dir, "fixture.exe")
	cmd := exec.CommandContext(ctx, "go", "build",
		"-gcflags=all=-l",
		"-ldflags=-s=false -w=false -compressdwarf=false",
		"-o", executable, source,
	)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOWORK=off", "GOFLAGS=")
	cmd.WaitDelay = 5 * time.Second
	output, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return nil, dwarfFixturePositions{}, fmt.Errorf("Go fixture build timed out: %v\n%s", ctx.Err(), output)
		}
		return nil, dwarfFixturePositions{}, fmt.Errorf("build Go fixture: %w\n%s", err, output)
	}
	data := openSelfDWARF(executable)
	if data == nil {
		return nil, dwarfFixturePositions{}, errors.New("Go fixture has no readable DWARF")
	}

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, source, dwarfFixtureSource, 0)
	if err != nil {
		return nil, dwarfFixturePositions{}, fmt.Errorf("parse Go fixture source: %w", err)
	}
	positions := dwarfFixturePositions{
		functions: make(map[string]string),
		literals:  make(map[string][]string),
	}
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Recv != nil {
			continue
		}
		name := function.Name.Name
		positions.functions[name] = dwarfTestPositionKey(fset, function.Pos())
		ast.Inspect(function.Body, func(node ast.Node) bool {
			if literal, ok := node.(*ast.FuncLit); ok {
				positions.literals[name] = append(positions.literals[name], dwarfTestPositionKey(fset, literal.Pos()))
			}
			return true
		})
	}
	return data, positions, nil
}

func dwarfTestPositionKey(fset *token.FileSet, position token.Pos) string {
	pos := fset.Position(position)
	return filepath.ToSlash(pos.Filename) + ":" + strconv.Itoa(pos.Line)
}

func dwarfTestCheckEntry(t *testing.T, index map[string]dwarfParamNames, key string, want []string, ambiguous bool) {
	t.Helper()
	entry, ok := index[key]
	if !ok {
		t.Fatalf("DWARF index has no entry at %s", key)
	}
	if entry.ambiguous != ambiguous {
		t.Fatalf("entry at %s ambiguous = %v, want %v", key, entry.ambiguous, ambiguous)
	}
	if !reflect.DeepEqual(entry.names, want) {
		t.Fatalf("entry at %s names = %v, want %v", key, entry.names, want)
	}
}

// The small DWARF builders below cover malformed and legacy inputs that the Go
// compiler no longer emits. Real compiler output remains covered by the fixture.
const (
	dwarfTestFormString      byte = 0x08
	dwarfTestFormData1       byte = 0x0b
	dwarfTestFormFlag        byte = 0x0c
	dwarfTestFormSdata       byte = 0x0d
	dwarfTestFormSecOffset   byte = 0x17
	dwarfTestFormFlagPresent byte = 0x19
)

type dwarfTestAttr struct {
	attr dwarf.Attr
	form byte
	val  any
}

type dwarfTestDIE struct {
	tag   dwarf.Tag
	attrs []dwarfTestAttr
	dies  []*dwarfTestDIE
}

type dwarfTestUnit struct {
	version int
	files   []string
	dies    []*dwarfTestDIE
	noLine  bool
	badLine bool
}

func dwarfTestName(name string) dwarfTestAttr {
	return dwarfTestAttr{attr: dwarf.AttrName, form: dwarfTestFormString, val: name}
}

func dwarfTestParameter(attrs ...dwarfTestAttr) *dwarfTestDIE {
	return &dwarfTestDIE{tag: dwarf.TagFormalParameter, attrs: attrs}
}

func dwarfTestSubprogram(file, line int64, dies ...*dwarfTestDIE) *dwarfTestDIE {
	return dwarfTestSubprogramWithAttrs([]dwarfTestAttr{
		{attr: dwarf.AttrDeclFile, form: dwarfTestFormData1, val: file},
		{attr: dwarf.AttrDeclLine, form: dwarfTestFormData1, val: line},
	}, dies...)
}

func dwarfTestSubprogramWithAttrs(attrs []dwarfTestAttr, dies ...*dwarfTestDIE) *dwarfTestDIE {
	return &dwarfTestDIE{tag: dwarf.TagSubprogram, attrs: attrs, dies: dies}
}

func dwarfTestAbsoluteName(t *testing.T, name string) string {
	t.Helper()
	path, err := filepath.Abs(name)
	if err != nil {
		t.Fatalf("resolve fixture file name: %v", err)
	}
	return path
}

func dwarfTestData(t *testing.T, units ...dwarfTestUnit) *dwarf.Data {
	t.Helper()
	if len(units) == 0 {
		t.Fatal("construct DWARF data without units")
	}

	var line bytes.Buffer
	roots := make([]*dwarfTestDIE, len(units))
	lineOffsets := make([]int64, len(units))
	for i := range units {
		lineOffsets[i] = int64(line.Len())
		switch {
		case units[i].noLine:
		case units[i].badLine:
			line.Write([]byte{0, 0, 0, 0, 6, 0}) // Unsupported line-table version 6.
		default:
			line.Write(dwarfTestLineTable(units[i].version, units[i].files))
		}
		roots[i] = &dwarfTestDIE{tag: dwarf.TagCompileUnit, dies: units[i].dies}
		if !units[i].noLine {
			roots[i].attrs = []dwarfTestAttr{{
				attr: dwarf.AttrStmtList, form: dwarfTestFormSecOffset, val: lineOffsets[i],
			}}
		}
	}

	var entries []*dwarfTestDIE
	var collect func(*dwarfTestDIE)
	collect = func(entry *dwarfTestDIE) {
		entries = append(entries, entry)
		for _, child := range entry.dies {
			collect(child)
		}
	}
	for _, root := range roots {
		collect(root)
	}
	codes := make(map[*dwarfTestDIE]uint64, len(entries))

	var abbrev bytes.Buffer
	for i, entry := range entries {
		code := uint64(i + 1)
		codes[entry] = code
		dwarfTestWriteULEB(&abbrev, code)
		dwarfTestWriteULEB(&abbrev, uint64(entry.tag))
		if len(entry.dies) > 0 {
			abbrev.WriteByte(1)
		} else {
			abbrev.WriteByte(0)
		}
		for _, attr := range entry.attrs {
			dwarfTestWriteULEB(&abbrev, uint64(attr.attr))
			dwarfTestWriteULEB(&abbrev, uint64(attr.form))
		}
		abbrev.WriteByte(0)
		abbrev.WriteByte(0)
	}
	abbrev.WriteByte(0)

	var info bytes.Buffer
	for i, root := range roots {
		var body bytes.Buffer
		if err := binary.Write(&body, binary.LittleEndian, uint16(units[i].version)); err != nil {
			t.Fatal(err)
		}
		if units[i].version == 5 {
			body.WriteByte(1) // DW_UT_compile.
			body.WriteByte(8) // Address size.
		}
		if err := binary.Write(&body, binary.LittleEndian, uint32(0)); err != nil {
			t.Fatal(err)
		}
		if units[i].version < 5 {
			body.WriteByte(8) // Address size.
		}
		dwarfTestWriteDIE(t, &body, root, codes)
		if err := binary.Write(&info, binary.LittleEndian, uint32(body.Len())); err != nil {
			t.Fatal(err)
		}
		body.WriteTo(&info)
	}

	data, err := dwarf.New(abbrev.Bytes(), nil, nil, info.Bytes(), line.Bytes(), nil, nil, nil)
	if err != nil {
		t.Fatalf("construct synthetic DWARF data: %v", err)
	}
	return data
}

func dwarfTestLineTable(version int, files []string) []byte {
	var fields bytes.Buffer
	fields.WriteByte(1) // Minimum instruction length.
	if version >= 4 {
		fields.WriteByte(1) // Maximum operations per instruction.
	}
	fields.WriteByte(1)    // default_is_stmt.
	fields.WriteByte(0xfb) // line_base = -5.
	fields.WriteByte(14)   // line_range.
	fields.WriteByte(13)   // opcode_base.
	fields.Write([]byte{0, 1, 1, 1, 1, 0, 0, 0, 1, 0, 0, 1})

	if version < 5 {
		fields.WriteByte(0) // Empty include-directory table.
		for _, file := range files {
			fields.WriteString(file)
			fields.WriteByte(0)
			fields.Write([]byte{0, 0, 0}) // Directory index, mtime, size.
		}
		fields.WriteByte(0) // Empty file-name terminator leaves slot 0 nil.
	} else {
		fields.WriteByte(0)            // Directory format count.
		dwarfTestWriteULEB(&fields, 0) // Directory count.
		fields.WriteByte(1)            // File format count.
		dwarfTestWriteULEB(&fields, 1) // DW_LNCT_path.
		dwarfTestWriteULEB(&fields, uint64(dwarfTestFormString))
		dwarfTestWriteULEB(&fields, uint64(len(files)))
		for _, file := range files {
			fields.WriteString(file)
			fields.WriteByte(0)
		}
	}

	var body bytes.Buffer
	if err := binary.Write(&body, binary.LittleEndian, uint16(version)); err != nil {
		panic(err)
	}
	if version >= 5 {
		body.WriteByte(8) // Address size.
		body.WriteByte(0) // Segment selector size.
	}
	if err := binary.Write(&body, binary.LittleEndian, uint32(fields.Len())); err != nil {
		panic(err)
	}
	fields.WriteTo(&body)
	var unit bytes.Buffer
	if err := binary.Write(&unit, binary.LittleEndian, uint32(body.Len())); err != nil {
		panic(err)
	}
	body.WriteTo(&unit)
	return unit.Bytes()
}

func dwarfTestWriteDIE(t *testing.T, out *bytes.Buffer, entry *dwarfTestDIE, codes map[*dwarfTestDIE]uint64) {
	t.Helper()
	dwarfTestWriteULEB(out, codes[entry])
	for _, attr := range entry.attrs {
		switch attr.form {
		case dwarfTestFormString:
			name, ok := attr.val.(string)
			if !ok {
				t.Fatalf("DW_FORM_string value has type %T", attr.val)
			}
			out.WriteString(name)
			out.WriteByte(0)
		case dwarfTestFormData1:
			value, ok := attr.val.(int64)
			if !ok {
				t.Fatalf("DW_FORM_data1 value has type %T", attr.val)
			}
			out.WriteByte(byte(value))
		case dwarfTestFormFlag:
			value, ok := attr.val.(bool)
			if !ok {
				t.Fatalf("DW_FORM_flag value has type %T", attr.val)
			}
			if value {
				out.WriteByte(1)
			} else {
				out.WriteByte(0)
			}
		case dwarfTestFormSdata:
			value, ok := attr.val.(int64)
			if !ok {
				t.Fatalf("DW_FORM_sdata value has type %T", attr.val)
			}
			dwarfTestWriteSLEB(out, value)
		case dwarfTestFormSecOffset:
			value, ok := attr.val.(int64)
			if !ok {
				t.Fatalf("DW_FORM_sec_offset value has type %T", attr.val)
			}
			if err := binary.Write(out, binary.LittleEndian, uint32(value)); err != nil {
				t.Fatal(err)
			}
		case dwarfTestFormFlagPresent:
		default:
			t.Fatalf("unsupported synthetic DWARF form %#x", attr.form)
		}
	}
	for _, child := range entry.dies {
		dwarfTestWriteDIE(t, out, child, codes)
	}
	if len(entry.dies) > 0 {
		out.WriteByte(0) // Null DIE closes the child list.
	}
}

func dwarfTestWriteULEB(out *bytes.Buffer, value uint64) {
	for {
		part := byte(value & 0x7f)
		value >>= 7
		if value != 0 {
			part |= 0x80
		}
		out.WriteByte(part)
		if value == 0 {
			return
		}
	}
}

func dwarfTestWriteSLEB(out *bytes.Buffer, value int64) {
	for {
		part := byte(value & 0x7f)
		value >>= 7
		done := (value == 0 && part&0x40 == 0) || (value == -1 && part&0x40 != 0)
		if !done {
			part |= 0x80
		}
		out.WriteByte(part)
		if done {
			return
		}
	}
}

func dwarfTestEncode(t *testing.T, value any) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := binary.Write(&out, binary.LittleEndian, value); err != nil {
		t.Fatalf("encode container header: %v", err)
	}
	return out.Bytes()
}

func dwarfTestPEWithoutDWARF(t *testing.T) []byte {
	t.Helper()
	data := make([]byte, 0x80)
	copy(data, "MZ")
	binary.LittleEndian.PutUint32(data[0x3c:], 0x80)
	data = append(data, []byte("PE\x00\x00")...)
	return append(data, dwarfTestEncode(t, pe.FileHeader{Machine: pe.IMAGE_FILE_MACHINE_UNKNOWN})...)
}

func dwarfTestELFWithoutDWARF(t *testing.T) []byte {
	t.Helper()
	header := elf.Header64{
		Type:    uint16(elf.ET_EXEC),
		Machine: uint16(elf.EM_X86_64),
		Version: uint32(elf.EV_CURRENT),
		Ehsize:  64,
	}
	copy(header.Ident[:], elf.ELFMAG)
	header.Ident[elf.EI_CLASS] = byte(elf.ELFCLASS64)
	header.Ident[elf.EI_DATA] = byte(elf.ELFDATA2LSB)
	header.Ident[elf.EI_VERSION] = byte(elf.EV_CURRENT)
	return dwarfTestEncode(t, header)
}

func dwarfTestMachOWithoutDWARF(t *testing.T) []byte {
	t.Helper()
	return dwarfTestEncode(t, macho.FileHeader{
		Magic: macho.Magic64,
		Cpu:   macho.CpuAmd64,
		Type:  macho.TypeExec,
	})
}
