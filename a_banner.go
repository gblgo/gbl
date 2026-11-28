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
	// The raw string backticks preserve the alignment byte-for-byte, so the
	// box-drawing art stays intact without any escaping.
	bannerArt = `  ┏━╸  ┏┓   ╻
  ┃╺┓  ┣┻┓  ┃
  ┗━┛  ┗━┛  ┗━╸`

	// startupSuccessMessage is the one-line confirmation emitted right after
	// the banner block. Unlike the art it is an ordinary Info record, so it
	// carries the Logger timestamp and level (and console tint) as an
	// explicit "we are live" event.
	startupSuccessMessage = "started successfully, ready to accept requests"

	// module path of the framework itself, matching go.mod: the key for
	// reading the version out of embedded build info, also when the library
	// is only a dependency of the consumer binary
	modulePath = "github.com/gblgo/gbl"
	// version stand-in for an untagged main module or local replacement
	versionDevel = "(devel)"
)

// printBanner emits the startup banner through the engine Logger so a
// redirected or structured sink captures startup like any other event. The whole
// block (logo plus status table) goes out verbatim: through BannerLogger when
// present, otherwise as a single Info record. The fallback preserves the block,
// but the Logger may add its normal level label, timestamp or structured fields.
// A one-line startupSuccessMessage Info record follows the block and carries the
// Logger metadata like any other event.
func (e *engine) printBanner(cfg *Config, addr net.Addr) {
	block := e.renderBanner(cfg, addr)
	logger := e.currentLogger()
	if bl, ok := logger.(BannerLogger); ok {
		safeLog(func() { bl.Banner(block) })
	} else {
		safeLog(func() { logger.Info(block) })
	}
	safeLog(func() { logger.Info(startupSuccessMessage) })
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

// readBuildInfo is a test seam for the standard library's unavailable-build-info
// path. Production keeps the default implementation.
var readBuildInfo = debug.ReadBuildInfo

// bannerVersion reports the framework version embedded in the running binary.
// It reads build info, so no version constant has to be maintained in source.
func bannerVersion() string {
	info, ok := readBuildInfo()
	if !ok {
		return versionDevel
	}
	return frameworkVersion(info)
}

// frameworkVersion extracts the version of gbl from build info. When gbl is the
// main module (its own examples and tests), it reads Main; when gbl is a
// dependency of the consumer app, it reads the gbl entry in Deps. A replacement
// supplies the version actually built: an untagged local replacement yields
// "(devel)", while a versioned replacement reports its own tag.
func frameworkVersion(info *debug.BuildInfo) string {
	if info == nil {
		return versionDevel
	}
	if info.Main.Path == modulePath {
		return buildModuleVersion(info.Main)
	}
	for _, dep := range info.Deps {
		if dep != nil && dep.Path == modulePath {
			return buildModuleVersion(*dep)
		}
	}
	return versionDevel
}

// buildModuleVersion resolves a module version through its replacement, if any.
func buildModuleVersion(module debug.Module) string {
	if module.Replace != nil {
		if module.Replace.Version != empty {
			return module.Replace.Version
		}
		return versionDevel
	}
	if module.Version != empty {
		return module.Version
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
	if child := node.varNode; child != nil {
		count += countRouteNodes(child)
	}
	return count
}

// displayHost turns a wildcard bind address into a browsable one: 0.0.0.0, ::
// or an empty host all mean "every interface", which is best shown to a human as
// localhost. A concrete host is kept as given, and JoinHostPort preserves the
// brackets required around IPv6 addresses and zones.
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
	return net.JoinHostPort(host, port)
}

// yesNo renders a boolean feature state as a word for the status block.
func yesNo(on bool) string {
	if on {
		return "enabled"
	}
	return "disabled"
}
