// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestWardynLabels_ReservedKeysWinOverExtra is the M3 regression test: extra
// (caller-supplied) must never override wardyn.component, wardyn.run-id, or
// wardyn.managed — an override on wardyn.component would un-select the
// agent from its own NetworkPolicy (built from this same label map).
func TestWardynLabels_ReservedKeysWinOverExtra(t *testing.T) {
	runID := uuid.New()
	l := wardynLabels(runID, componentAgent, map[string]string{
		labelComponent: componentProxy, // attempted override
		labelRun:       "attacker-controlled",
		labelManaged:   "false",
		"team":         "sre",
	})
	if l[labelComponent] != componentAgent {
		t.Errorf("labelComponent = %q, want %q (reserved, must win over extra)", l[labelComponent], componentAgent)
	}
	if l[labelRun] != runID.String() {
		t.Errorf("labelRun = %q, want %q (reserved, must win over extra)", l[labelRun], runID.String())
	}
	if l[labelManaged] != "true" {
		t.Errorf("labelManaged = %q, want \"true\" (reserved, must win over extra)", l[labelManaged])
	}
	if l["team"] != "sre" {
		t.Errorf("team = %q, want \"sre\" (a non-reserved extra key must still pass through)", l["team"])
	}
}

// TestWardynLabels_SanitizesOrOmitsExtraValues is the L4 regression test: a
// free-form extra value (e.g. wardyn.agent from a run's Agent field) that
// isn't legal k8s label syntax must never reach the apiserver as-is — that
// would 422 the WHOLE object create, taking the run-id/component/managed
// labels down with it. Sanitize when possible; omit when not.
func TestWardynLabels_SanitizesOrOmitsExtraValues(t *testing.T) {
	runID := uuid.New()
	l := wardynLabels(runID, componentAgent, map[string]string{
		"wardyn.agent": "Claude Code (Sonnet 5)!",
		"empty":        "",
		"toolong":      strings.Repeat("x", 100),
		"legal":        "already-fine_1.0",
	})
	// "Claude Code (Sonnet 5)!" -> "Claude-Code--Sonnet-5--" before trimming;
	// strings.Trim removes ALL trailing separator runs, not just one, so the
	// trailing "!" and ")" (both illegal, both -> '-') are stripped entirely.
	if got := l["wardyn.agent"]; got != "Claude-Code--Sonnet-5" {
		t.Errorf("wardyn.agent = %q, want illegal characters replaced with '-' and no leading/trailing separator", got)
	}
	if _, ok := l["empty"]; ok {
		t.Errorf("empty extra value must be OMITTED, got %q", l["empty"])
	}
	if got := l["toolong"]; len(got) > 63 {
		t.Errorf("toolong = %q (%d chars), want <=63", got, len(got))
	}
	if l["legal"] != "already-fine_1.0" {
		t.Errorf("legal = %q, want unchanged (already legal)", l["legal"])
	}
	// The reserved labels must still be present and correct regardless.
	if l[labelRun] != runID.String() || l[labelComponent] != componentAgent || l[labelManaged] != "true" {
		t.Errorf("reserved labels corrupted by sanitization: %v", l)
	}
}

func TestSanitizeLabelValue(t *testing.T) {
	cases := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{"already-legal_1.0", "already-legal_1.0", true},
		{"has spaces", "has-spaces", true},
		{"emoji \U0001F600 name", "emoji---name", true},
		{"-leading-dash", "leading-dash", true},
		{"trailing-dash-", "trailing-dash", true},
		{"...", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		got, ok := sanitizeLabelValue(tc.in)
		if got != tc.want || ok != tc.wantOK {
			t.Errorf("sanitizeLabelValue(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.wantOK)
		}
	}
}
