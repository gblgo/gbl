// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import "maps"

var iniConfigData map[string]map[string]string = nil

// IniBySection Get its configuration items through the section
func IniBySection(section string) (map[string]string, bool) {

	values, found := iniConfigData[section]
	if !found {
		return nil, false
	}

	// Return a copy to prevent external modification of internal configuration
	result := make(map[string]string, len(values))
	maps.Copy(result, values)

	return result, true
}

// IniBySectionAndKey Get the exact configuration item through section and key
func IniBySectionAndKey(section, key string) (string, bool) {

	values, found := iniConfigData[section]
	if !found {
		return "", false
	}

	value, found := values[key]

	return value, found
}
