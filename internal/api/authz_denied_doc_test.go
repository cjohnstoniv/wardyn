// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// authzDeniedReasons is the audited `authz.denied` reason vocabulary — the enum
// docs/AUDIT-ACTIONS.md calls CLOSED and docs/OPERATIONS.md calls itself the
// source of record for. Three artifacts have to agree: this list, the emit sites
// in this package, and those two documents. The test below checks all three
// pairings, so adding a reason and telling nobody fails here instead of at
// someone's SIEM.
var authzDeniedReasons = []string{
	"admin_surface",
	"attach_ticket_foreign_run",
	"byoi_member",
	"capability_" + capAgent,
	"capability_" + capEgressHost,
	"capability_" + capIntegration,
	"capability_" + capSecret,
	"capability_" + capWorkspace,
	"governance_profile",
	"grant_pairing_not_eligible",
	"not_owner",
	"second_human_required",
	"security_admin_surface",
}

// How a reason is written at an emit site. The first two are read only INSIDE a
// window that starts at an "authz.denied" literal, because `reason` is a field
// several OTHER actions carry too (approval.second_human.bypass's
// admin_token_break_glass, source.scan's no_facts_uploaded). The rest are the
// helpers that feed authz.denied and nothing else: denyMemberField writes the
// event, denyMemberCapability delegates to it, and a capDrop is what
// inline_policy's per-reason authz.denied loop iterates.
var (
	reasonInWindow    = regexp.MustCompile(`"reason":\s*"([a-z_]+)"(\s*\+)?`)
	reasonCapInWindow = regexp.MustCompile(`"reason":\s*"capability_"\s*\+\s*(cap[A-Za-z]+)`)
	reasonHelperKinds = []*regexp.Regexp{
		regexp.MustCompile(`capDrop\{reason:\s*"capability_"\s*\+\s*(cap[A-Za-z]+)`),
		regexp.MustCompile(`denyMemberCapability\(w, r, (cap[A-Za-z]+),`),
	}
	reasonHelperLiterals = []*regexp.Regexp{
		regexp.MustCompile(`capDrop\{reason:\s*"([a-z_]+)"(\s*\+)?`),
		regexp.MustCompile(`denyMemberField\(w, r, [^,]+, "([a-z_]+)"(\s*\+)?`),
	}
)

// authzDeniedWindow is how far past an "authz.denied" literal its reason field
// is looked for: the emit is a single auditEvent call, never longer than this.
const authzDeniedWindow = 400

// capKindValue resolves a cap* identifier to the string it holds, so an emit
// site written as "capability_" + capAgent compares as "capability_agent".
var capKindValue = map[string]string{
	"capEgressHost":  capEgressHost,
	"capSecret":      capSecret,
	"capWorkspace":   capWorkspace,
	"capImage":       capImage,
	"capAgent":       capAgent,
	"capIntegration": capIntegration,
}

// TestAuthzDeniedReasonsAreDocumented pins the CLOSED reason enum to the code
// that emits it and to the two documents that publish it.
//
// docs/AUDIT-ACTIONS.md marks authz.denied "stable (documented, closed `reason`
// enum)" and docs/OPERATIONS.md's table declares itself the source of record,
// whose reason field "is the whole vocabulary" — a promise SIEM rules are
// written against. 0.7 added two reasons and neither document learned about
// either: security_admin_surface (the second admin tier's own refusal, kept
// separate from admin_surface precisely so an auditor can tell the tiers apart)
// and attach_ticket_foreign_run (a foreign PTY ticket, refused even to a
// security admin, and deliberately not filed under not_owner). A rule keyed on
// the published eleven matched neither.
func TestAuthzDeniedReasonsAreDocumented(t *testing.T) {
	want := slices.Sorted(slices.Values(authzDeniedReasons))

	if got := emittedAuthzDeniedReasons(t); !slices.Equal(got, want) {
		t.Errorf("the reasons this package emits are\n  %v\nbut authzDeniedReasons says\n  %v\n"+
			"update the list, docs/OPERATIONS.md's table and docs/AUDIT-ACTIONS.md's row together — "+
			"or, if an emit site changed shape, teach the scanner about it", got, want)
	}
	if got := documentedAuthzDeniedReasons(t); !slices.Equal(got, want) {
		t.Errorf("docs/OPERATIONS.md's reason table lists\n  %v\nbut the vocabulary is\n  %v\n"+
			"that table calls itself the source of record for a CLOSED enum", got, want)
	}
	actions, err := os.ReadFile("../../docs/AUDIT-ACTIONS.md")
	if err != nil {
		t.Fatalf("read docs/AUDIT-ACTIONS.md: %v", err)
	}
	row := rowFor(t, string(actions), "| `authz.denied` |")
	for _, reason := range want {
		if !strings.Contains(row, "`"+reason+"`") {
			t.Errorf("docs/AUDIT-ACTIONS.md's authz.denied row omits %q from its inline vocabulary", reason)
		}
	}
}

// emittedAuthzDeniedReasons scans this package's non-test sources for every
// reason literal an authz.denied event can carry.
func emittedAuthzDeniedReasons(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	seen := map[string]bool{}
	addKind := func(ident string) {
		v, ok := capKindValue[ident]
		if !ok {
			t.Errorf("emit site names capability kind %q, which capKindValue does not resolve", ident)
			return
		}
		seen["capability_"+v] = true
	}
	sawEmit := false
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		src := string(b)
		for i := 0; ; {
			j := strings.Index(src[i:], `"authz.denied"`)
			if j < 0 {
				break
			}
			sawEmit = true
			start := i + j
			end := min(start+authzDeniedWindow, len(src))
			window := src[start:end]
			for _, m := range reasonCapInWindow.FindAllStringSubmatch(window, -1) {
				addKind(m[1])
			}
			for _, m := range reasonInWindow.FindAllStringSubmatch(window, -1) {
				if m[2] != "" {
					continue // "capability_" + capX — handled above
				}
				seen[m[1]] = true
			}
			i = start + len(`"authz.denied"`)
		}
		for _, re := range reasonHelperKinds {
			for _, m := range re.FindAllStringSubmatch(src, -1) {
				addKind(m[1])
			}
		}
		for _, re := range reasonHelperLiterals {
			for _, m := range re.FindAllStringSubmatch(src, -1) {
				if m[2] != "" {
					continue // "capability_" + capX — the kind regexes have it
				}
				seen[m[1]] = true
			}
		}
	}
	if !sawEmit {
		t.Fatal(`no "authz.denied" emit site found — the scanner, not the vocabulary, is what changed`)
	}
	out := make([]string, 0, len(seen))
	for r := range seen {
		out = append(out, r)
	}
	slices.Sort(out)
	return out
}

// documentedAuthzDeniedReasons returns the `reason` column of OPERATIONS.md's
// "Every denial that isn't a 404" table (body rows only).
func documentedAuthzDeniedReasons(t *testing.T) []string {
	t.Helper()
	doc := operationsDoc(t)
	i := strings.Index(doc, "### Every denial that isn't a 404")
	if i < 0 {
		t.Fatal(`docs/OPERATIONS.md has no "Every denial that isn't a 404" section`)
	}
	key := regexp.MustCompile("^\\| `([a-z_]+)` \\|")
	var out []string
	inTable, pastHeader := false, false
	for _, line := range strings.Split(doc[i:], "\n") {
		if !strings.HasPrefix(line, "|") {
			if inTable {
				break
			}
			continue
		}
		inTable = true
		if strings.HasPrefix(line, "|---") {
			pastHeader = true
			continue
		}
		if !pastHeader {
			continue // the header row's own `reason` cell is not a value
		}
		if m := key.FindStringSubmatch(line); m != nil {
			out = append(out, m[1])
		}
	}
	if len(out) == 0 {
		t.Fatal("no reason rows parsed from the denial table")
	}
	slices.Sort(out)
	return out
}

// rowFor returns the markdown table row starting with prefix.
func rowFor(t *testing.T, doc, prefix string) string {
	t.Helper()
	for _, line := range strings.Split(doc, "\n") {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	t.Fatalf("no table row starting %q", prefix)
	return ""
}
