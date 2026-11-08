// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// A run that names hosts whose allowed traffic is not activity names activity_ignore_hosts, so a remote
// runner that would drop the list is refused the run instead of reading a macOS guest's background traffic
// as the run working.
func TestProxySettingsSet_NamesActivityIgnoreHosts(t *testing.T) {
	if got := ProxySettingsSet(runner.ProxyConfig{}); len(got) != 0 {
		t.Fatalf("an unset config names %v", got)
	}
	pc := runner.ProxyConfig{ActivityIgnoreHosts: []string{"mask.icloud.com"}}
	if got := ProxySettingsSet(pc); !slices.Equal(got, []string{"activity_ignore_hosts"}) {
		t.Fatalf("ProxySettingsSet = %v, want [activity_ignore_hosts]", got)
	}
	if !slices.Contains(ProxySettings, ProxySettingActivityIgnoreHosts) {
		t.Fatalf("ProxySettings = %v, want it to list %s", ProxySettings, ProxySettingActivityIgnoreHosts)
	}
}
