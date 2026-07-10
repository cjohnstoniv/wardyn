// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"strings"
	"testing"
)

func TestMavenProxyOpts(t *testing.T) {
	got := mavenProxyOpts("http://wardyn-proxy:3128")
	want := "-Dhttp.proxyHost=wardyn-proxy -Dhttp.proxyPort=3128 -Dhttps.proxyHost=wardyn-proxy -Dhttps.proxyPort=3128 -Dhttp.nonProxyHosts=localhost|127.0.0.1|::1|wardyn-proxy"
	if got != want {
		t.Errorf("mavenProxyOpts:\n got=%q\nwant=%q", got, want)
	}
	// Default port when unspecified; https scheme stripped.
	if p := mavenProxyOpts("https://proxy.example"); p == "" || !strings.Contains(p, "proxyHost=proxy.example") || !strings.Contains(p, "proxyPort=3128") {
		t.Errorf("default-port/https parse = %q", p)
	}
	// Unparseable → empty (no bogus MAVEN_OPTS).
	if p := mavenProxyOpts(""); p != "" {
		t.Errorf("empty proxyURL should yield empty opts, got %q", p)
	}
}
