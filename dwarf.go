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
)

// buildDWARFIndex recovers parameter names from the running binary's own DWARF
// debug information, keyed by the source position the runtime reports for each
// function ("<file>:<line>").
//
// This is the production half of name binding. The Go compiler discards parameter
// names from the runtime type metadata that reflect exposes, but it still emits
// them into the binary's DWARF section by default — and -trimpath does not strip
// it. So a sourceless production binary (a multi-stage image, a bare executable)
// still carries every function's parameter names, plus the declaration file and
// line that let us match a handler to its DWARF entry by the SAME position key
// the disk parser uses. Position keys also resolve closures and func literals,
// and are immune to Go 1.27's merging and renaming of identical literals.
//
// It returns nil when the binary has no readable DWARF — most commonly because it
// was built with `-ldflags "-w"` or `"-s"`, which strip debug information. The
// caller treats nil as "no names recoverable" and fails startup for a handler
// that binds by name, pointing the user at struct binding instead.
//
// DWARF lists a function's formal parameters in source order: the inputs first,
// then pseudo-parameters for the results (`~r0`, ...) and, for methods, the
// receiver (`~p0`). The caller slices the list to the handler's NumIn, which keeps
// exactly the input names and drops the trailing result pseudo-parameters.
func buildDWARFIndex() map[string][]string {
	exe, err := os.Executable()
	if err != nil {
		return nil
	}
	data := openSelfDWARF(exe)
	if data == nil {
		return nil
	}

	index := make(map[string][]string)
	reader := data.Reader()
	var files []*dwarf.LineFile

	// The DWARF reader is a flat pre-order walk: a subprogram is followed by its
	// formal parameters, then the next subprogram. We accumulate parameters into
	// the most recent subprogram's key and flush on each new subprogram/unit, so
	// every entry is processed exactly once (reading children by lookahead would
	// consume and lose the following sibling).
	var (
		key   string
		names []string
		seen  bool
	)
	flush := func() {
		if seen && key != "" {
			index[key] = names
		}
		key, names, seen = "", nil, false
	}

	for {
		entry, err := reader.Next()
		if err != nil || entry == nil {
			break
		}
		switch entry.Tag {
		case dwarf.TagCompileUnit:
			flush()
			// DeclFile on a subprogram is a 1-based index into its unit's line-table
			// file list, so the list must be refreshed per compile unit.
			if lr, lerr := data.LineReader(entry); lerr == nil && lr != nil {
				files = lr.Files()
			}
		case dwarf.TagSubprogram:
			flush()
			var declFile, declLine int64
			for _, field := range entry.Field {
				switch field.Attr {
				case dwarf.AttrDeclFile:
					if v, ok := field.Val.(int64); ok {
						declFile = v
					}
				case dwarf.AttrDeclLine:
					if v, ok := field.Val.(int64); ok {
						declLine = v
					}
				}
			}
			if declFile > 0 && int(declFile) < len(files) && declLine > 0 {
				key = files[declFile].Name + colon + strconv.FormatInt(declLine, 10)
				seen = true
			}
		case dwarf.TagFormalParameter:
			if seen {
				var name string
				for _, field := range entry.Field {
					if field.Attr == dwarf.AttrName {
						if v, ok := field.Val.(string); ok {
							name = v
						}
					}
				}
				// An unnamed parameter contributes an empty string, preserving its
				// position so the caller's slice-to-NumIn stays aligned.
				names = append(names, name)
			}
		}
	}
	flush()

	return index
}

// openSelfDWARF reads the DWARF section from the executable at path, trying each
// platform's format. The debug sections are loaded into memory by the call, so
// closing the file afterward is safe. It returns nil for an unknown format or a
// binary with no DWARF (e.g. built with -w / -s).
func openSelfDWARF(path string) *dwarf.Data {
	if f, err := pe.Open(path); err == nil {
		defer f.Close()
		data, _ := f.DWARF()
		return data
	}
	if f, err := elf.Open(path); err == nil {
		defer f.Close()
		data, _ := f.DWARF()
		return data
	}
	if f, err := macho.Open(path); err == nil {
		defer f.Close()
		data, _ := f.DWARF()
		return data
	}
	return nil
}
