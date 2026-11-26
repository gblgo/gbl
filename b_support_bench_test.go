// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"net"
	"testing"
)

var bannerTestBenchmarkSink string

func BenchmarkBannerVersion(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		bannerTestBenchmarkSink = bannerVersion()
	}
}

func BenchmarkDisplayHost(b *testing.B) {
	values := []struct {
		name string
		addr net.Addr
	}{
		{name: "nil", addr: nil},
		{name: "IPv4", addr: &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 8080}},
		{name: "IPv4Wildcard", addr: &net.TCPAddr{IP: net.IPv4zero, Port: 9801}},
		{name: "IPv6", addr: &net.TCPAddr{IP: net.ParseIP("::1"), Port: 8080}},
		{name: "IPv6Wildcard", addr: &net.TCPAddr{IP: net.IPv6zero, Port: 9801}},
		{name: "unparsed", addr: bannerTestAddr("/tmp/gbl.sock")},
	}

	for _, tt := range values {
		b.Run(tt.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				bannerTestBenchmarkSink = displayHost(tt.addr)
			}
		})
	}
}
