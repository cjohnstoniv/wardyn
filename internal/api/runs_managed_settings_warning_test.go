// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The 201's managed-settings warning reads the harness set and the attended
// tool rules dispatch reads (agentPolicyDocument), so it is neither missing for
// a secondary Claude Code nor spurious for a run dispatch refuses.
func TestManagedSettingsWarning_ReadsTheDocumentDispatchWrites(t *testing.T) {
	images := map[string]string{"claude-code": "wardyn/agent-claude-code:local", "codex-cli": "wardyn/agent-codex-cli:local", "claude-code+codex-cli": setImage}
	const undelivered = "managed settings for autonomy level"
	codexThenClaude := `"experience":"interactive","tools":[` + codexTool + `,` + claudeTool + `],"startup":{"kind":"harness","tool":"codex-cli","prompt":"go"},` + noRepos
	attended := `"experience":"interactive","tools":[{"id":"claude-code","kind":"harness","tool_rules":[{"tool":"Bash","effect":"deny"}]}],` + noRepos
	for _, tc := range []struct {
		name    string
		level   types.AutonomyLevel
		body    string
		capable bool
		want    string
		absent  string
	}{
		{"a secondary Claude Code names itself", types.AutonomyL2, codexThenClaude, false, "Agent claude-code's " + undelivered + " L2 are not delivered", ""},
		{"a capable runner owes the secondary nothing", types.AutonomyL2, codexThenClaude, true, "", "are not delivered"},
		{"a run dispatch refuses over its tool rules is not told it runs on flags alone", types.AutonomyL1, attended, false, "", "launch flags alone"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := govProfile("managed-settings-warning")
			p.Limits = types.GovernanceLimits{AutonomyRubric: autonomyRubric(tc.level)}
			srv, _, _ := harnessSetFixture(t, autonomyCapStore(p), images, nil)
			srv.cfg.Runner = &fakeRunner{noManagedFiles: !tc.capable}
			w := harnessSetCall{"sub-managed", tc.body}.ask(t, srv, harnessDoors[0])
			if w.Code != http.StatusCreated {
				t.Fatalf("create = %d, want 201: %s", w.Code, w.Body.String())
			}
			var resp createRunResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode create: %v", err)
			}
			all := strings.Join(resp.Warnings, "\n")
			if tc.want != "" && !strings.Contains(all, tc.want) {
				t.Errorf("the 201 does not say %q; warnings were:\n%s", tc.want, all)
			}
			if tc.absent != "" && strings.Contains(all, tc.absent) {
				t.Errorf("the 201 says %q and should not; warnings were:\n%s", tc.absent, all)
			}
		})
	}
}

// The gap is the bound runner's, never the deployment executor's; a run that
// waits answers only when every runner it could land on does.
func TestManagedSettingsWarning_ReadsTheBoundRunner(t *testing.T) {
	caps := func(managed bool, resolved string) *runnerwire.Caps {
		c := laptopCaps(types.CC2)
		c.Support.ManagedFiles = managed
		if resolved != "" {
			c.Support.Resolved = map[types.ConfinementClass]string{types.CC2: resolved}
		}
		return c
	}
	deployment := func(noManaged bool, resolved string) *fakeRunner {
		r := &fakeRunner{noManagedFiles: noManaged}
		if resolved != "" {
			r.capsResolved = map[types.ConfinementClass]string{types.CC2: resolved}
		}
		return r
	}
	f := managedSettingsRun{class: types.CC2}
	for _, tc := range []struct {
		name   string
		deploy *fakeRunner
		runner *runnerwire.Caps
		want   string
	}{
		{"a runner that delivers, on a deployment that does not", deployment(true, ""), caps(true, ""), ""},
		{"a runner that does not, on a deployment that does", deployment(false, ""), caps(false, ""), "does not deliver managed files"},
		{"a krun runner, on a deployment that is not", deployment(false, ""), caps(true, "oci/krun"), krunManagedFilesUnverified},
		{"a plain runner, on a krun deployment", deployment(false, "oci/krun"), caps(true, ""), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := capsServer(tc.deploy)
			id := installRunner(t, s, true, tc.runner)
			got, ok := s.placementManagedFilesGap(capsCtx, f, &types.AgentRun{RunnerID: &id})
			if !ok || (tc.want == "" && got != "") || (tc.want != "" && !strings.Contains(got, tc.want)) {
				t.Errorf("gap = %q, %v; want %q", got, ok, tc.want)
			}
		})
	}

	t.Run("a waiting run answers only when its runners agree", func(t *testing.T) {
		s := capsServer(deployment(false, ""))
		a, b := installRunner(t, s, true, caps(false, "")), installRunner(t, s, true, caps(false, ""))
		w := managedSettingsRun{class: types.CC2, place: runPlacement{pooled: true, eligible: []poolTarget{
			{id: a.String(), runnerID: &a, facts: runner.TargetFacts{Classes: []types.ConfinementClass{types.CC2}}},
			{id: b.String(), runnerID: &b, facts: runner.TargetFacts{Classes: []types.ConfinementClass{types.CC2}}},
		}}}
		if got, ok := s.placementManagedFilesGap(capsCtx, w, nil); !ok || !strings.Contains(got, "does not deliver managed files") {
			t.Errorf("agreeing runners: gap = %q, %v; want the shared gap", got, ok)
		}
		c := installRunner(t, s, true, caps(true, ""))
		w.place.eligible = append(w.place.eligible, poolTarget{id: c.String(), runnerID: &c, facts: runner.TargetFacts{Classes: []types.ConfinementClass{types.CC2}}})
		if got, ok := s.placementManagedFilesGap(capsCtx, w, nil); ok {
			t.Errorf("disagreeing runners: gap = %q, want none", got)
		}
	})
}
