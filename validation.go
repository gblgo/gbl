// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import "reflect"

// Validatable is the framework's zero-dependency validation convention. A bound
// business parameter whose type (or whose pointer) implements Validate() error
// is checked immediately after binding and before the handler runs. Returning a
// non-nil error stops the request with a 400; the error text is written only to
// the server log, so validation may freely describe internal rules without
// leaking them to the client.
//
// Types that do not implement it are bound exactly as before, at no cost.
type Validatable interface {
	Validate() error
}

// validateBusiness runs Validate over every business argument that opted in,
// returning the first failure. Only the business-parameter indexes are visited:
// injected request/writer/context/file arguments carry no client data to check.
func validateBusiness(values []reflect.Value, indexes []int) error {
	for _, i := range indexes {
		if i < 0 || i >= len(values) {
			continue
		}
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
		return target, true
	}
	if v.CanAddr() {
		if target, ok := v.Addr().Interface().(Validatable); ok {
			return target, true
		}
	}
	return nil, false
}
