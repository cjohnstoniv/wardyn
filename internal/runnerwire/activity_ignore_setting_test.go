// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// The hosts whose allowed requests are not idle activity are advisory: a run that carries them names no proxy
// setting, so an older remote runner that drops the list is still given the run and keeps its old accounting.
func TestProxySettingsSet_ActivityIgnoreHostsIsAdvisory(t *testing.T) {
	pc := runner.ProxyConfig{ActivityIgnoreHosts: []string{"mask.icloud.com"}}
	if got := ProxySettingsSet(pc); len(got) != 0 {
		t.Fatalf("ProxySettingsSet = %v, want none: the list is advisory", got)
	}
	if slices.Contains(ProxySettings, "activity_ignore_hosts") {
		t.Fatalf("ProxySettings = %v, want it not to gate activity_ignore_hosts", ProxySettings)
	}
	if why := proxyFieldExemptions["ActivityIgnoreHosts"]; why == "" {
		t.Fatal("ActivityIgnoreHosts has no exemption reason in proxyFieldExemptions")
	}
}
