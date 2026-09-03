// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ─── the scan door ────────────────────────────────────────────────────────────

// scanLaneStore adds the source fence + scan-result writes launchSourceScanRun
// performs to the governance escape store, so the whole lane can run for real.
type scanLaneStore struct {
	*govEscapeStore
}

func (s *scanLaneStore) ClaimSourceActiveRun(context.Context, uuid.UUID, uuid.UUID) error {
	return nil
}
func (s *scanLaneStore) ClearSourceActiveRun(context.Context, uuid.UUID, uuid.UUID) error {
	return nil
}
func (s *scanLaneStore) SetSourceScanResultUnfenced(_ context.Context, _ uuid.UUID, _ []byte, st types.WorkspaceStatus, _ map[string]types.WorkspaceRequirement) (types.Source, error) {
	return types.Source{Status: st}, nil
}

// TestSourceScanLaneResolvesTheActingPrincipalsCeiling is the scan half of the
// dispatch-ceiling requirement.
//
// POST /workspaces/{id}/scan is mounted on the MEMBER group (owner-or-admin in
// handleScanWorkspace), and the run it launches carries a git-broker grant and
// SSH grants for the source's clone host. Before this, launchSourceScanRun built
// its dispatchParams without ever resolving a ceiling, so:
//
//   - the profile's denies were never unioned in and no credential lane was
//     dropped — a member walled off a forge still got a brokered credential FOR
//     that forge, through a door their profile was supposed to close;
//   - no run.ceiling.reassert row was written, so an auditor could not tell the
//     ceiling had been skipped: the absence looked exactly like an unassigned
//     principal;
//   - confinement came from s.cfg.DefaultPolicy rather than the principal's own
//     ceiling.
//
// All three are asserted, against the SAME lane run twice — assigned and
// unassigned — so nothing but the assignment can explain the split.
func TestSourceScanLaneResolvesTheActingPrincipalsCeiling(t *testing.T) {
	// The profile denies the forge this scan clones from, which is what makes
	// the broker-lane drop observable: github.com is in gitBrokerManagedHosts.
	walled := govProfile("walled")
	walled.Ceiling.DeniedDomains = []string{"github.com"}
	walled.Ceiling.MinConfinementClass = types.CC3

	scan := func(t *testing.T, assigned bool) (*recRecorder, runner.SandboxSpec, types.AgentRun) {
		t.Helper()
		cs := &capStore{}
		if assigned {
			cs = assignedStore(walled)
		}
		srv, st, audit := govEscapeFixture(t, cs)
		fr := &fakeRunner{}
		srv.cfg.Runner = fr
		srv.cfg.Store = &scanLaneStore{govEscapeStore: st}
		// The DEPLOYMENT floor is deliberately the WEAKEST class and differs
		// from the profile's: a scan that still reads Config.DefaultPolicy shows
		// up as CC1 on the assigned arm.
		srv.cfg.DefaultPolicy.MinConfinementClass = types.CC1

		// A signed-in MEMBER's context, group snapshot and all — the ceiling
		// resolver refuses an unanswerable one, so a bare identity ctx would
		// fail this lane for the wrong reason.
		ctx := govMemberCtx([]string{"eng"}, false)
		src := types.Source{ID: uuid.New(), Kind: types.SourceRepo, Locator: govWorkspaceRepo}
		run, err := srv.launchSourceScanRun(ctx, "bob@corp.example", src)
		if err != nil {
			t.Fatalf("launchSourceScanRun: %v", err)
		}
		return audit, fr.lastSpec, run
	}

	t.Run("an ASSIGNED member's scan reaches dispatch carrying its ceiling", func(t *testing.T) {
		audit, spec, run := scan(t, true)

		ev := findAudit(audit.events, run.ID, "run.ceiling.reassert", "success")
		if ev == nil {
			t.Fatalf("the scan lane recorded no run.ceiling.reassert — it never resolved a ceiling, so the phase is inert on this door; events=%s",
				auditDump(audit.events, run.ID))
		}
		var got struct {
			Profile      string   `json:"profile"`
			DroppedLanes []string `json:"dropped_broker_lanes"`
		}
		if err := json.Unmarshal(ev.Data, &got); err != nil {
			t.Fatalf("decode run.ceiling.reassert: %v", err)
		}
		if got.Profile != "walled" {
			t.Errorf("run.ceiling.reassert profile = %q, want %q", got.Profile, "walled")
		}
		// The load-bearing half: the brokered clone credential for the denied
		// forge is WITHHELD, not merely accompanied by a deny entry.
		if len(got.DroppedLanes) == 0 {
			t.Error("the ceiling denied the clone forge but no broker lane was dropped — the member still holds a credential for a host their profile walls off")
		}
		if len(spec.ProxyConfig.GitGrants) != 0 {
			t.Errorf("ProxyConfig.GitGrants = %v, want empty — the git-broker lane reaches a denied host", spec.ProxyConfig.GitGrants)
		}
		// And the floor is the PRINCIPAL's, not the deployment's.
		if spec.ConfinementClass != types.CC3 {
			t.Errorf("confinement class = %q, want the profile's %q (the deployment floor is CC1)", spec.ConfinementClass, types.CC3)
		}
	})

	t.Run("an UNASSIGNED member's scan carries none (absent-row)", func(t *testing.T) {
		audit, spec, run := scan(t, false)
		if ev := findAudit(audit.events, run.ID, "run.ceiling.reassert", "success"); ev != nil {
			t.Errorf("run.ceiling.reassert fired for an UNASSIGNED member — the phase must stay a provable no-op there: %s", ev.Data)
		}
		if len(spec.ProxyConfig.GitGrants) == 0 {
			t.Error("the unassigned member's scan lost its git-broker lane — the fix must not narrow the no-profile path")
		}
		if spec.ConfinementClass != types.CC1 {
			t.Errorf("confinement class = %q, want the deployment floor %q for an unassigned principal", spec.ConfinementClass, types.CC1)
		}
	})
}

// ─── the seam itself ──────────────────────────────────────────────────────────

// TestDispatchCeilingZeroValueRefusesToLaunch pins the runtime half of the
// structural guarantee. The compiler forces a lane to pass SOMETHING; this is
// what happens if a future lane passes the one thing it can construct without
// deciding. Fail CLOSED — a failed run with an audit row — never a sandbox that
// runs with no profile enforcement.
func TestDispatchCeilingZeroValueRefusesToLaunch(t *testing.T) {
	fr := &fakeRunner{}
	srv, _, audit, run := dispatchTeardownFixture(t, fr, types.RunPending)

	srv.dispatchRun(context.Background(), run, dispatchCeiling{}, dispatchParams{
		RunToken: "run-token", Image: "wardyn/claude-code:latest",
	})

	if fr.createCalls != 0 {
		t.Errorf("CreateSandbox called %d times — an unresolved ceiling must never reach the runner", fr.createCalls)
	}
	ev := findAudit(audit.events, run.ID, "run.dispatch", "failure")
	if ev == nil {
		t.Fatalf("no run.dispatch failure event for a refused launch; events=%s", auditDump(audit.events, run.ID))
	}
	if !strings.Contains(string(ev.Data), "unresolved governance ceiling") {
		t.Errorf("run.dispatch failure data = %s, want it to name the unresolved ceiling", ev.Data)
	}
}

// TestDispatchCeilingIsRequiredAtEveryLane is the enumeration the finding asks
// for, and it guards the ONE hole the type system leaves: a lane could satisfy
// the required argument with a bare dispatchCeiling{} literal and be fail-open
// again (the runtime guard above turns that into a refused launch, but a refused
// launch found in production is worse than a red test here).
//
// So: outside the file that defines the constructors, no production source may
// build a dispatchCeiling literal at all — the only ways to obtain one are
// ceilingForDispatch (from an already-resolved ceiling) and resolveDispatchCeiling
// (which resolves and fails closed). Every dispatch lane must therefore have
// decided.
func TestDispatchCeilingIsRequiredAtEveryLane(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	lanes := map[string]int{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, rerr := os.ReadFile(filepath.Clean(name))
		if rerr != nil {
			t.Fatalf("read %s: %v", name, rerr)
		}
		body := string(src)
		if name != "runs_dispatch_ceiling.go" && strings.Contains(body, "dispatchCeiling{") {
			t.Errorf("%s builds a dispatchCeiling literal — a dispatch lane must obtain its ceiling from "+
				"ceilingForDispatch or resolveDispatchCeiling, never by constructing one (the zero value is the fail-open state this replaced)", name)
		}
		// The other way to satisfy the compiler without deciding: hand the
		// constructor an EMPTY governanceCeiling, which answers "no profile
		// applies" for a principal nobody asked about. Same fail-open, one call
		// deeper.
		if strings.Contains(body, "ceilingForDispatch(governanceCeiling{") {
			t.Errorf("%s calls ceilingForDispatch on a literal governanceCeiling — that asserts \"no profile applies\" "+
				"without resolving anyone's ceiling; use resolveDispatchCeiling, or pass a ceiling effectiveCeiling actually returned", name)
		}
		for _, call := range []string{"s.dispatchRun(", "s.dispatchAndSettle("} {
			lanes[name] += strings.Count(body, call)
		}
	}
	// Every lane that dispatches must also be a lane that resolves: it either
	// holds a governanceCeiling already (ceilingForDispatch) or asks for one
	// (resolveDispatchCeiling). This is a property of the call sites, not a
	// frozen count — a sixth lane passes by deciding, which is the whole point.
	total := 0
	for name, n := range lanes {
		if n == 0 {
			continue
		}
		total += n
		src, rerr := os.ReadFile(filepath.Clean(name))
		if rerr != nil {
			t.Fatalf("read %s: %v", name, rerr)
		}
		body := string(src)
		if !strings.Contains(body, "ceilingForDispatch(") && !strings.Contains(body, "resolveDispatchCeiling(") {
			t.Errorf("%s dispatches %d run(s) but names neither ceilingForDispatch nor resolveDispatchCeiling — "+
				"that lane has not decided which ceiling its runs are bounded by", name, n)
		}
	}
	if total == 0 {
		t.Fatal("found no dispatch call sites at all — this test stopped scanning what it claims to scan")
	}
}
