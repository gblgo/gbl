// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"debug/dwarf"
	"debug/elf"
	"debug/macho"
	"debug/pe"
	"os"
	"strconv"
	"strings"
)

// dwarfParamNames records one source position. Duplicate positions are marked
// ambiguous instead of allowing the later function to overwrite the earlier one.
type dwarfParamNames struct {
	// names holds input parameter slots in declaration order.
	names []string
	// ambiguous marks duplicate source positions with different signatures.
	ambiguous bool
}

// buildDWARFIndex recovers parameter names from the running binary's DWARF
// debug information, keyed by the source position the runtime reports for each
// function ("<file>:<line>"). It is the compatibility entry point for callers
// that need a complete index; route generation uses buildDWARFIndexFor below so
// unrelated functions are never retained in memory.
//
// This is the production half of name binding. The Go compiler discards parameter
// names from the runtime type metadata. It still emits them into the binary DWARF
// section by default, and -trimpath does not strip that section. So even a
// sourceless production binary — a multi-stage image, a bare executable — carries
// every function parameter name. The section also records each declaration file
// and line, so a handler is matched to its DWARF entry by the SAME position key
// the disk parser uses. Position keys also resolve closures and func literals,
// and are immune to the merging and renaming of identical literals introduced in
// Go 1.27.
//
// It returns nil when the binary has no readable DWARF. This happens most
// commonly when the binary was built with `-ldflags "-w"` or `"-s"`, which strip
// debug information. The caller treats nil as "no names recoverable" and fails
// startup for a handler that binds by name, pointing the user at struct binding
// instead.
//
// Only input parameters are retained. Go marks result pseudo-parameters with
// AttrVarParam (and usually names them "~r0"); unnamed or blank inputs use
// synthetic "~pN" names or no name at all. Those inputs become empty slots rather
// than being dropped, so later names never shift left. The caller verifies that
// the resulting input count exactly matches the reflected handler signature.
func buildDWARFIndex() map[string]dwarfParamNames {
	return buildDWARFIndexFor(nil)
}

// osExecutable and selfDWARFLoader isolate the system lookups so tests can
// exercise unavailable executable, missing DWARF and loaded-data outcomes.
var (
	// osExecutable resolves the running binary path.
	osExecutable = os.Executable
	// selfDWARFLoader opens DWARF data from an executable path.
	selfDWARFLoader = openSelfDWARF
)

// buildDWARFIndexFor indexes only targets when non-nil. It skips compilation
// units without a target file and skips every non-target subprogram subtree,
// while still visiting all matching entries so duplicate positions are marked
// ambiguous. A nil target set requests the compatibility full index.
func buildDWARFIndexFor(targets map[string]struct{}) map[string]dwarfParamNames {
	exe, err := osExecutable()
	if err != nil {
		return nil
	}
	data := selfDWARFLoader(exe)
	if data == nil {
		return nil
	}
	return buildDWARFIndexFromData(data, targets)
}

// buildDWARFIndexFromData parses an already loaded DWARF data set. Keeping the
// container-specific executable lookup separate lets callers and tests supply
// debug data directly without depending on the running test binary's build flags.
func buildDWARFIndexFromData(data *dwarf.Data,
	targets map[string]struct{}) map[string]dwarfParamNames {
	if data == nil {
		return nil
	}

	index := make(map[string]dwarfParamNames, len(targets))
	reader := data.Reader()
	var files []*dwarf.LineFile
	targetFiles := make(map[string]struct{})
	for key := range targets {
		if separator := strings.LastIndexByte(key, ':'); separator >= 0 {
			targetFiles[key[:separator]] = struct{}{}
		}
	}
	targeted := targets != nil

	// The reader is a flat pre-order walk. Track depth so only direct formal
	// parameter children of a subprogram contribute to its signature; parameters
	// nested in lexical blocks or inlined routines belong to another scope.
	var (
		key             string
		names           []string
		seen            bool
		depth           int
		subprogramDepth int
	)
	flush := func() {
		if seen && key != empty {
			if _, exists := index[key]; exists {
				index[key] = dwarfParamNames{ambiguous: true}
			} else {
				index[key] = dwarfParamNames{names: names}
			}
		}
		key, names, seen = empty, nil, false
	}

	for {
		entry, err := reader.Next()
		if err != nil || entry == nil {
			break
		}
		// A zero tag closes the preceding DWARF child list.
		if entry.Tag == 0 {
			depth--
			continue
		}
		switch entry.Tag {
		case dwarf.TagCompileUnit:
			flush()
			files = nil
			// DeclFile indexes this unit line-table file list. Older DWARF
			// versions reserve entry 0; DWARF 5 may use it as a real file.
			if lr, lErr := data.LineReader(entry); lErr == nil && lr != nil {
				files = lr.Files()
			}
			if targeted {
				matches := false
				for _, file := range files {
					if file != nil {
						if _, ok := targetFiles[file.Name]; ok {
							matches = true
							break
						}
					}
				}
				if !matches {
					reader.SkipChildren()
					continue
				}
			}
		case dwarf.TagSubprogram:
			flush()
			var declFile, declLine int64
			hasDeclFile := false
			for _, field := range entry.Field {
				switch field.Attr {
				case dwarf.AttrDeclFile:
					if v, ok := field.Val.(int64); ok {
						declFile = v
						hasDeclFile = true
					}
				case dwarf.AttrDeclLine:
					if v, ok := field.Val.(int64); ok {
						declLine = v
					}
				}
			}
			if hasDeclFile && declFile >= 0 && declFile < int64(len(files)) &&
				files[declFile] != nil && declLine > 0 {
				key = files[declFile].Name + colon + strconv.FormatInt(declLine, 10)
				if targeted {
					_, seen = targets[key]
				} else {
					seen = true
				}
			}
			if targeted && !seen {
				reader.SkipChildren()
				continue
			}
			subprogramDepth = depth
		case dwarf.TagFormalParameter:
			if seen && depth == subprogramDepth+1 {
				var name string
				output := false
				for _, field := range entry.Field {
					switch field.Attr {
					case dwarf.AttrName:
						if v, ok := field.Val.(string); ok {
							name = v
						}
					case dwarf.AttrVarParam:
						if v, ok := field.Val.(bool); ok {
							output = v
						}
					}
				}
				// Preserve the slot for unnamed and blank inputs. Synthetic ~pN
				// names are compiler artifacts, not usable request bind keys.
				if !output && !strings.HasPrefix(name, "~r") {
					if name == empty || name == "_" || strings.HasPrefix(name, "~p") {
						name = empty
					}
					names = append(names, name)
				}
			}
		}
		if entry.Children {
			depth++
		}
	}
	flush()

	return index
}

// dwarfFile is the shape the three platform container formats share: pe.File,
// elf.File and macho.File all expose exactly these two methods. Wrapping their
// openers in it lets openSelfDWARF try each format in one loop.
type dwarfFile interface {
	DWARF() (*dwarf.Data, error)
	Close() error
}

// dwarfOpener opens one platform container format and returns it as a
// dwarfFile; openSelfDWARF tries each opener in turn.
type dwarfOpener func(string) (dwarfFile, error)

// openSelfDWARF reads the DWARF section from the executable at path, trying each
// platform format. The debug sections are loaded into memory by the call, so
// closing the file afterward is safe. It returns nil for an unknown format, for a
// binary with no DWARF (e.g. built with -w / -s), or when the DWARF data cannot
// be read — none of these leave a usable result behind.
func openSelfDWARF(path string) *dwarf.Data {
	openers := []dwarfOpener{
		func(p string) (dwarfFile, error) { return pe.Open(p) },
		func(p string) (dwarfFile, error) { return elf.Open(p) },
		func(p string) (dwarfFile, error) { return macho.Open(p) },
	}
	for _, open := range openers {
		f, err := open(path)
		if err != nil {
			// The file uses a different container format.
			continue
		}
		data, dErr := f.DWARF()
		_ = f.Close()
		if dErr != nil {
			// The container matched but has no readable DWARF data.
			return nil
		}
		return data
	}
	return nil
}
