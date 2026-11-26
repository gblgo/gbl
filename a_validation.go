// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import "reflect"

// Validatable is the framework zero-dependency validation convention. A bound
// business parameter whose type (or whose pointer) implements Validate() error
// is checked immediately after binding and before the handler runs. Returning a
// non-nil error stops the request with a 400 whose body is the error text: the
// message is authored by the application developer, so echoing it respects
// their intent — whoever writes it owns what it discloses.
//
// Types that do not implement it are bound exactly as before, at no cost. A nil
// pointer is passed to the handler without calling Validate.
type Validatable interface {
	Validate() error
}

// validateBusiness runs Validate over the precomputed validatable indexes,
// returning the first failure. buildBindPlan already filtered these by type, so
// a handler whose parameters do not implement Validatable passes an empty slice
// and pays no per-request reflection or interface boxing.
func validateBusiness(values []reflect.Value, indexes []int) error {
	for _, i := range indexes {
		target, ok := asValidatable(values[i])
		if !ok {
			continue
		}
		if err := target.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// asValidatable reports whether a bound value satisfies Validatable, trying the
// value itself first and then its address so that a struct parameter bound by
// value still reaches a Validate method declared on the pointer receiver.
func asValidatable(v reflect.Value) (Validatable, bool) {
	if !v.IsValid() || !v.CanInterface() {
		return nil, false
	}
	if target, ok := v.Interface().(Validatable); ok {
		value := reflect.ValueOf(target)
		if value.Kind() == reflect.Pointer && value.IsNil() {
			return nil, false
		}
		return target, true
	}
	if v.CanAddr() {
		if target, ok := v.Addr().Interface().(Validatable); ok {
			return target, true
		}
	}
	return nil, false
}
