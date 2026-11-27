// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import "testing"

var (
	routeTestMethodSink   httpMethod
	routeTestMethodOKSink bool
	routeTestPathSink     string
	routeTestMetaSink     []segmentMeta
	routeTestHandlerSink  *routeHandler
	routeTestParamsSink   map[string]string
	routeTestNamesSink    []string
)

func BenchmarkRouteMethodOf(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		routeTestMethodSink, routeTestMethodOKSink = methodOf("pAtCh")
	}
}

func BenchmarkRouteDealPath(b *testing.B) {
	e := newEngine()
	e.logger = routeTestDiscardLogger{}
	b.Cleanup(e.cancelStreams)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		routeTestMetaSink, routeTestPathSink = e.dealPath(" //api///users//:id/ ")
	}
}

func BenchmarkRouteStaticMatch(b *testing.B) {
	e := newEngine()
	e.logger = routeTestDiscardLogger{}
	b.Cleanup(e.cancelStreams)
	e.register(methodGet, "/api/users/me", routeTestNoArgHandler)
	e.generateRoute()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		routeTestHandlerSink, routeTestParamsSink = e.matchNode(methodGet, "/api/users/me", nil)
	}
}

// BenchmarkRouteStaticNormalizeMatch measures the pooled non-normalized static probe.
func BenchmarkRouteStaticNormalizeMatch(b *testing.B) {
	e := newEngine()
	e.logger = routeTestDiscardLogger{}
	b.Cleanup(e.cancelStreams)
	e.register(methodGet, "/api/users/me", routeTestNoArgHandler)
	e.generateRoute()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		routeTestHandlerSink, routeTestParamsSink = e.matchNode(methodGet, "/api//users/me/", nil)
	}
	b.StopTimer()
	if routeTestHandlerSink == nil || routeTestParamsSink != nil {
		b.Fatal("normalized static route did not match")
	}
}

func BenchmarkRouteDynamicMatch(b *testing.B) {
	e := newEngine()
	e.logger = routeTestDiscardLogger{}
	b.Cleanup(e.cancelStreams)
	e.register(methodGet, "/api/users/:id/posts/:postID", routeTestNoArgHandler)
	e.generateRoute()
	params := make(map[string]string, 2)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		routeTestHandlerSink, routeTestParamsSink = e.matchNode(methodGet, "/api/users/42/posts/7", params)
	}
}

func BenchmarkRouteNormalizeParamNames(b *testing.B) {
	names := []string{"id", "", "count"}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		routeTestNamesSink = normalizeParamNames(names, 4)
	}
}
