// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestApplyDispatchModeEnv_BootSeed is Part A1's dispatch-env contract, tested
// directly against applyDispatchModeEnv (same direct-call style as
// TestBrokeredRunWithholdsSSHGrantEnv/GitPATGrantEnv in sshkey_test.go /
// gitpat_test.go) rather than the full HTTP+DB round trip: the branching this
// pins is pure env-map logic, so a table test here is exhaustive without a
// Postgres dependency. An interactive run's Task rides the sandbox env as
// WARDYN_INTERACTIVE_SEED — gated on `interactive` exactly the way
// WARDYN_INTERACTIVE_START is (TestDispatch_InteractiveStart_SetsSandboxEnv,
// task_mode_test.go): structurally absent for a batch run no matter what the
// request said.
func TestApplyDispatchModeEnv_BootSeed(t *testing.T) {
	cases := []struct {
		name          string
		interactive   bool
		task          string
		seedAutoTools bool
		wantSeed      string // "" means absent
		wantAutoTools bool
	}{
		{"interactive with a seed sets it", true, "review the failing tests", false, "review the failing tests", false},
		{"interactive with no seed stays idle (today's behavior)", true, "", false, "", false},
		{"batch NEVER seeds, even carrying a task", false, "review the failing tests", false, "", false},
		{"seed_auto_tools follows the bool", true, "review the failing tests", true, "review the failing tests", true},
		{"seed_auto_tools never fires with no seed", true, "", true, "", false},
		{"seed_auto_tools never fires on batch", false, "review the failing tests", true, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			run := types.AgentRun{ID: uuid.New(), Task: c.task}
			env := map[string]string{}
			applyDispatchModeEnv(env, run, c.interactive, "", "", c.seedAutoTools, "", nil, nil, nil, nil)
			if got := env["WARDYN_INTERACTIVE_SEED"]; got != c.wantSeed {
				t.Errorf("Env[WARDYN_INTERACTIVE_SEED] = %q, want %q", got, c.wantSeed)
			}
			if _, ok := env["WARDYN_SEED_AUTO_TOOLS"]; ok != c.wantAutoTools {
				t.Errorf("Env[WARDYN_SEED_AUTO_TOOLS] present = %v, want %v", ok, c.wantAutoTools)
			}
		})
	}
}

// TestApplyDispatchModeEnv_BootSeed_ReservedTasksExcluded is the load-bearing
// half of the seed gate: a server-launched record/verify/login run is an
// INTERACTIVE run whose Task is one of the server-set discriminators
// reservedRunTasks guards elsewhere (runs_create_validate.go, same package) —
// without this exclusion, seeding would boot `claude "workspace record"` into
// what is supposed to be a plain record-mode sandbox, or boot-seed over the
// login sandbox's own login flow. Iterates the real map rather than
// hardcoding the three strings, so this test cannot go stale if a fourth
// reserved task is ever added.
func TestApplyDispatchModeEnv_BootSeed_ReservedTasksExcluded(t *testing.T) {
	for task := range reservedRunTasks {
		t.Run(task, func(t *testing.T) {
			run := types.AgentRun{ID: uuid.New(), Task: task}
			env := map[string]string{}
			applyDispatchModeEnv(env, run, true /* interactive */, "", "", true /* seedAutoTools */, "", nil, nil, nil, nil)
			if v, ok := env["WARDYN_INTERACTIVE_SEED"]; ok {
				t.Errorf("Env[WARDYN_INTERACTIVE_SEED] = %q on reserved task %q, want absent", v, task)
			}
			if v, ok := env["WARDYN_SEED_AUTO_TOOLS"]; ok {
				t.Errorf("Env[WARDYN_SEED_AUTO_TOOLS] = %q on reserved task %q, want absent", v, task)
			}
		})
	}
}

// TestApplyDispatchModeEnv_ToolApprovals is Part C1's dispatch-env contract:
// "hold" rides the sandbox env as WARDYN_TOOL_APPROVALS for an AUTONOMOUS run
// only. Interactive runs have their own supervised-seed posture (the boot-seed
// tests above), so this can never ride one no matter what the request said —
// the mirror image of the seed gate's own interactive/!interactive split.
func TestApplyDispatchModeEnv_ToolApprovals(t *testing.T) {
	cases := []struct {
		name          string
		interactive   bool
		toolApprovals string
		want          string // "" means absent
	}{
		{"batch + hold sets it", false, "hold", "hold"},
		{"batch + auto stays absent (never emit the wire default)", false, "auto", ""},
		{"batch + empty stays absent", false, "", ""},
		{"interactive + hold NEVER rides (structural gate)", true, "hold", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			run := types.AgentRun{ID: uuid.New()}
			env := map[string]string{}
			applyDispatchModeEnv(env, run, c.interactive, "", "", false, c.toolApprovals, nil, nil, nil, nil)
			if got := env["WARDYN_TOOL_APPROVALS"]; got != c.want {
				t.Errorf("Env[WARDYN_TOOL_APPROVALS] = %q, want %q", got, c.want)
			}
		})
	}
}

// TestCreateRun_ToolApprovals_Validation covers C1's closed-enum + codex-cli
// rejection at the HTTP layer (runs_create_validate.go) — same shape as
// TestCreateRun_UnknownTaskModeIs400 (task_mode_test.go). The two 400 cases
// fail before any store write, so the cheap newHarness (no Postgres) suffices;
// the acceptance case actually dispatches, so it needs pgHarnessWithRunner
// (WARDYN_TEST_PG-gated — skipped cleanly when unset, same as every other
// dispatch test in this package).
func TestCreateRun_ToolApprovals_Validation(t *testing.T) {
	t.Run("unknown value is 400", func(t *testing.T) {
		h := newHarness(t)
		w := do(t, h.srv, http.MethodPost, "/api/v1/runs", adminToken,
			`{"agent":"claude-code","task":"echo hi","tool_approvals":"yolo"}`)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for unknown tool_approvals, got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("hold is refused for codex-cli", func(t *testing.T) {
		h := newHarness(t)
		w := do(t, h.srv, http.MethodPost, "/api/v1/runs", adminToken,
			`{"agent":"codex-cli","task":"echo hi","tool_approvals":"hold"}`)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for tool_approvals=hold on codex-cli, got %d: %s", w.Code, w.Body.String())
		}
	})

	// C.3: the field used to be ACCEPTED AND SILENTLY DISCARDED on an
	// interactive run — applyDispatchModeEnv writes WARDYN_TOOL_APPROVALS only
	// when !interactive, so the caller got a 201 and none of the supervision they
	// asked for. A field accepted and thrown away is worse than one refused: the
	// caller believes the run is gated.
	t.Run("hold is refused for an interactive run", func(t *testing.T) {
		h := newHarness(t)
		w := do(t, h.srv, http.MethodPost, "/api/v1/runs", adminToken,
			`{"agent":"claude-code","task":"echo hi","interactive":true,"tool_approvals":"hold"}`)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for tool_approvals=hold on an interactive run, got %d: %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "tool_approvals") {
			t.Errorf("the 400 must NAME the field the caller got wrong, got: %s", w.Body.String())
		}
	})

	// The same request with NO task, which runs_create_validate.go COERCES to
	// interactive. A guard sited before that coercion passes here and the field
	// is still dropped — the identical hole, one step later. This is why the
	// guard runs after it.
	t.Run("hold is refused for a run coerced to interactive by having no task", func(t *testing.T) {
		h := newHarness(t)
		w := do(t, h.srv, http.MethodPost, "/api/v1/runs", adminToken,
			`{"agent":"claude-code","tool_approvals":"hold"}`)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400: an empty task is coerced to interactive, where hold is inert. got %d: %s", w.Code, w.Body.String())
		}
	})

	t.Run("hold is accepted for claude-code and reaches the sandbox env", func(t *testing.T) {
		fr := &fakeRunner{}
		srv, _ := pgHarnessWithRunner(t, fr)
		w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken,
			`{"agent":"claude-code","task":"echo hi","tool_approvals":"hold"}`)
		if w.Code != http.StatusCreated {
			t.Fatalf("expected 201 for tool_approvals=hold on claude-code, got %d: %s", w.Code, w.Body.String())
		}
		if got := fr.lastSpec.Env["WARDYN_TOOL_APPROVALS"]; got != "hold" {
			t.Errorf("Env[WARDYN_TOOL_APPROVALS] = %q, want hold", got)
		}
	})
}
