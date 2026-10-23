// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/agentpolicy"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestInnerWalls_CreateDoor is harness_inner_walls through POST /runs: the walls a run launches
// with are on its row and its 201, approvals in Wardyn put a background Claude Code run on the hold
// lane, a looser choice is refused with its reason, and a harness sandbox mode stores and binds a
// Codex run where its class starts it, and is refused elsewhere.
func TestInnerWalls_CreateDoor(t *testing.T) {
	const run = `{"agent":"claude-code","repo":"acme/widgets","task":"do the thing"`
	create := func(t *testing.T, body string) (*fakeRunner, int, string) {
		t.Helper()
		fr := &fakeRunner{}
		srv, _ := pgHarnessWithRunner(t, fr)
		w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken, body)
		return fr, w.Code, w.Body.String()
	}
	walls := func(t *testing.T, body string) *types.HarnessInnerWalls {
		t.Helper()
		var got createRunResponse
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatal(err)
		}
		return got.InnerWalls
	}

	t.Run("absent records today's walls", func(t *testing.T) {
		fr, code, body := create(t, run+`}`)
		if code != http.StatusCreated {
			t.Fatalf("code %d: %s", code, body)
		}
		if got := walls(t, body); got == nil || *got != (types.HarnessInnerWalls{SandboxMode: types.InnerSandboxOff, Approvals: types.InnerApprovalsNone}) {
			t.Errorf("inner_walls = %+v, want off/none", got)
		}
		fr.waitForSandbox(t)
		if fr.lastSpec.Env["WARDYN_TOOL_APPROVALS"] != "" {
			t.Errorf("WARDYN_TOOL_APPROVALS = %q with no choice", fr.lastSpec.Env["WARDYN_TOOL_APPROVALS"])
		}
	})
	t.Run("approvals in Wardyn take the hold lane", func(t *testing.T) {
		fr, code, body := create(t, run+`,"inline_policy":{"min_confinement_class":"CC2","harness_inner_walls":{"approvals":"wardyn"}}}`)
		if code != http.StatusCreated {
			t.Fatalf("code %d: %s", code, body)
		}
		if got := walls(t, body); got == nil || got.Approvals != types.InnerApprovalsWardyn {
			t.Errorf("inner_walls = %+v, want approvals wardyn", got)
		}
		if !strings.Contains(body, "harness_inner_walls.approvals: wardyn") {
			t.Errorf("201 does not say the hold was derived: %s", body)
		}
		fr.waitForSandbox(t)
		if fr.lastSpec.Env["WARDYN_TOOL_APPROVALS"] != "hold" {
			t.Errorf("WARDYN_TOOL_APPROVALS = %q, want hold", fr.lastSpec.Env["WARDYN_TOOL_APPROVALS"])
		}
	})
	t.Run("no approvals on a hold run is looser", func(t *testing.T) {
		_, code, body := create(t, run+`,"tool_approvals":"hold","inline_policy":{"min_confinement_class":"CC2","harness_inner_walls":{"approvals":"none"}}}`)
		if code != http.StatusUnprocessableEntity || !strings.Contains(body, reasonHarnessInnerWallsLooser) {
			t.Fatalf("code %d body %s, want 422 %s", code, body, reasonHarnessInnerWallsLooser)
		}
	})
	t.Run("a harness sandbox mode stores at the policy write", func(t *testing.T) {
		srv, _ := pgHarnessWithRunner(t, &fakeRunner{})
		w := do(t, srv, http.MethodPost, "/api/v1/policies", adminToken,
			`{"name":"inner-walls-ro-`+uuid.NewString()+`","spec":{"min_confinement_class":"CC2","harness_inner_walls":{"sandbox_mode":"read-only"}}}`)
		if w.Code >= 300 {
			t.Fatalf("code %d body %s, want it stored", w.Code, w.Body.String())
		}
	})
	t.Run("read-only on a Claude Code run is unmet", func(t *testing.T) {
		_, code, body := create(t, run+`,"inline_policy":{"min_confinement_class":"CC1","harness_inner_walls":{"sandbox_mode":"read-only"}}}`)
		if code != http.StatusUnprocessableEntity || !strings.Contains(body, reasonHarnessInnerWallsUnmet) {
			t.Fatalf("code %d body %s, want 422 %s", code, body, reasonHarnessInnerWallsUnmet)
		}
	})
	const codex = `{"agent":"codex-cli","repo":"acme/widgets","task":"do the thing"`
	createAt := func(t *testing.T, body string) (*fakeRunner, int, string) {
		t.Helper()
		fr := &fakeRunner{capsClasses: []types.ConfinementClass{types.CC1, types.CC2, types.CC3}}
		srv, _ := pgHarnessWithRunner(t, fr)
		w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken, body)
		return fr, w.Code, w.Body.String()
	}
	t.Run("read-only binds a Codex run at CC3", func(t *testing.T) {
		fr, code, body := createAt(t, codex+`,"confinement_class":"CC3","inline_policy":{"min_confinement_class":"CC1","harness_inner_walls":{"sandbox_mode":"read-only"}}}`)
		if code != http.StatusCreated {
			t.Fatalf("code %d: %s", code, body)
		}
		if got := walls(t, body); got == nil || *got != (types.HarnessInnerWalls{SandboxMode: types.InnerSandboxReadOnly, Approvals: types.InnerApprovalsNone}) {
			t.Errorf("inner_walls = %+v, want read-only/none", got)
		}
		fr.waitForSandbox(t)
		if fr.lastSpec.Env["WARDYN_HARNESS_SANDBOX"] != types.InnerSandboxReadOnly || !slices.ContainsFunc(fr.lastSpec.ManagedFiles,
			func(f runner.ManagedFile) bool { return f.Path == agentpolicy.CodexRequirementsPath }) {
			t.Errorf("env %q files %+v, want the read-only launch and its requirements", fr.lastSpec.Env["WARDYN_HARNESS_SANDBOX"], fr.lastSpec.ManagedFiles)
		}
	})
	t.Run("read-only on a Codex run at CC1 is unavailable", func(t *testing.T) {
		_, code, body := createAt(t, codex+`,"confinement_class":"CC1","inline_policy":{"min_confinement_class":"CC1","harness_inner_walls":{"sandbox_mode":"read-only"}}}`)
		if code != http.StatusUnprocessableEntity || !strings.Contains(body, reasonHarnessSandboxUnavailable) {
			t.Fatalf("code %d body %s, want 422 %s", code, body, reasonHarnessSandboxUnavailable)
		}
	})
}
