// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package egress

import (
	"os"
	"strings"
	"testing"
)

// TestHostListRuleSources pins the three rule sources the host list decides: their wire values, and that
// the proxy files emitting them still write those values. A rename on either side fails here.
func TestHostListRuleSources(t *testing.T) {
	for _, c := range []struct{ name, got, want string }{
		{"allowed", RuleSourcePolicyAllowed, "policy:allowed"},
		{"default-deny", RuleSourceDefaultDeny, "policy:default-deny"},
		{"denied", RuleSourcePolicyDenied, "policy:denied"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
	read := func(path string) string {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		return string(b)
	}
	for _, f := range []string{"proxy/connect_gate.go", "proxy/llm_host_deny.go", "proxy/mcp_broker.go", "refusal.go"} {
		if !strings.Contains(read(f), `"`+RuleSourcePolicyDenied+`"`) {
			t.Errorf("%s no longer writes %q", f, RuleSourcePolicyDenied)
		}
	}
	proxy := read("proxy/proxy.go")
	for _, c := range []string{"RuleSourceDefaultDeny", "RuleSourcePolicyAllowed"} {
		if !strings.Contains(proxy, "egress."+c) {
			t.Errorf("proxy/proxy.go no longer emits egress.%s", c)
		}
	}
	if !strings.Contains(read("refusal.go"), `"`+RuleSourceDefaultDeny+`"`) {
		t.Errorf("refusal.go no longer maps %q", RuleSourceDefaultDeny)
	}
}
