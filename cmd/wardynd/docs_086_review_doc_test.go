// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"regexp"
	"strings"
	"testing"
)

// changelog086 is the CHANGELOG.md [0.8.6] section, read by version so text under [Unreleased] and
// the position of the neighbouring headings never move what these tests pin.
func changelog086(t *testing.T) string {
	t.Helper()
	sec, ok := changelogSections(readRepo(t, "CHANGELOG.md"))["0.8.6"]
	if !ok {
		t.Fatal("CHANGELOG.md has no [0.8.6] section")
	}
	return sec
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
// upgrade is in the [0.8.6] changelog, and that the default-on ones are under "Before you upgrade".
func TestUnreleasedNamesTheOperatorFacingAdditions(t *testing.T) {
	un := changelog086(t)
	_, before, ok := strings.Cut(un, "\n### Before you upgrade")
	if !ok {
		t.Fatal("CHANGELOG.md [0.8.6] has no Before you upgrade subsection")
	}
	before, _, _ = strings.Cut(before, "\n### ")
	wantAll(t, "CHANGELOG.md [0.8.6] Before you upgrade", before,
		"WARDYN_PREFLIGHT_RATE_PER_MIN", "preflight_rate_limited", "WARDYN_RUN_OUTPUT_TAIL_BYTES")
	wantAll(t, "CHANGELOG.md [0.8.6]", un,
		"WARDYN_RUN_MAX_AGE", "run.max_age.expire", "activeDeadlineSeconds",
		"WARDYN_GOVERN_ADMIN_RUNS", "WARDYN_GOVERN_ADMIN_RUNS_EXEMPT", "recording_governed", "user_view_preview",
		"WARDYN_MAX_CONCURRENT_RUNS", "WARDYN_SANDBOX_REQUEST_RATIO", "WARDYN_KEK_REQUIRED",
		"WARDYN_AZURE_KEK_KEY_PLATFORM", "WARDYN_AZURE_KEK_SIGNING_KEY_PLATFORM", "WARDYN_AZURE_CLIENT_ID_PLATFORM",
		"/admin/runs/capacity", "wardyn_runs_active", "metrics.serviceMonitor", "0131_principal_key_handles")
}

// TestUnreleasedNamesTheTerminalChanges pins the day-one terminal behaviour changes.
func TestUnreleasedNamesTheTerminalChanges(t *testing.T) {
	wantAll(t, "CHANGELOG.md [0.8.6]", changelog086(t),
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
	// The changelog's 0.8.6 entries make the same promise: the dump is the only way back.
	cl := strings.Join(strings.Fields(changelog086(t)), " ")
	wantAll(t, "CHANGELOG.md [0.8.6]", cl, "restore the pre-upgrade dump")
	wantNone(t, "CHANGELOG.md [0.8.6]", cl,
		"First turn every composed profile back into a standalone one",
		"A downgrade to 0.8.5 ignores the new tables",
		"a downgrade runs a narrowed policy unnarrowed",
		"`effective.ceiling` and `effective.limits` as the new")
}

// TestKeyDomainAssignmentsAreListedAsFourEyesCovered pins every page that lists the covered set.
func TestKeyDomainAssignmentsAreListedAsFourEyesCovered(t *testing.T) {
	ops := readRepo(t, "docs/OPERATIONS.md")
	wantAll(t, "docs/OPERATIONS.md what-is-held table", ops, "| Key-domain assignment set, delete | always | security admin or super admin |")
	env := readRepo(t, "docs/ENV.md")
	wantAll(t, "docs/ENV.md WARDYN_GOVERNANCE_SECOND_HUMAN row",
		regexp.MustCompile("(?m)^\\| `WARDYN_GOVERNANCE_SECOND_HUMAN` \\|.*$").FindString(env), "key-domain assignment")
	if n := strings.Count(changelog086(t), "key-domain assignment"); n < 2 {
		t.Errorf("[0.8.6] names a key-domain assignment as four-eyes covered %d times, want both four-eyes bullets", n)
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
		"**at least 4**", "`pool_max_conns` plus up to 3 dedicated lifetime connections per replica", "`pool_max_conns` plus 3 dedicated lifetime connections plus 8 lock connections plus 2 listener connections per replica")
	wantAll(t, "CHANGELOG.md [0.8.6]", changelog086(t), "two more database connections", "LISTEN wardyn_mask")
}
