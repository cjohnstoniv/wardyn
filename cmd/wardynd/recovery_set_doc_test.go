// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"regexp"
	"strings"
	"testing"
)

// TestRecoverySetByDeploymentDocumented: an operator who backs up Wardyn must
// collect five things (database, age identity and keys, recordings, drives,
// audit spool with its sidecars), and where each lives differs per deployment.
// docs/OPERATIONS.md carries one table for Compose, the managed desktop and
// Helm; the three deployment pages must link to it, and the section must not
// inline a key, DSN or token.
func TestRecoverySetByDeploymentDocumented(t *testing.T) {
	ops := readOpsDoc(t, "docs", "OPERATIONS.md")
	_, sec, found := strings.Cut(ops, "\n### Recovery set by deployment\n")
	if !found {
		t.Fatal("docs/OPERATIONS.md has no '### Recovery set by deployment' section")
	}
	sec, _, _ = strings.Cut(sec, "\n### ")

	// Each row must name all five items, in the column that item owns.
	cols := []struct {
		name string
		want []string // every string must appear in the cell
	}{
		{"database", []string{"postgres"}},
		{"age identity and keys", []string{"age_key|age.key|ageKey"}},
		{"recordings", []string{"WARDYN_RECORDING_STORE"}},
		{"drives", []string{"drive"}},
		{"audit spool", []string{"audit-spool.jsonl", ".consumed", ".quarantine"}},
	}
	rows := map[string][]string{}
	for _, line := range strings.Split(sec, "\n") {
		if !strings.HasPrefix(line, "| **") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "| "), " | ")
		rows[strings.Trim(cells[0], "*")] = cells[1:]
	}
	for _, dep := range []string{"Compose", "Managed desktop", "Helm"} {
		cells, ok := rows[dep]
		if !ok {
			t.Errorf("recovery-set table has no %q row", dep)
			continue
		}
		if len(cells) != len(cols) {
			t.Errorf("%s row has %d item cells, want %d", dep, len(cells), len(cols))
			continue
		}
		for i, c := range cols {
			for _, w := range c.want {
				found := false
				for _, alt := range strings.Split(w, "|") {
					found = found || strings.Contains(strings.ToLower(cells[i]), strings.ToLower(alt))
				}
				if !found {
					t.Errorf("%s row, %s cell never mentions %q", dep, c.name, w)
				}
			}
		}
	}

	// Honest limits: not rehearsed, and the desktop's age.key stays out.
	for _, want := range []string{"No shipped tool rehearses a restore", "not in the set, deliberately", "wardyn.drive=<id>", "fresh enrolment token"} {
		if !strings.Contains(strings.ToLower(sec), strings.ToLower(want)) {
			t.Errorf("recovery-set section never says %q", want)
		}
	}

	// No key material, DSN or token inline.
	for _, bad := range []*regexp.Regexp{
		regexp.MustCompile(`AGE-SECRET-KEY-`),
		regexp.MustCompile(`age1[0-9a-z]{20,}`),
		regexp.MustCompile(`postgres(ql)?://[^\s]*@`),
		regexp.MustCompile(`(?i)bearer [a-z0-9._-]{16,}`),
	} {
		if m := bad.FindString(sec); m != "" {
			t.Errorf("recovery-set section inlines secret-shaped material %q", m)
		}
	}

	// Each deployment's own page links to it.
	for _, tc := range []struct {
		name string
		path []string
		want string
	}{
		{"docs/OPERATIONS.md 'Back them up'", []string{"docs", "OPERATIONS.md"}, "(#recovery-set-by-deployment)"},
		{"docs/DESKTOP.md", []string{"docs", "DESKTOP.md"}, "OPERATIONS.md#recovery-set-by-deployment"},
		{"deploy/helm/wardyn/README.md", []string{"deploy", "helm", "wardyn", "README.md"}, "OPERATIONS.md#recovery-set-by-deployment"},
	} {
		if !strings.Contains(readOpsDoc(t, tc.path...), tc.want) {
			t.Errorf("%s does not link to the recovery-set section (%s)", tc.name, tc.want)
		}
	}
}
