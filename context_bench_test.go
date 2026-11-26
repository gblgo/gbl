// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"reflect"
	"testing"
)

type ctxTestBenchPayload struct {
	Name  string
	Count int
}

var (
	ctxTestBenchResult any
	ctxTestBenchInt    int
	ctxTestBenchOK     bool
)

func BenchmarkContextKeysSet(b *testing.B) {
	c := &Context{}
	value := &ctxTestBenchPayload{Name: "value", Count: 1}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		c.Set("ctxTest-key", value)
	}
}

func BenchmarkContextKeysGet(b *testing.B) {
	c := &Context{}
	c.Set("ctxTest-key", 42)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		ctxTestBenchInt, ctxTestBenchOK = c.Get[int]("ctxTest-key")
	}
}

func BenchmarkContextKeysValue(b *testing.B) {
	c := &Context{}
	c.Set("ctxTest-key", 42)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		ctxTestBenchInt = c.Value[int]("ctxTest-key")
	}
}

func BenchmarkContextResultUncached(b *testing.B) {
	payload := &ctxTestBenchPayload{Name: "value", Count: 1}
	c := &Context{ret: reflect.ValueOf(payload)}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		c.result = nil
		ctxTestBenchResult = c.Result()
	}
}

func BenchmarkContextResultMemoized(b *testing.B) {
	payload := &ctxTestBenchPayload{Name: "value", Count: 1}
	c := &Context{ret: reflect.ValueOf(payload)}
	ctxTestBenchResult = c.Result()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		ctxTestBenchResult = c.Result()
	}
}
