// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/authz"
)

// TestAuthzDeniedReasonsAreDocumented pins the CLOSED reason enum — the
// audited reasons in internal/authz's registry — to the two documents that
// publish it.
//
// docs/AUDIT-ACTIONS.md marks authz.denied "stable (documented, closed `reason`
// enum)" and docs/OPERATIONS.md's table declares itself the source of record,
// whose reason field "is the whole vocabulary" — a promise SIEM rules are
// written against. The registry is walked rather than the emit sites scanned:
// refuse will not emit a reason the registry lacks, and TestNoAdHocAuthz holds
// every emit to refuse, so a new reason cannot reach a row without landing
// here. (The regex scanner this replaced missed harness_login_not_per_user for
// a whole release: its call split across two lines.)
func TestAuthzDeniedReasonsAreDocumented(t *testing.T) {
	var want []string
	for _, r := range authz.Reasons() {
		if ref, _ := authz.Lookup(r); ref.Audit {
			want = append(want, string(r))
		}
	}

	if got := documentedAuthzDeniedReasons(t); !slices.Equal(got, want) {
		t.Errorf("docs/OPERATIONS.md's reason table lists\n  %v\nbut the registry's audited reasons are\n  %v\n"+
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

// documentedAuthzDeniedReasons returns the `reason` column of OPERATIONS.md's
// "Every denial that isn't a 404" table (body rows only).
func documentedAuthzDeniedReasons(t *testing.T) []string {
	t.Helper()
	doc := operationsDoc(t)
	i := strings.Index(doc, "### Every denial that isn't a 404")
	if i < 0 {
		t.Fatal(`docs/OPERATIONS.md has no "Every denial that isn't a 404" section`)
	}
	key := regexp.MustCompile("^\\| `([a-z0-9_]+)` \\|")
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
