// Copyright 2026-present lutme. All rights reserved.
// Use of this source code is governed by a MIT style
// license that can be found in the LICENSE file.

package gbl

import (
	"fmt"
	"net"
	"runtime/debug"
	"strconv"
	"strings"
)

// bannerArt is the startup logo, a small fixed mark that lives beside the code
// that renders it. The raw string's backticks preserve the alignment
// byte-for-byte, so the box-drawing art stays intact without any escaping.
var bannerArt = `  ┏━╸  ┏┓   ╻
  ┃╺┓  ┣┻┓  ┃      zero-dependency Go web framework
  ┗━┛  ┗━┛  ┗━╸`

// startupSuccessMessage is the one-line confirmation emitted right after the
// banner block. Unlike the art it is an ordinary Info record, so it carries the
// Logger's timestamp and level (and console tint) and lands in the log files as
// an explicit "we are live" event.
const startupSuccessMessage = "started successfully, ready to accept requests"

// printBanner emits the startup banner through the engine's Logger so a
// redirected or structured sink captures startup like any other event. The whole
// block (logo plus status table) goes out verbatim: through BannerLogger when
// present, otherwise as a single Info record, so no timestamp or level label
// crowds the art. A one-line startupSuccessMessage Info record follows the block,
// and that line does carry the Logger's timestamp and level like any event.
func (e *engine) printBanner(cfg *gblConfig, addr net.Addr) {
	block := e.renderBanner(cfg, addr)
	if bl, ok := e.logger.(BannerLogger); ok {
		bl.Banner(block)
	} else {
		e.logger.Info(block)
	}
	e.logger.Info(startupSuccessMessage)
}

// renderBanner assembles the display block: the logo, then an aligned status
// table. It is layout only; the actual sink is chosen by printBanner. The
// trailing blank line separates the status table from whatever log line follows
// (the startup success record) so the block does not crowd it.
func (e *engine) renderBanner(cfg *gblConfig, addr net.Addr) string {
	scheme := "http"
	if cfg.useTLS {
		scheme = "https"
	}

	rows := [][2]string{
		{"version", bannerVersion()},
		{"listening", scheme + "://" + displayHost(addr)},
		{"tls", yesNo(cfg.useTLS)},
		{"routes", strconv.Itoa(e.countRoutes())},
	}

	lines := make([]string, len(rows))
	for i, row := range rows {
		lines[i] = fmt.Sprintf("  %-10s %s", row[0], row[1])
	}

	return strings.TrimRight(bannerArt, "\n") + "\n\n" + strings.Join(lines, "\n") + "\n"
}

// bannerVersion reports the version the binary was built with, read from the
// embedded build info so no version constant has to be maintained in source. An
// untagged or local build yields "(devel)".
func bannerVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Version == null {
		return "(devel)"
	}
	return info.Main.Version
}

// countRoutes totals every handler installed across all method trees. It runs
// after generateRoute, so the number matches exactly what serveHTTP can match,
// counting per concrete route rather than per registration (a Match across
// several verbs installs one line per verb, each genuinely routable).
func (e *engine) countRoutes() int {
	var total int
	for _, root := range e.roots {
		total += countRouteNodes(root)
	}
	return total
}

// countRouteNodes counts terminal (exist) nodes in the subtree rooted at node.
func countRouteNodes(node *routeNode) int {
	if node == nil {
		return 0
	}

	count := 0
	if node.exist {
		count++
	}
	for _, child := range node.staticNode {
		count += countRouteNodes(child)
	}
	for _, child := range node.varNode {
		count += countRouteNodes(child)
	}
	return count
}

// displayHost turns a wildcard bind address into a browsable one: 0.0.0.0, ::
// or an empty host all mean "every interface", which is best shown to a human as
// localhost. A concrete host is kept as given. The port always carries through.
func displayHost(addr net.Addr) string {
	if addr == nil {
		return "?"
	}
	host, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		return addr.String()
	}
	switch host {
	case null, "0.0.0.0", "::", "[::]":
		host = "localhost"
	}
	return host + colon + port
}

// yesNo renders a boolean feature state as a word for the status block.
func yesNo(on bool) string {
	if on {
		return "enabled"
	}
	return "disabled"
}
