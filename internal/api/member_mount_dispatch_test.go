// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The member mount posture END TO END: create-run resolves it
// (memberMountPosture), dispatch stamps it onto the SandboxSpec the driver
// actually receives, and the run-create re-check refuses a source that has since
// fallen outside the roots. The helper-level unit is
// TestMemberMountPosture_Resolve (workspace_owner_test.go); these tests exist
// because that one stays green with every production wire cut — a refactor that
// drops `MemberMounts: s.memberMountPosture(...)` from handleCreateRun,
// launchRecordRun or the SandboxSpec literal silently deletes the bind-time
// TOCTOU defense, which is the whole point of §c check 2.
//
// They also pin the asymmetry that broke the feature once: the roots gate the
// MEMBER's own binds, never the operator-staged credential mounts that ride the
// same spec (runner.Mount.MemberAuthored).

// memberCredsSource is the operator's staged ~/.claude dir — deliberately a
// path under no member root, which is what every real deployment looks like.
const memberCredsSource = "/var/lib/wardyn/claude-creds"

// memberDispatchHarness is ownerHarness (OIDC on, real workspace list) plus the
// two things a DISPATCH assertion needs: a runner that captures the SandboxSpec
// and the operator ceiling that blesses the subscription credential mount.
func memberDispatchHarness(t *testing.T, mounts runner.MemberMountPolicy) (*Server, *ownerStore, *fakeRunner) {
	t.Helper()
	st := newOwnerStore()
	h := newHarness(t)
	cfg := baseTestConfig(h, st)
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Broker = h.broker
	cfg.MemberMounts = mounts
	fr := &fakeRunner{}
	cfg.Runner = fr
	cfg.DefaultPolicy = types.RunPolicySpec{
		AllowedDomains:      []string{"api.anthropic.com"},
		MinConfinementClass: types.CC2,
		// The operator-blessed subscription creds mount (scripts/stage-claude-creds.sh
		// + WARDYN_DEFAULT_POLICY) — the compose/resident-copy posture.
		WorkspaceMounts: []types.WorkspaceMount{{Source: memberCredsSource, Target: claudeCredTarget}},
	}
	return New(cfg), st, fr
}

// mountFor returns the spec's bind at target.
func mountFor(t *testing.T, spec runner.SandboxSpec, target string) runner.Mount {
	t.Helper()
	for _, m := range spec.Mounts {
		if m.Target == target {
			return m
		}
	}
	t.Fatalf("no mount at %q in %+v", target, spec.Mounts)
	return runner.Mount{}
}

// createMemberRun drives POST /api/v1/runs for a run attaching ws and returns
// the SandboxSpec the runner was handed.
func createMemberRun(t *testing.T, srv *Server, fr *fakeRunner, session *http.Cookie, wsID uuid.UUID) runner.SandboxSpec {
	t.Helper()
	body := `{"agent":"claude-code","task":"do the thing","workspace_id":"` + wsID.String() + `"}`
	w := doSSO(t, srv, http.MethodPost, "/api/v1/runs", session, body)
	if w.Code != http.StatusCreated {
		t.Fatalf("create run: code = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	if fr.createCalls != 1 {
		t.Fatalf("CreateSandbox calls = %d, want 1", fr.createCalls)
	}
	return fr.lastSpec
}

// memberOwnedWorkspace seeds a scanned workspace owned by owner ("" = the
// operator) whose single source is the local dir at path.
func memberOwnedWorkspace(st *ownerStore, owner, path string) uuid.UUID {
	return st.put(types.Workspace{
		OwnedBy: owner,
		Status:  types.WorkspaceScanned,
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: path}},
	})
}

// TestMemberMountPosture_DispatchedToDriver is the production wire, end to end:
// a member's own run against their own workspace must reach the driver carrying
// (a) that member's roots and (b) a member-authored stamp on their local_dir
// bind and NOT on the operator's blessed credential mount — which is what makes
// the run work at all on a subscription deployment, since the staged creds dir
// lives under no member root.
func TestMemberMountPosture_DispatchedToDriver(t *testing.T) {
	root, project := memberProjectRoot(t)
	srv, st, fr := memberDispatchHarness(t, runner.MemberMountPolicy{Roots: []string{root}})
	member := ssoSession(t, ownerMemberSub, "member@corp.example", oidc.RoleMember)

	spec := createMemberRun(t, srv, fr, member, memberOwnedWorkspace(st, ownerMemberSub, project))

	if len(spec.MemberMountRoots) != 1 || spec.MemberMountRoots[0] != root {
		t.Fatalf("spec.MemberMountRoots = %v, want %v — without this the driver never re-resolves the bind at ContainerCreate time (design §c check 2)",
			spec.MemberMountRoots, []string{root})
	}
	if m := mountFor(t, spec, composerWorkspaceTarget); !m.MemberAuthored {
		t.Errorf("the member's own local_dir bind %q is not stamped MemberAuthored; the driver would skip the within-root re-check", m.Source)
	}
	if m := mountFor(t, spec, claudeCredTarget); m.MemberAuthored {
		t.Errorf("the operator's blessed creds mount %q is stamped MemberAuthored; it lives under no member root, so the driver would REFUSE it and every model run against a member-owned workspace would fail at CreateSandbox", m.Source)
	}
}

// TestMemberMountPosture_OperatorRunUnstamped is the counterfactual: the same
// dispatch for an OPERATOR-owned workspace carries nil roots and no stamps, so
// the driver takes exactly today's path. Without it, "roots are threaded" could
// be satisfied by threading them onto every run.
func TestMemberMountPosture_OperatorRunUnstamped(t *testing.T) {
	root, project := memberProjectRoot(t)
	srv, st, fr := memberDispatchHarness(t, runner.MemberMountPolicy{Roots: []string{root}})
	admin := ssoSession(t, "sub-owner-admin", "admin@corp.example", oidc.RoleAdmin)

	spec := createMemberRun(t, srv, fr, admin, memberOwnedWorkspace(st, "", project))

	if spec.MemberMountRoots != nil {
		t.Errorf("spec.MemberMountRoots = %v, want nil for an operator-owned workspace (today's driver path)", spec.MemberMountRoots)
	}
	for _, m := range spec.Mounts {
		if m.MemberAuthored {
			t.Errorf("operator run stamped %q -> %q member-authored", m.Source, m.Target)
		}
	}
}

// TestCreateRun_MemberSourceOutsideRootsIs422 is the run-create re-check
// (validateWorkspaceSources -> memberMountAllowed): the workspace was onboarded
// while its dir was inside the roots, the operator has since narrowed them, and
// the next run against it must be refused at the door — not dispatched and left
// for the driver to catch.
func TestCreateRun_MemberSourceOutsideRootsIs422(t *testing.T) {
	root, _ := memberProjectRoot(t)
	// A different root tree entirely: the workspace's dir is no longer inside
	// anything the operator allows.
	_, stranded := memberProjectRoot(t)
	srv, st, fr := memberDispatchHarness(t, runner.MemberMountPolicy{Roots: []string{root}})
	member := ssoSession(t, ownerMemberSub, "member@corp.example", oidc.RoleMember)
	wsID := memberOwnedWorkspace(st, ownerMemberSub, stranded)

	body := `{"agent":"claude-code","task":"do the thing","workspace_id":"` + wsID.String() + `"}`
	w := doSSO(t, srv, http.MethodPost, "/api/v1/runs", member, body)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("run against a member source outside every root: code = %d, want 422; body=%s", w.Code, w.Body.String())
	}
	// The refusal must be the MEMBER-mount gate, not some other 422 that happens
	// to fire first (which would leave this test green with the re-check gone).
	if !strings.Contains(w.Body.String(), "member workspace mount") {
		t.Errorf("422 body = %s, want the member workspace mount refusal", w.Body.String())
	}
	if fr.createCalls != 0 {
		t.Errorf("CreateSandbox calls = %d, want 0 — the refusal must happen before dispatch", fr.createCalls)
	}
}

// TestLaunchRecordRun_ThreadsMemberMountPosture is the third production wire:
// an ADMIN-launched record/verify session against a MEMBER-owned workspace
// still carries the member's roots and the member-authored stamp, because the
// SOURCE was member-authored — the gate follows the source, not the launcher.
// (Reuses record_llm_mode_test.go's store fake, the one in-memory harness that
// drives launchRecordRun.)
func TestLaunchRecordRun_ThreadsMemberMountPosture(t *testing.T) {
	root, project := memberProjectRoot(t)
	h := newHarness(t)
	ws := types.Workspace{
		ID: uuid.New(), Status: types.WorkspaceScanned, OwnedBy: ownerMemberSub,
		Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeLocalDir, Path: project}},
	}
	fake := newRecordLLMModeStore(ws)
	cfg := baseTestConfig(h, fake)
	fr := &fakeRunner{}
	cfg.Runner = fr
	cfg.Broker = h.broker
	cfg.MemberMounts = runner.MemberMountPolicy{Roots: []string{root}}
	srv := New(cfg)

	if _, _, err := srv.launchRecordRun(context.Background(), "admin@corp.example", ws, "record", "record", false); err != nil {
		t.Fatalf("launchRecordRun: %v", err)
	}
	if fr.createCalls != 1 {
		t.Fatalf("CreateSandbox calls = %d, want 1", fr.createCalls)
	}
	spec := fr.lastSpec
	if len(spec.MemberMountRoots) != 1 || spec.MemberMountRoots[0] != root {
		t.Fatalf("record session spec.MemberMountRoots = %v, want %v — an admin-launched session over a MEMBER's dir still gates on the member's roots",
			spec.MemberMountRoots, []string{root})
	}
	if m := mountFor(t, spec, composerWorkspaceTarget); !m.MemberAuthored {
		t.Errorf("record session: the member's local_dir bind %q is not stamped MemberAuthored", m.Source)
	}
}
