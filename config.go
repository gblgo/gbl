// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"
)

var durationType = reflect.TypeFor[time.Duration]()

type gblConfig struct {
	Name              string
	Port              string
	ReadTimeout       time.Duration
	ReadHeaderTimeout time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
}

func defaultConfig() *gblConfig {
	return &gblConfig{
		Name:              "gbl",
		Port:              "9801",
		ReadTimeout:       10 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       time.Minute,
	}
}

// generateConfig Automatically map ini configuration based on struct tags
func generateConfig(ini map[string]map[string]string) (*gblConfig, error) {

	cfg := defaultConfig()

	values := ini["gbl"]
	if values == nil {
		return cfg, nil
	}

	v := reflect.ValueOf(cfg).Elem()
	t := v.Type()

	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		// Get INI Tag
		key1, key2 := formatName(field.Name)
		raw, exists := values[key1]
		if !exists {
			raw, exists = values[key2]
			if !exists {
				continue
			}
		}
		println(field.Name, raw)
		if err := setConfigValue(v.Field(i), raw); err != nil {
			return nil, fmt.Errorf("invalid config %s: %w", field.Name, err)
		}
	}

	return cfg, nil
}

// setConfigValue Convert a string to the corresponding field type
func setConfigValue(field reflect.Value, value string) error {
	if !field.CanSet() {
		return fmt.Errorf("field cannot be set")
	}

	// Special handling time.Duration
	if field.Type() == durationType {
		d, err := time.ParseDuration(value)
		if err != nil {
			return err
		}
		field.SetInt(int64(d))
		return nil
	}

	switch field.Kind() {
	case reflect.String:
		field.SetString(value)

	case reflect.Bool:
		v, err := strconv.ParseBool(value)
		if err != nil {
			return err
		}
		field.SetBool(v)

	case reflect.Int, reflect.Int8, reflect.Int16,
		reflect.Int32, reflect.Int64:
		v, err := strconv.ParseInt(value, 10, field.Type().Bits())
		if err != nil {
			return err
		}
		field.SetInt(v)

	case reflect.Uint, reflect.Uint8, reflect.Uint16,
		reflect.Uint32, reflect.Uint64:
		v, err := strconv.ParseUint(value, 10, field.Type().Bits())
		if err != nil {
			return err
		}
		field.SetUint(v)

	case reflect.Float32, reflect.Float64:
		v, err := strconv.ParseFloat(value, field.Type().Bits())
		if err != nil {
			return err
		}
		field.SetFloat(v)

	default:
		return fmt.Errorf("unsupported configuration type: %s", field.Type())
	}

	return nil
}

// formatName Generate keys in two formats
// Give an example: Name -> name, name; ReadTimeout -> readTimeout, read-timeout
func formatName(s string) (lowerFirst, kebabCase string) {
	if s == "" {
		return "", ""
	}

	var result strings.Builder

	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				result.WriteByte('-')
			}
			r += 'a' - 'A'
		}

		result.WriteRune(r)
	}

	lowerFirst = strings.ToLower(s[:1]) + s[1:]
	kebabCase = result.String()

	return lowerFirst, kebabCase
}
