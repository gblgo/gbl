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

// durationType is cached so the time.Duration special case in setConfigValue can
// compare against it without rebuilding the type on every config field.
var durationType = reflect.TypeFor[time.Duration]()

// gblConfig is the resolved runtime configuration: defaults from defaultConfig,
// then overlaid with values read from the [gbl] section of application.ini. Each
// field maps to an ini key by its name (see formatName); useTLS is derived, not
// set directly.
type gblConfig struct {
	Name              string        // application name
	Port              string        // listen port, without the leading colon
	CertFile          string        // TLS certificate file path
	KeyFile           string        // TLS private key file path
	LogFile           string        // combined file for every level; the lowest-specificity destination
	LogDir            string        // directory holding auto-named info.log/warn.log/error.log
	InfoLog           string        // explicit path for the info level; beats LogDir and LogFile
	WarnLog           string        // explicit path for the warn level; beats LogDir and LogFile
	ErrorLog          string        // explicit path for the error level; beats LogDir and LogFile
	ReadTimeout       time.Duration // max duration to read an entire request
	ReadHeaderTimeout time.Duration // max duration to read request headers
	WriteTimeout      time.Duration // max duration to write an entire response
	IdleTimeout       time.Duration // max idle wait before a keep-alive connection closes
	useTLS            bool          // derived: true when both CertFile and KeyFile are set
}

// defaultConfig returns the baseline used when application.ini is absent or omits
// a key: name "gbl" on port 9801 with the standard server timeouts.
func defaultConfig() *gblConfig {
	return &gblConfig{
		Name:              "gbl",
		Port:              "9801",
		ReadTimeout:       10 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       time.Minute,
		useTLS:            false,
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
		// Obtain INI Tag
		key1, key2 := formatName(field.Name)
		raw, exists := values[key1]
		if !exists {
			raw, exists = values[key2]
			if !exists {
				continue
			}
		}

		if err := setConfigValue(v.Field(i), raw); err != nil {
			return nil, fmt.Errorf("invalid config %s: %w", field.Name, err)
		}
	}

	if cfg.CertFile != null && cfg.KeyFile != null {
		cfg.useTLS = true
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
	if s == null {
		return null, null
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
