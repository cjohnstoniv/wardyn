// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"encoding/json"
	"testing"
)

func TestHostCallKeyNamesHostMethodAndPath(t *testing.T) {
	base := HostCallKey("api.example.com", "GET", "/public")
	if base != HostCallKey("api.example.com", "GET", "/public") {
		t.Fatal("the key is not stable")
	}
	for _, other := range []string{
		HostCallKey("api.example.org", "GET", "/public"),
		HostCallKey("api.example.com", "POST", "/public"),
		HostCallKey("api.example.com", "GET", "/public/x"),
		// Length prefixes keep a part from sliding into the next.
		HostCallKey("api.example.comG", "ET", "/public"),
	} {
		if other == base {
			t.Fatalf("a different call has the same key %s", base)
		}
	}
}

func TestHostCallScopeValidate(t *testing.T) {
	good := HostCallScope{Host: "api.example.com", Method: "POST", Path: "/admin/users", HoldSeconds: 90}
	good.CallSHA256 = good.ComputeKey()
	if err := good.Validate(); err != nil {
		t.Fatalf("good scope: %v", err)
	}
	for name, mut := range map[string]func(*HostCallScope){
		"no host":          func(s *HostCallScope) { s.Host = "" },
		"upper host":       func(s *HostCallScope) { s.Host = "API.example.com" },
		"host with port":   func(s *HostCallScope) { s.Host = "api.example.com:443" },
		"no method":        func(s *HostCallScope) { s.Method = "" },
		"lower method":     func(s *HostCallScope) { s.Method = "post" },
		"method not token": func(s *HostCallScope) { s.Method = "PO ST" },
		"no path":          func(s *HostCallScope) { s.Path = "" },
		"query in path":    func(s *HostCallScope) { s.Path = "/admin?x=1" },
		"dot segment":      func(s *HostCallScope) { s.Path = "/a/../admin" },
		"double slash":     func(s *HostCallScope) { s.Path = "//admin" },
		"encoded slash":    func(s *HostCallScope) { s.Path = "/a%2Fb" },
		"undecoded A":      func(s *HostCallScope) { s.Path = "/%41dmin" },
		"negative hold":    func(s *HostCallScope) { s.HoldSeconds = -1 },
	} {
		s := good
		mut(&s)
		if err := s.Validate(); err == nil {
			t.Errorf("%s: Validate passed %+v", name, s)
		}
	}
}

func TestHostCallScopeIsItsOwnKind(t *testing.T) {
	s := HostCallScope{Host: "api.example.com", Method: "GET", Path: "/", HoldSeconds: 90}
	s.CallSHA256 = s.ComputeKey()
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if !IsHostCallScope(raw) || IsMCPCallScope(raw) || IsKubeCallScope(raw) {
		t.Fatalf("host scope: host %v mcp %v kube %v", IsHostCallScope(raw), IsMCPCallScope(raw), IsKubeCallScope(raw))
	}
	for _, in := range []string{
		`{"host":"api.example.com"}`, // an egress_domain scope
		`{"host":"api.example.com","call_sha256":""}`,
		`{"call_sha256":"ab","tool":"t","mcp_server":"s"}`,
		`{"call_sha256":"ab","cluster":"k8s.example.com","host":"api.example.com"}`,
		`[]`,
	} {
		if IsHostCallScope(json.RawMessage(in)) {
			t.Errorf("IsHostCallScope(%s) = true", in)
		}
	}
}

// TestHostCallHoldRuleValidates: effect hold is a rule a policy may carry.
func TestHostCallHoldRuleValidates(t *testing.T) {
	sets := []HostRuleSet{{Rules: []HostRule{{Host: "api.example.com", Methods: []string{"POST"}, Paths: []string{"/admin"}, Effect: HostRuleHold}}}}
	if err := ValidateHostRuleSets(sets); err != nil {
		t.Fatalf("a hold rule was refused: %v", err)
	}
}
