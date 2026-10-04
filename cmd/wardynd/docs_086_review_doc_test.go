// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"regexp"
	"strings"
	"testing"
)

// unreleasedChangelog is the [Unreleased] section: from its heading to the first released one.
func unreleasedChangelog(t *testing.T) string {
	t.Helper()
	doc := readRepo(t, "CHANGELOG.md")
	_, rest, ok := strings.Cut(doc, "## [Unreleased]")
	if !ok {
		t.Fatal("CHANGELOG.md has no [Unreleased] section")
	}
	if i := strings.Index(rest, "\n## ["); i >= 0 {
		rest = rest[:i]
	}
	return rest
}

func wantAll(t *testing.T, where, text string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(text, w) {
			t.Errorf("%s is missing %q", where, w)
		}
	}
}

func wantNone(t *testing.T, where, text string, bans ...string) {
	t.Helper()
	for _, b := range bans {
		if strings.Contains(text, b) {
			t.Errorf("%s still says %q", where, b)
		}
	}
}

// TestUnreleasedNamesTheOperatorFacingAdditions pins that every 0.8.6 switch an operator can trip on
// upgrade is in the [Unreleased] changelog, and that the default-on ones are under "Before you upgrade".
func TestUnreleasedNamesTheOperatorFacingAdditions(t *testing.T) {
	un := unreleasedChangelog(t)
	before, _, _ := strings.Cut(un, "\n### Added")
	wantAll(t, "[Unreleased] Before you upgrade", before,
		"WARDYN_PREFLIGHT_RATE_PER_MIN", "preflight_rate_limited", "WARDYN_RUN_OUTPUT_TAIL_BYTES")
	wantAll(t, "[Unreleased]", un,
		"WARDYN_RUN_MAX_AGE", "run.max_age.expire", "activeDeadlineSeconds",
		"WARDYN_GOVERN_ADMIN_RUNS", "WARDYN_GOVERN_ADMIN_RUNS_EXEMPT", "recording_governed", "user_view_preview",
		"WARDYN_MAX_CONCURRENT_RUNS", "WARDYN_SANDBOX_REQUEST_RATIO", "WARDYN_KEK_REQUIRED",
		"WARDYN_AZURE_KEK_KEY_PLATFORM", "WARDYN_AZURE_KEK_SIGNING_KEY_PLATFORM", "WARDYN_AZURE_CLIENT_ID_PLATFORM",
		"/admin/runs/capacity", "wardyn_runs_active", "metrics.serviceMonitor")
}

// TestUnreleasedNamesTheTerminalChanges pins the day-one terminal behaviour changes.
func TestUnreleasedNamesTheTerminalChanges(t *testing.T) {
	wantAll(t, "[Unreleased]", unreleasedChangelog(t),
		"tmux selection", "Shift", "right-click", "device-login", "Terminal renderer",
		"stale_writer", "session.takeover", "session.attach")
}

// TestAzureFoundryIsDocumentedAsSwitchedOff pins the doc spots to the refusal the server answers.
func TestAzureFoundryIsDocumentedAsSwitchedOff(t *testing.T) {
	src := readRepo(t, "internal/api/model_providers_azure.go")
	if !regexp.MustCompile(`(?m)^var azureFoundryGateReady = false`).MatchString(src) {
		t.Skip("the azure_foundry kind is switched on: revisit these doc notes, not the guard")
	}
	m := regexp.MustCompile(`mp400AzureGate\s*=\s*"model_providers: %q: ([^"]+)"`).FindStringSubmatch(src)
	if m == nil {
		t.Fatal("mp400AzureGate moved: the guard asserts nothing")
	}
	refusal := m[1]
	integ := readRepo(t, "docs/operations/integrations.md")
	_, section, ok := strings.Cut(integ, "## Azure Foundry (`azure_foundry`)")
	if !ok {
		t.Fatal("the Azure Foundry section moved: the guard asserts nothing")
	}
	wantAll(t, "docs/operations/integrations.md Azure Foundry section", section[:min(len(section), 800)], refusal, "Switched off in 0.8.6")
	row := regexp.MustCompile("(?m)^\\| `azure_foundry` \\|.*$").FindString(integ)
	wantAll(t, "docs/operations/integrations.md azure_foundry kind row", row, refusal)
	env := readRepo(t, "docs/ENV.md")
	codex := regexp.MustCompile("(?m)^\\| `WARDYN_CODEX_MODEL` \\|.*$").FindString(env)
	wantAll(t, "docs/ENV.md WARDYN_CODEX_MODEL row", codex, refusal)
}

// TestAgentThreatModelNamesTheRunAgeCap pins row 14 to the absolute run-age cap 0.8.6 added.
func TestAgentThreatModelNamesTheRunAgeCap(t *testing.T) {
	row := regexp.MustCompile(`(?m)^\| 14 \|.*$`).FindString(readRepo(t, "threatmodel/AGENT-THREAT-MODEL.md"))
	if row == "" {
		t.Fatal("row 14 moved: the guard asserts nothing")
	}
	wantAll(t, "threat-model row 14", row, "WARDYN_RUN_MAX_AGE", "no token-spend or model-call budget")
	wantNone(t, "threat-model row 14", row, "no wall-clock lifetime bound anywhere")
}

// TestNoDowngradePathIsDocumented pins the two rollback paragraphs to the boot refusal in internal/db.
func TestNoDowngradePathIsDocumented(t *testing.T) {
	ops := readRepo(t, "docs/OPERATIONS.md")
	wantAll(t, "docs/OPERATIONS.md", ops,
		"There is no downgrade: a 0.8.5 binary refuses",
		"refuses to boot, naming the newest one")
	wantNone(t, "docs/OPERATIONS.md", ops,
		"before a downgrade convert every composed profile to standalone",
		"**it boots anyway**",
		"nothing there refuses a schema newer than the binary")
}

// TestKeyDomainAssignmentsAreListedAsFourEyesCovered pins every page that lists the covered set.
func TestKeyDomainAssignmentsAreListedAsFourEyesCovered(t *testing.T) {
	ops := readRepo(t, "docs/OPERATIONS.md")
	wantAll(t, "docs/OPERATIONS.md what-is-held table", ops, "| Key-domain assignment set, delete | always | security admin or super admin |")
	env := readRepo(t, "docs/ENV.md")
	wantAll(t, "docs/ENV.md WARDYN_GOVERNANCE_SECOND_HUMAN row",
		regexp.MustCompile("(?m)^\\| `WARDYN_GOVERNANCE_SECOND_HUMAN` \\|.*$").FindString(env), "key-domain assignment")
	if n := strings.Count(unreleasedChangelog(t), "key-domain assignment"); n < 2 {
		t.Errorf("[Unreleased] names a key-domain assignment as four-eyes covered %d times, want both four-eyes bullets", n)
	}
	wantAll(t, "docs/USERS.md approver table", readRepo(t, "docs/USERS.md"), "key-domain assignment")
	wantAll(t, "threatmodel/THREAT-MODEL.md four-eyes guarantees", readRepo(t, "threatmodel/THREAT-MODEL.md"), "user-type or key-domain API;")
}

// TestNoPlanLabelsInShippedDocs pins that plan-internal decision and lane ids stay out of shipped docs.
func TestNoPlanLabelsInShippedDocs(t *testing.T) {
	label := regexp.MustCompile(`\bQ-[A-Z]+[0-9]+\b|\bha-l2\.[0-9]\b`)
	for _, f := range []string{"CHANGELOG.md", "docs/OPERATIONS.md", "docs/AUDIT-ACTIONS.md", "docs/sdk.md"} {
		for i, line := range strings.Split(readRepo(t, f), "\n") {
			if m := label.FindString(line); m != "" {
				t.Errorf("%s:%d carries the plan label %q, which no reader can resolve", f, i+1, m)
			}
		}
	}
}

// TestConnectionBudgetCountsTheListeners pins the per-replica connection budget to the two LISTEN
// connections wardynd dials outside the pool.
func TestConnectionBudgetCountsTheListeners(t *testing.T) {
	wantAll(t, "docs/ENV.md WARDYN_PG_DSN row", readRepo(t, "docs/ENV.md"),
		"`pool_max_conns` plus 8 lock connections plus 2 listener connections per replica")
	wantAll(t, "[Unreleased]", unreleasedChangelog(t), "two more database connections", "LISTEN wardyn_mask")
}
