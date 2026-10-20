// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
)

// The previous-release proxy loads a config only while port_rule is absent
// (internal/api proxy_config_skew_test.go), so off must be sent as no key.
func TestEgressPortRuleOffIsNoKey(t *testing.T) {
	if got := egressPortRule(proxy.PortRuleOff); got != "" {
		t.Fatalf("egressPortRule(off) = %q, want no key", got)
	}
	if got := egressPortRule(proxy.PortRuleOn); got != proxy.PortRuleOn {
		t.Fatalf("egressPortRule(on) = %q", got)
	}
}
