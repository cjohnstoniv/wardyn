// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import "testing"

func TestMavenProxyOpts(t *testing.T) {
	got := mavenProxyOpts("http://wardyn-proxy:3128")
	want := "-Dhttp.proxyHost=wardyn-proxy -Dhttp.proxyPort=3128 -Dhttps.proxyHost=wardyn-proxy -Dhttps.proxyPort=3128 -Dhttp.nonProxyHosts=localhost|127.0.0.1|::1|wardyn-proxy"
	if got != want {
		t.Errorf("mavenProxyOpts:\n got=%q\nwant=%q", got, want)
	}
	// Default port when unspecified; https scheme stripped.
	if p := mavenProxyOpts("https://proxy.example"); p == "" || !contains(p, "proxyHost=proxy.example") || !contains(p, "proxyPort=3128") {
		t.Errorf("default-port/https parse = %q", p)
	}
	// Unparseable → empty (no bogus MAVEN_OPTS).
	if p := mavenProxyOpts(""); p != "" {
		t.Errorf("empty proxyURL should yield empty opts, got %q", p)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
