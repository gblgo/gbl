// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

func ReadIni(path string) (map[string]map[string]string, error) {

	file, err := os.Open(path)

	if err != nil {
		return nil, fmt.Errorf("error opening file: %w", err)
	}

	defer func(file *os.File) {
		if e := file.Close(); e != nil {
			_ = fmt.Errorf("error closing file: %w", e)
		}
	}(file)

	ini := make(map[string]map[string]string)

	section := ""
	scanner := bufio.NewScanner(file)

	for lineNo := 1; scanner.Scan(); lineNo++ {

		// Remove leading and trailing spaces
		line := strings.TrimSpace(scanner.Text())

		// Ignore blank lines and comments
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// Analysis Group
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(line[1 : len(line)-1])

			if section == "" {
				return nil, fmt.Errorf("error parsing section: line %d missing section", lineNo)
			}

			if _, exists := ini[section]; !exists {
				ini[section] = make(map[string]string)
			}
			continue
		}

		if section == "" {
			return nil, fmt.Errorf("error parsing configuration item: line %d missing section", lineNo)
		}

		before, after, found := strings.Cut(line, "=")
		if !found {
			return nil, fmt.Errorf("error parsing configuration item: line %d missing =", lineNo)
		}

		key := strings.TrimSpace(before)
		value := strings.TrimSpace(after)

		if key == "" {
			return nil, fmt.Errorf("error parsing configuration item: line %d configuration item key cannot be empty", lineNo)
		}

		ini[section][key] = value
	}

	if err = scanner.Err(); err != nil {
		return nil, fmt.Errorf("error scanning file: %w", err)
	}

	return ini, nil
}
