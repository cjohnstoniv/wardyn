// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"strings"
	"testing"
)

func TestNormalizeRequestPath(t *testing.T) {
	for _, tc := range []struct {
		raw, want string
		refused   string // a substring of the refusal; "" when the path normalises
	}{
		{"/a/b", "/a/b", ""},
		{"", "/", ""},
		{"/a?x=1#f", "/a", ""},
		{"/a/", "/a/", ""},
		{"/", "/", ""},
		{"/Admin;x=1/b", "/Admin;x=1/b", ""},
		{"/a/./b", "", "normal form"},
		{"/a/../b", "", "normal form"},
		{"/../../etc", "", "normal form"},
		{"/a//b///c", "", "normal form"},
		{"//a", "", "normal form"},
		{"/a/b/..", "", "normal form"},
		{"/a/.", "", "normal form"},
		{"/admin//../users", "", "normal form"},
		{"/public/..;x/admin", "", "normal form"},
		{"/a/.;x", "", "normal form"},
		{"/;x/admin", "", "normal form"},
		{"/a;x", "/a;x", ""},
		{"/%41dmin", "/Admin", ""},
		{"/%2e%2e/admin", "", "normal form"},
		{"/x/%2E%2E/admin", "", "normal form"},
		{"/x/%2E/admin", "", "normal form"},
		{"/a%20b", "/a%20b", ""},
		{"/a%3fb", "/a%3Fb", ""},
		{"/a%2Fb", "", "encoded"},
		{"/a%2fb", "", "encoded"},
		{"/a%5Cb", "", "encoded"},
		{"/a%2F..%2Fadmin", "", "encoded"},
		{"/a%00b", "", "%00"},
		{"/a\x01b", "", "control"},
		{"/a%0Ab", "", "control"},
		{"/a\\b", "", "backslash"},
		{"/a%zz", "", "malformed"},
		{"/a%4", "", "malformed"},
		{"a/b", "", "start with /"},
	} {
		got, err := NormalizeRequestPath(tc.raw)
		if tc.refused != "" {
			if err == nil || !strings.Contains(err.Error(), tc.refused) {
				t.Errorf("NormalizeRequestPath(%q) = %q, %v; want a refusal naming %q", tc.raw, got, err, tc.refused)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("NormalizeRequestPath(%q) = %q, %v; want %q", tc.raw, got, err, tc.want)
		}
	}
}

func TestEvalHostRules(t *testing.T) {
	sets := []HostRuleSet{{Rules: []HostRule{
		{Host: "h.example", Methods: []string{"DELETE"}, Effect: HostRuleDeny},
		{Host: "h.example", Paths: []string{"/repos"}, Effect: HostRuleAllow},
	}}}
	for _, tc := range []struct {
		host, method, path string
		want               HostRuleEffect
		named              bool
		set, rule          int
	}{
		{"other.example", "DELETE", "/x", HostRuleAllow, false, -1, -1},
		{"h.example", "GET", "/repos", HostRuleAllow, true, -1, -1},
		{"h.example", "get", "/repos/a/b", HostRuleAllow, true, -1, -1},
		{"h.example", "DELETE", "/repos/a", HostRuleDeny, true, 0, 0},
		{"h.example", "GET", "/reposx", HostRuleDeny, true, 0, -1},
		{"h.example", "GET", "/x/../repos", HostRuleDeny, true, -1, -1},
		{"h.example", "GET", "/repos/..%2Fadmin", HostRuleDeny, true, -1, -1},
		{"h.example", "GET", "/repos/../admin", HostRuleDeny, true, -1, -1},
		{"h.example", "GET", "/repos;x=1", HostRuleDeny, true, 0, -1},
		{"h.example", "GET", "/repos/a;x=1", HostRuleAllow, true, -1, -1},
	} {
		d := EvalHostRules(sets, tc.host, tc.method, tc.path)
		if d.Effect != tc.want || d.Named != tc.named || d.Set != tc.set || d.Rule != tc.rule {
			t.Errorf("%s %s %s = %+v, want effect %s named %v set %d rule %d", tc.host, tc.method, tc.path, d, tc.want, tc.named, tc.set, tc.rule)
		}
	}
}

func TestValidateHostRuleSets(t *testing.T) {
	ok := HostRule{Host: "api.example.com", Methods: []string{"GET"}, Paths: []string{"/a"}, Effect: HostRuleAllow}
	with := func(f func(*HostRule)) []HostRuleSet {
		r := ok
		f(&r)
		return []HostRuleSet{{Rules: []HostRule{r}}}
	}
	if err := ValidateHostRuleSets(with(func(*HostRule) {})); err != nil {
		t.Fatalf("a valid rule was refused: %v", err)
	}
	many := make([]HostRuleSet, MaxHostRuleSets+1)
	for i := range many {
		many[i] = HostRuleSet{Rules: []HostRule{ok}}
	}
	long := []HostRuleSet{{Rules: make([]HostRule, MaxHostRulesPerSet+1)}}
	for i := range long[0].Rules {
		long[0].Rules[i] = ok
	}
	for name, tc := range map[string]struct {
		sets []HostRuleSet
		want string
	}{
		"unknown effect":  {with(func(r *HostRule) { r.Effect = "maybe" }), "effects: allow, deny, hold"},
		"uppercase host":  {with(func(r *HostRule) { r.Host = "API.example.com" }), "lowercase"},
		"wildcard host":   {with(func(r *HostRule) { r.Host = "*.example.com" }), "wildcard"},
		"host with port":  {with(func(r *HostRule) { r.Host = "api.example.com:443" }), "no port"},
		"empty host":      {with(func(r *HostRule) { r.Host = "" }), "required"},
		"lowercase verb":  {with(func(r *HostRule) { r.Methods = []string{"get"} }), "uppercase"},
		"not a token":     {with(func(r *HostRule) { r.Methods = []string{"GET /"} }), "no HTTP method token"},
		"relative path":   {with(func(r *HostRule) { r.Paths = []string{"a"} }), "start with /"},
		"query":           {with(func(r *HostRule) { r.Paths = []string{"/a?b"} }), "no query"},
		"not normal form": {with(func(r *HostRule) { r.Paths = []string{"/a//b"} }), "not in normal form"},
		"encoded slash":   {with(func(r *HostRule) { r.Paths = []string{"/a%2Fb"} }), "encoded"},
		"empty set":       {[]HostRuleSet{{}}, "at least one rule"},
		"too many sets":   {many, "at most 16 rule sets"},
		"too many rules":  {long, "at most 64 rules"},
	} {
		err := ValidateHostRuleSets(tc.sets)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one naming %q", name, err, tc.want)
		}
	}
}

func TestAppendHostRuleSetsKeepsEachSetAndIsOrderFree(t *testing.T) {
	a := []HostRuleSet{{Rules: []HostRule{{Host: "h", Methods: []string{"POST", "GET"}, Effect: HostRuleAllow}}}}
	a2 := []HostRuleSet{{Rules: []HostRule{{Host: "h", Methods: []string{"GET", "POST"}, Effect: HostRuleAllow}}}}
	b := []HostRuleSet{{Rules: []HostRule{{Host: "h", Effect: HostRuleDeny}}}}
	ab, ba := AppendHostRuleSets(a, b), AppendHostRuleSets(b, a2)
	if len(ab) != 2 || HostRuleSetKey(ab[0]) != HostRuleSetKey(ba[0]) || HostRuleSetKey(ab[1]) != HostRuleSetKey(ba[1]) {
		t.Fatalf("append = %+v and %+v, want the same two sets", ab, ba)
	}
	if AppendHostRuleSets(nil, nil) != nil {
		t.Error("no sets should stay nil")
	}
	if !HostRuleSetsWithin(ab, a2) || HostRuleSetsWithin(a, ab) {
		t.Error("within should read sets in normal form, and one set is not within two")
	}
}
