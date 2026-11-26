// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"reflect"
	"testing"
)

// ctxTestBenchPayload is the concrete handler return used by Result benchmarks.
type ctxTestBenchPayload struct {
	Name  string
	Count int
}

// Benchmark sinks keep optimized-away reads and lookups observable.
var (
	ctxTestBenchResult any
	ctxTestBenchInt    int
	ctxTestBenchOK     bool
)

// BenchmarkContextSetRequestCycle measures pooled Context acquisition, lazy
// keys-map acquisition, four first insertions, and recycling. It reports the
// end-to-end Set lifecycle rather than isolated Set latency; sync.Pool reuse
// depends on runtime garbage collection.
func BenchmarkContextSetRequestCycle(b *testing.B) {
	keys := [...]string{"ctxTest-key-0", "ctxTest-key-1", "ctxTest-key-2", "ctxTest-key-3"}
	values := [...]any{1, "two", true, &ctxTestBenchPayload{Name: "value", Count: 1}}
	b.ReportAllocs()
	b.ReportMetric(float64(len(keys)), "keys/op")
	b.ResetTimer()
	for range b.N {
		c := contextPool.Get().(*Context)
		for i := range keys {
			c.Set(keys[i], values[i])
		}
		c.recycle()
	}
}

// BenchmarkContextKeysGet measures a typed lookup after the key map is warm.
func BenchmarkContextKeysGet(b *testing.B) {
	c := &Context{}
	c.Set("ctxTest-key", 42)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		ctxTestBenchInt, ctxTestBenchOK = c.Get[int]("ctxTest-key")
	}
}

// BenchmarkContextKeysValue measures a typed value read after the key map is warm.
func BenchmarkContextKeysValue(b *testing.B) {
	c := &Context{}
	c.Set("ctxTest-key", 42)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		ctxTestBenchInt = c.Value[int]("ctxTest-key")
	}
}

// BenchmarkContextResult compares first boxing with memoized reads for value and
// pointer returns. Both reflect.Values are addressable, like staged handler
// return storage. Value returns expose the boxing allocation; pointer returns
// retain their allocation-free path because the pointer already lives on the
// heap. The first_boxing case clears only the memo to model a fresh request
// without timing Context allocation.
func BenchmarkContextResult(b *testing.B) {
	payload := &ctxTestBenchPayload{Name: "value", Count: 1}
	valueReturn := reflect.ValueOf(payload).Elem()
	pointerReturn := reflect.New(reflect.TypeFor[*ctxTestBenchPayload]()).Elem()
	pointerReturn.Set(reflect.ValueOf(payload))
	if !valueReturn.CanAddr() || !pointerReturn.CanAddr() {
		b.Fatal("Result benchmark returns must be addressable")
	}

	for _, tc := range []struct {
		name string
		ret  reflect.Value
	}{
		{name: "pointer", ret: pointerReturn},
		{name: "value", ret: valueReturn},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.Run("first_boxing", func(b *testing.B) {
				c := &Context{ret: tc.ret}
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					c.result = nil
					ctxTestBenchResult = c.Result()
				}
			})
			b.Run("memoized", func(b *testing.B) {
				c := &Context{ret: tc.ret}
				ctxTestBenchResult = c.Result()
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					ctxTestBenchResult = c.Result()
				}
			})
		})
	}
}
