// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package live

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLive_TierMatrix is the honest tier-coverage gate. For every confinement
// class the corpus declares, it either RUNS the class (when the host installs its
// runtime) or asserts the control plane FAILS CLOSED (422) for it — no silent
// skip, no green that reads as "confined" when only the scheduler was exercised.
func TestLive_TierMatrix(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	installed := h.installedClasses(ctx)
	t.Logf("installed confinement classes: %v", keys(installed))

	for _, class := range []string{"CC1", "CC2", "CC3"} {
		class := class
		t.Run(class, func(t *testing.T) {
			if installed[class] {
				// Accurate everywhere: scheduling accepts this class. TestLive_Tasks
				// currently exercises the corpus at CC1; a host that installs CC2/CC3
				// runs the real-model lane at its best class (bestInstalledClass).
				t.Logf("%s runtime is installed — scheduling accepts it; the task sub-tests exercise CC1 (real-model lane uses the host's best class)", class)
				return
			}
			// Not installed: the ONLY honest claim is that scheduling fails closed.
			hh := &harness{t: t, base: h.base, token: h.token, sdk: h.sdk, http: h.http}
			hh.expectFailClosed(class)
			t.Logf("%s runtime NOT installed on this host: scheduling correctly FAILS CLOSED (422). "+
				"Confinement at %s is UNVERIFIED here — install its runtime (wardyn setup %s) to exercise it.",
				class, class, wallOrVault(class))
		})
	}
}

// TestLive_GradersRejectBadWorkspace proves the graders aren't rubber stamps in
// the FAIL direction: each task shipping a bad-workspace/ fixture (plausible-but-
// wrong final state) must be scored a graded FAILURE (exit 1) — not a pass, and
// not a docker infra error masquerading as a rejection. Needs docker, no stack.
func TestLive_GradersRejectBadWorkspace(t *testing.T) {
	h := newHarness(t)
	ran := 0
	for _, task := range h.loadTasks() {
		task := task
		bad := filepath.Join(task.dir, "bad-workspace")
		if _, err := os.Stat(bad); err != nil {
			continue // no negative fixture for this task
		}
		ran++
		t.Run(task.Name, func(t *testing.T) {
			code, out := h.gradeExit(task.GraderImage, task.dir, bad)
			switch {
			case code == 0:
				t.Fatalf("grader PASSED the bad-workspace fixture — it is a rubber stamp:\n%s", out)
			case code >= 125:
				t.Fatalf("grader exited %d (docker infra error, not a graded rejection):\n%s", code, out)
			default:
				t.Logf("grader correctly REJECTED bad-workspace (exit %d): %s", code, strings.TrimSpace(lastLine(out)))
			}
		})
	}
	if ran == 0 {
		t.Fatal("no bad-workspace fixtures found — the negative-fixture guard is not being exercised")
	}
}

// TestLive_Tasks is the core: for each gradeable corpus task, launch a REAL
// sandbox at CC1 (the installed tier on the reference box), run the agent, and
// prove via a fresh-container state grader that the work was actually done —
// plus, for the egress task, that the sandbox blocked/allowed correctly.
//
// Two lanes share the graders:
//   - ORACLE (default, $0): agent="oracle" runs the task's solution.sh through the
//     real dispatch→exec→completion path. Proves plumbing + graders + confinement.
//   - REAL MODEL (WARDYN_E2E_REAL_MODEL=1): a real claude-code agent (subscription)
//     does the model tasks. Proves an actual agent completes the work.
func TestLive_Tasks(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	// CC1 is the always-on floor for a docker-runner stack; its absence means the
	// stack is misconfigured or /healthz drifted — FAIL (matching this suite's own
	// "no silent skip" bar), never green-skip the whole thing.
	installed := h.installedClasses(ctx)
	if !installed["CC1"] {
		t.Fatal("stack does not advertise CC1 — a docker-runner wardynd must; check /healthz")
	}

	// Run the corpus at EVERY installed tier, so an agent completing its task and
	// the allow/block boundary are proven under each confinement substrate that
	// this host can actually enforce (CC1/runc, CC2/gVisor, CC3/Kata) — not just
	// the floor. Uninstalled tiers are covered by TestLive_TierMatrix (fail-closed).
	for _, class := range sortedInstalled(installed) {
		class := class
		for _, task := range h.loadTasks() {
			task := task
			if task.Interactive || !task.hasSolution() {
				continue
			}
			t.Run(class+"/oracle/"+task.Name, func(t *testing.T) {
				hh := h.forT(t)
				ws := hh.seedWorkspace(task, "oracle-"+class, true)
				spec := hh.buildManualPolicy(task, class, ws, false /* no model */, false)
				run := hh.launchManual(ctx, "oracle", ".wardyn-task/solution.sh", class, spec, false)
				t.Logf("launched oracle run %s at %s (%s)", run.ID, class, run.State)
				final := hh.pollTerminal(run.ID, 180*time.Second)
				if final.State != "COMPLETED" {
					t.Fatalf("oracle run at %s did not COMPLETE: state=%s", class, final.State)
				}
				if string(final.ConfinementClass) != "enforced" && string(final.ConfinementClass) != class {
					t.Logf("note: run.confinement_class=%q (enforced marker)", final.ConfinementClass)
				}
				ok, out := hh.grade(task, ws)
				if !ok {
					t.Fatalf("grader FAILED for %s at %s:\n%s", task.Name, class, out)
				}
				t.Logf("%s grader PASS: %s", class, strings.TrimSpace(lastLine(out)))

				// Allow/block proof = the GRADED in-sandbox evidence (what the agent
				// itself could/couldn't reach; the oracle is the trusted author). The
				// proxy→control-plane audit callback is best-effort corroboration: on
				// a managed-VM docker (Docker Desktop/WSL) it may not route back — a
				// missing event is a known limitation, never a boundary failure.
				if task.Name == "egress-boundary" {
					allow, deny, pending := hh.auditEgress(run.ID)
					t.Logf("%s egress audit corroboration (best-effort): allow=%d deny=%d pending=%d", class, allow, deny, pending)
				}
			})
		}
	}
}

// TestLive_RealModel proves a REAL model-backed agent completes real work. Gated
// (WARDYN_E2E_REAL_MODEL=1) because it calls a real model and costs tokens/time.
// Runs the model tasks through BOTH the composer (subscription) and, when creds
// are staged, the manual subscription path.
func TestLive_RealModel(t *testing.T) {
	h := newHarness(t)
	if !h.realModel {
		t.Skip("WARDYN_E2E_REAL_MODEL=1 not set; skipping the real-model lane (costs model tokens)")
	}
	ctx := context.Background()
	best := h.bestInstalledClass(ctx)
	installedRM := h.installedClasses(ctx)
	haveCreds := h.subscriptionMounts() != nil

	for _, task := range h.loadTasks() {
		task := task
		if !task.NeedsModel || task.Interactive {
			continue
		}

		// MANUAL path (reliable): launch a real claude-code agent directly with a
		// subscription inline policy — no composer-analysis step. Proves "a real
		// model-backed agent actually completes the task" under EACH installed
		// confinement substrate (so a real Opus agent is exercised at CC1 AND CC2,
		// not just the best tier).
		for _, class := range sortedInstalled(installedRM) {
			class := class
			t.Run("manual/"+class+"/"+task.Name, func(t *testing.T) {
				hh := h.forT(t)
				if !haveCreds {
					t.Skip("no staged Claude subscription creds (scripts/stage-claude-creds.sh); skipping manual real-model lane")
				}
				ws := hh.seedWorkspace(task, "manual-"+class, false)
				spec := hh.buildManualPolicy(task, class, ws, true /* model */, false)
				run := hh.launchManual(ctx, "claude-code", task.Prompt, class, spec, false)
				t.Logf("launched real claude-code run %s at %s", run.ID, class)
				final := hh.pollTerminal(run.ID, 300*time.Second)
				if final.State != "COMPLETED" {
					t.Fatalf("real claude-code run at %s did not COMPLETE: state=%s", class, final.State)
				}
				ok, out := hh.grade(task, ws)
				if !ok {
					t.Fatalf("grader FAILED for real-model %s at %s:\n%s", task.Name, class, out)
				}
				t.Logf("REAL-MODEL manual grader PASS at %s: %s", class, strings.TrimSpace(lastLine(out)))
			})
		}

		// COMPOSER path (headline): the full "AI Run Composer → real sandbox →
		// graded" flow. The composer's own ANALYSIS backend (the host claude CLI)
		// can flake independently of the sandbox (e.g. max_turns); that is a
		// composer-robustness issue, not a boundary/agent defect, so a backend 502
		// SKIPS this sub-test rather than failing it.
		t.Run("composer/"+task.Name, func(t *testing.T) {
			hh := h.forT(t)
			ws := hh.seedWorkspace(task, "composer", false)
			run, prop, skip := hh.launchComposer(ctx, task, ws, best)
			if skip != "" {
				t.Skipf("composer analysis backend flaked (not a sandbox failure): %s", skip)
			}
			t.Logf("composed+launched %s run %s; warnings: %s", task.Name, run.ID, strings.Join(prop.Warnings, " | "))
			final := hh.pollTerminal(run.ID, 240*time.Second)
			if final.State != "COMPLETED" {
				t.Fatalf("composed run did not COMPLETE: state=%s", final.State)
			}
			ok, out := hh.grade(task, ws)
			if !ok {
				t.Fatalf("grader FAILED for composed %s:\n%s", task.Name, out)
			}
			t.Logf("REAL-MODEL composer grader PASS: %s", strings.TrimSpace(lastLine(out)))
		})
	}
}

// forT returns a shallow copy of the harness bound to a sub-test's *testing.T so
// helper failures attribute to the right sub-test.
func (h *harness) forT(t *testing.T) *harness {
	cp := *h
	cp.t = t
	return &cp
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// sortedInstalled returns the installed confinement classes weakest-first (CC1,
// CC2, CC3) so tier sub-tests run in a stable, readable order.
func sortedInstalled(installed map[string]bool) []string {
	var out []string
	for _, c := range []string{"CC1", "CC2", "CC3"} {
		if installed[c] {
			out = append(out, c)
		}
	}
	return out
}

func lastLine(s string) string {
	s = strings.TrimRight(s, "\n")
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return s[i+1:]
	}
	return s
}

func wallOrVault(class string) string {
	if class == "CC2" {
		return "wall"
	}
	return "vault"
}
