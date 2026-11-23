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

// Constants used only by this file: the startup logo, the line that confirms
// the server is live, and the build-info keys that resolve the version.
const (
	// bannerArt is the startup logo, a small fixed mark rendered once at Run.
	// The raw string's backticks preserve the alignment byte-for-byte, so the
	// box-drawing art stays intact without any escaping.
	bannerArt = `  ┏━╸  ┏┓   ╻
  ┃╺┓  ┣┻┓  ┃
  ┗━┛  ┗━┛  ┗━╸`

	// startupSuccessMessage is the one-line confirmation emitted right after
	// the banner block. Unlike the art it is an ordinary Info record, so it
	// carries the Logger's timestamp and level (and console tint) as an
	// explicit "we are live" event.
	startupSuccessMessage = "started successfully, ready to accept requests"

	// module path of the framework itself, matching go.mod: the key for
	// reading the version out of embedded build info, also when the library
	// is only a dependency of the consumer binary
	modulePath = "github.com/gblgo/gbl"
	// version stand-in when build info carries no tag: a local or replaced checkout
	versionDevel = "(devel)"
)

// printBanner emits the startup banner through the engine's Logger so a
// redirected or structured sink captures startup like any other event. The whole
// block (logo plus status table) goes out verbatim: through BannerLogger when
// present, otherwise as a single Info record, so no timestamp or level label
// crowds the art. A one-line startupSuccessMessage Info record follows the block,
// and that line does carry the Logger's timestamp and level like any event.
func (e *engine) printBanner(cfg *Config, addr net.Addr) {
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
func (e *engine) renderBanner(cfg *Config, addr net.Addr) string {
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

// bannerVersion reports the framework version embedded in the running binary.
// It reads build info, so no version constant has to be maintained in source.
func bannerVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return versionDevel
	}
	return frameworkVersion(info)
}

// frameworkVersion extracts the version of gbl from build info. When gbl is the
// main module (its own examples and tests), it reads Main.Version. When gbl is a
// dependency of the consumer's app, it reads the gbl entry in Deps, which carries
// the tagged version that app requires. A local or replaced build has no tag,
// so it yields "(devel)".
func frameworkVersion(info *debug.BuildInfo) string {
	if info.Main.Path == modulePath {
		if info.Main.Version != empty {
			return info.Main.Version
		}
		return versionDevel
	}
	for _, dep := range info.Deps {
		if dep.Path == modulePath {
			if dep.Version != empty {
				return dep.Version
			}
			break
		}
	}
	return versionDevel
}

// countRoutes totals every handler serveHTTP can reach: the terminal nodes of
// all method trees plus every entry of the static-route maps. It runs after
// generateRoute, so the number counts per concrete route rather than per
// registration (a Match across several verbs installs one line per verb, each
// genuinely routable).
func (e *engine) countRoutes() int {
	var total int
	for _, root := range e.roots {
		total += countRouteNodes(root)
	}
	for _, static := range e.staticRoutes {
		total += len(static)
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
	case empty, "0.0.0.0", "::":
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
