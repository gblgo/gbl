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
// names from the runtime type metadata that reflect exposes. It still emits them
// into the binary's DWARF section by default, and -trimpath does not strip that
// section. So even a sourceless production binary — a multi-stage image, a bare
// executable — carries every function's parameter names. The section also records
// each declaration's file and line, so a handler is matched to its DWARF entry by
// the SAME position key the disk parser uses. Position keys also resolve closures
// and func literals, and are immune to the merging and renaming of identical
// literals introduced in Go 1.27.
//
// It returns nil when the binary has no readable DWARF. This happens most
// commonly when the binary was built with `-ldflags "-w"` or `"-s"`, which strip
// debug information. The caller treats nil as "no names recoverable" and fails
// startup for a handler that binds by name, pointing the user at struct binding
// instead.
//
// DWARF lists a function's formal parameters in source order: the inputs first,
// then pseudo-parameters for the results (`~r0`, ...) and, for methods, the
// receiver (`~p0`). The caller slices the list to the handler's NumIn. That keeps
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
		if seen && key != empty {
			index[key] = names
		}
		key, names, seen = empty, nil, false
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
			if lr, lErr := data.LineReader(entry); lErr == nil && lr != nil {
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
// closing the file afterward is safe. It returns nil for an unknown format, for a
// binary with no DWARF (e.g. built with -w / -s), or when the DWARF data cannot
// be read — none of these leave a usable result behind.
func openSelfDWARF(path string) *dwarf.Data {
	// The three container types share this shape; wrapping the openers lets the
	// branches collapse into one loop.
	type dwarfFile interface {
		DWARF() (*dwarf.Data, error)
		Close() error
	}
	openers := []func(string) (dwarfFile, error){
		func(p string) (dwarfFile, error) { return pe.Open(p) },
		func(p string) (dwarfFile, error) { return elf.Open(p) },
		func(p string) (dwarfFile, error) { return macho.Open(p) },
	}
	for _, open := range openers {
		f, err := open(path)
		if err != nil {
			continue // not this container format
		}
		data, dErr := f.DWARF()
		_ = f.Close()
		if dErr != nil {
			return nil // right format, no readable DWARF: nothing left to try
		}
		return data
	}
	return nil
}
