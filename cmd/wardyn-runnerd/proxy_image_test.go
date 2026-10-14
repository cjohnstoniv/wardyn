// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import "testing"

func TestReleaseProxyImageIsTheVersionsOwn(t *testing.T) {
	for version, want := range map[string]string{
		"0.9.1":             "ghcr.io/cjohnstoniv/wardyn-proxy:0.9.1",
		"0.9.1-rc.3":        "ghcr.io/cjohnstoniv/wardyn-proxy:0.9.1-rc.3",
		"0.9.1+abc123def":   "",
		"0.9.1+abc-dirty":   "",
		"0.9.1-rc.3+abc123": "",
	} {
		old := buildVersion
		buildVersion = func() string { return version }
		got := releaseProxyImage()
		buildVersion = old
		if got != want {
			t.Errorf("%s: %q, want %q", version, got, want)
		}
	}
}

// serve takes the same default, and an environment value still wins over it.
func TestServeProxyImageDefaultsToTheReleaseImage(t *testing.T) {
	old := buildVersion
	buildVersion = func() string { return "0.9.1" }
	t.Cleanup(func() { buildVersion = old })
	t.Setenv("WARDYN_PROXY_IMAGE", "")
	if got := newServeCommand(nil).Flags().Lookup("proxy-image").DefValue; got != "ghcr.io/cjohnstoniv/wardyn-proxy:0.9.1" {
		t.Errorf("serve --proxy-image defaults to %q", got)
	}
	t.Setenv("WARDYN_PROXY_IMAGE", "local/proxy:dev")
	if got := newServeCommand(nil).Flags().Lookup("proxy-image").DefValue; got != "local/proxy:dev" {
		t.Errorf("the environment did not win: %q", got)
	}
}
