// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"errors"
	"maps"
	"os"
	"sync"
)

// iniConfigData is the process-wide configuration snapshot.
//
// It is written exactly once, inside loadIni, guarded by iniOnce, and is only
// ever handed out as a copy. Nothing in the package mutates it afterwards, so
// every read is safe from any goroutine at any time: registration code,
// handlers and the startup path all see the same immutable data.
var (
	iniOnce       sync.Once
	iniConfigData map[string]map[string]string
	iniLoadErr    error
)

// loadIni Reads application.ini at most once and freezes the snapshot.
//
// The file is read on first access instead of inside Run, so configuration is
// already available while routes and middleware are registered.
//
// A missing application.ini is a normal empty configuration: defaults apply and
// every getter simply reports "not found". A file that exists but cannot be
// parsed is fatal, and is reported on every read rather than swallowed by the
// sync.Once, so a recovered panic cannot permanently hide the real problem.
func loadIni() {
	iniOnce.Do(func() {
		ini, err := ReadIni(applicationIni)
		switch {
		case errors.Is(err, os.ErrNotExist):
			// No configuration file: keep the snapshot empty.
		case err != nil:
			iniLoadErr = err
		default:
			iniConfigData = ini
		}
	})
}

// iniSnapshot Returns the frozen snapshot, panicking only on a broken file.
func iniSnapshot() map[string]map[string]string {
	loadIni()
	if iniLoadErr != nil {
		panic(iniLoadErr)
	}
	return iniConfigData
}

// IniBySection Get its configuration items through the section
//
// The returned map is a copy; changing it does not affect the configuration.
// It may be called before or after Run, from any goroutine.
func IniBySection(section string) (map[string]string, bool) {

	values, found := iniSnapshot()[section]
	if !found {
		return nil, false
	}

	// Return a copy to prevent external modification of internal configuration
	result := make(map[string]string, len(values))
	maps.Copy(result, values)

	return result, true
}

// IniBySectionAndKey Get the exact configuration item through section and key
//
// It may be called before or after Run, from any goroutine.
func IniBySectionAndKey(section, key string) (string, bool) {

	values, found := iniSnapshot()[section]
	if !found {
		return null, false
	}

	value, found := values[key]

	return value, found
}
