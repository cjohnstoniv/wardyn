// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// F1 PROBE — approval decide -> credential mint -> egress allow.
//
// DESTINATION: internal/api/approvals_f1_probe_test.go
//   (copy this file there; it reuses the package's existing test fixtures:
//    newScopeFixture / seedEgress / egressLists / decideBody from
//    approvals_decide_test.go, ssoSession / doSSO from rbac_test.go, do +
//    adminToken from api_test.go, and unionWorkspaceEgress from
//    workspace_egress.go — nothing new is exported or mocked.)
//
// RUN (no PG needed — every fixture here is the in-memory authzStore):
//   cd /home/cjohn/wt-v07-profiles && cp local/review-0.7/deep/F1-approval-to-mint-to-egress/approvals_f1_probe_test.go internal/api/ \
//     && nice -n 10 GOMAXPROCS=8 WARDYN_TEST_PG= go test ./internal/api/ -run 'TestF1_' -count=1 -v ; rm internal/api/approvals_f1_probe_test.go
//
// EXPECTED on fa910735 (feat/v0.7-profiles):
//   TestF1_AlwaysTargetsPrimaryWorkspaceOnly     GREEN  (pins the invariant; goes RED if `always` ever leaks to W')
//   TestF1_DecideAuthzMatrix                      GREEN  (pins who-may-decide-for-whose-run; RED on any authz drift)
//   TestF1_ReconcileDoesNotReverseNewerDecision   RED    (hypothesis H2 in the trace doc — reconcile is state-major, not time-major)
//   TestF1_ReconcileDoesNotResurrectRemovedHost   RED    (hypothesis H3 — a PUT removal is undone at the next boot)
// The two RED probes are deliberate: they FAIL while the defect exists and turn
// GREEN once it is fixed. Do not "fix" the probe to pass.

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const f1Host = "registry.npmjs.org"

// f1SeedWorkspace registers a bare workspace row (ownedBy may be "") and
// returns its id. Unlike scopeFixture.seedWorkspace it does NOT touch the run —
// the caller decides the linkage shape, which is the whole point of the probe.
func f1SeedWorkspace(f *scopeFixture, name, ownedBy string) uuid.UUID {
	id := uuid.New()
	f.store.mu.Lock()
	f.store.workspaces[id] = types.Workspace{ID: id, Name: name, OwnedBy: ownedBy}
	f.store.mu.Unlock()
	return id
}

// f1LinkRun rewrites the fixture run's two workspace linkages exactly as given.
func f1LinkRun(f *scopeFixture, runID uuid.UUID, ids []uuid.UUID, trusted *uuid.UUID) {
	f.store.mu.Lock()
	run := f.store.runs[runID]
	run.WorkspaceIDs = ids
	run.WorkspaceID = trusted
	f.store.runs[runID] = run
	f.store.mu.Unlock()
}

// f1SeedApproval seeds a PENDING approval of kind on runID.
func f1SeedApproval(f *scopeFixture, runID uuid.UUID, kind types.ApprovalKind) uuid.UUID {
	id := uuid.New()
	scope := json.RawMessage(`{"host":` + strconv.Quote(f1Host) + `}`)
	if kind == types.ApprovalCredential {
		scope = json.RawMessage(`{"host":"dev.azure.com","secret_name":"ado-pat"}`)
	}
	f.approval.mu.Lock()
	f.approval.byID[id] = types.ApprovalRequest{
		ID: id, RunID: runID, Kind: kind, RequestedScope: scope,
		State: types.ApprovalPending, RequestedAt: time.Now().UTC(),
	}
	f.approval.mu.Unlock()
	return id
}

// TestF1_AlwaysTargetsPrimaryWorkspaceOnly pins the boundary the trace doc
// names: an `always`-scoped decision on an approval raised by a run whose
// PRIMARY workspace is W lands on W and ONLY W. A second workspace W' — whether
// unrelated, the run's non-primary reference, or the run's TRUSTED WorkspaceID —
// must never gain the host, and a FUTURE run of W' must not inherit it through
// unionWorkspaceEgress (the only path a persisted `always` reaches a proxy).
//
// resolveAlwaysTarget's `target := primaryWorkspace(run)` and primaryWorkspace's
// tie-break (both in approvals.go) are the code under test; handleCreateRun
// (runs.go) is where WorkspaceIDs is stamped and unionWorkspaceEgress
// (workspace_egress.go) is where a future run reads it back.
func TestF1_AlwaysTargetsPrimaryWorkspaceOnly(t *testing.T) {
	type shape struct {
		name string
		// link builds the run's linkage from (W, W').
		link       func(w, wPrime uuid.UUID) ([]uuid.UUID, *uuid.UUID)
		wantStatus int
		wantOnW    bool
	}
	cases := []shape{
		{"WorkspaceIDs=[W]",
			func(w, _ uuid.UUID) ([]uuid.UUID, *uuid.UUID) { return []uuid.UUID{w}, nil }, http.StatusOK, true},
		{"WorkspaceIDs=[W] and trusted WorkspaceID=W' — the denormalization wins, the trusted linkage is not a target",
			func(w, wp uuid.UUID) ([]uuid.UUID, *uuid.UUID) { return []uuid.UUID{w}, &wp }, http.StatusOK, true},
		{"WorkspaceIDs=nil and trusted WorkspaceID=W — a record/verify step run",
			func(w, _ uuid.UUID) ([]uuid.UUID, *uuid.UUID) { return nil, &w }, http.StatusOK, true},
		{"WorkspaceIDs=[W, W'] — a multi-workspace run writes ONLY the primary (H1 in the trace doc)",
			func(w, wp uuid.UUID) ([]uuid.UUID, *uuid.UUID) { return []uuid.UUID{w, wp}, nil }, http.StatusOK, true},
		{"WorkspaceIDs=[W', W] — order decides: W' is the primary, so W must NOT be written",
			func(w, wp uuid.UUID) ([]uuid.UUID, *uuid.UUID) { return []uuid.UUID{wp, w}, nil }, http.StatusOK, false},
		{"no linkage at all — 400, nothing written anywhere",
			func(_, _ uuid.UUID) ([]uuid.UUID, *uuid.UUID) { return nil, nil }, http.StatusBadRequest, false},
	}
	for _, verb := range []string{"approve", "deny"} {
		for _, tc := range cases {
			t.Run(verb+"/"+tc.name, func(t *testing.T) {
				f := newScopeFixture(t)
				admin := ssoSession(t, "sub-admin-f1", "admin@corp.example", oidc.RoleAdmin)
				// W' is deliberately owned by a DIFFERENT member than the run's
				// creator, so a leak would also be a cross-owner durable write.
				w := f1SeedWorkspace(f, "W", f.memberID)
				wp := f1SeedWorkspace(f, "W-prime", "sub-other-owner-f1")
				ids, trusted := tc.link(w, wp)
				f1LinkRun(f, f.runID, ids, trusted)

				apID := f.seedEgress(t, f1Host)
				resp := doSSO(t, f.srv, http.MethodPost, "/api/v1/approvals/"+apID.String()+"/"+verb,
					admin, decideBody(t, types.ScopeAlways, nil))
				if resp.Code != tc.wantStatus {
					t.Fatalf("%s always: status = %d, want %d; body=%s", verb, resp.Code, tc.wantStatus, resp.Body.String())
				}

				list := func(wsID uuid.UUID) []string {
					approved, denied := f.egressLists(t, wsID)
					if verb == "approve" {
						return approved
					}
					return denied
				}
				if got := slices.Contains(list(w), f1Host); got != tc.wantOnW {
					t.Errorf("%s always: host on W = %v, want %v (W lists: %v)", verb, got, tc.wantOnW, list(w))
				}
				// THE INVARIANT: a workspace that is NOT the run's primary never
				// learns the host, in either direction. In the "[W', W]" row W' IS
				// the primary, so there it is W (asserted above) that must stay
				// clean; this gate mirrors the FUTURE-RUN half below.
				approvedWp, deniedWp := f.egressLists(t, wp)
				if (tc.wantOnW || tc.wantStatus != http.StatusOK) &&
					(slices.Contains(approvedWp, f1Host) || slices.Contains(deniedWp, f1Host)) {
					t.Errorf("%s always LEAKED to W' (approved=%v denied=%v) — a run whose primary workspace is W unlocked/locked a host on W'",
						verb, approvedWp, deniedWp)
				}
				// The "W' was the primary" row must have written W' and not W: assert
				// it so the row proves something rather than passing vacuously.
				if tc.wantStatus == http.StatusOK && !tc.wantOnW {
					if !slices.Contains(list(wp), f1Host) {
						t.Errorf("%s always on a run whose PRIMARY is W' wrote nowhere: W'=%v", verb, list(wp))
					}
				}

				// FUTURE-RUN HALF: what a fresh run of each workspace would inherit.
				// unionWorkspaceEgress is the single reader of ApprovedEgress /
				// DeniedEgress on the run-create path (unionRunEgress in
				// runs_create.go).
				wsW, _ := f.store.GetWorkspace(t.Context(), w)
				wsWp, _ := f.store.GetWorkspace(t.Context(), wp)
				var specWp types.RunPolicySpec
				unionWorkspaceEgress(&specWp, []types.Workspace{wsWp})
				if tc.wantOnW || tc.wantStatus != http.StatusOK {
					if slices.Contains(specWp.AllowedDomains, f1Host) || slices.Contains(specWp.DeniedDomains, f1Host) {
						t.Errorf("a future run of W' inherits %q (allowed=%v denied=%v)", f1Host, specWp.AllowedDomains, specWp.DeniedDomains)
					}
				}
				if tc.wantOnW {
					var specW types.RunPolicySpec
					unionWorkspaceEgress(&specW, []types.Workspace{wsW})
					want := specW.AllowedDomains
					if verb == "deny" {
						want = specW.DeniedDomains
					}
					if !slices.Contains(want, f1Host) {
						t.Errorf("a future run of W does NOT inherit the %s·always (allowed=%v denied=%v)", verb, specW.AllowedDomains, specW.DeniedDomains)
					}
				}
			})
		}
	}
}

// f1Caller is one row of the decide-authz matrix's caller axis.
type f1Caller struct {
	tier string // admin | security_admin | member_owner | member_other | admin_token | anonymous
	sub  string
}

// f1Do issues the decide for a caller: bearer for the admin token, SSO cookie
// for every human tier, nothing for anonymous.
func f1Do(t *testing.T, f *scopeFixture, c f1Caller, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	switch c.tier {
	case "admin_token":
		return do(t, f.srv, http.MethodPost, path, adminToken, body)
	case "anonymous":
		return doSSO(t, f.srv, http.MethodPost, path, nil, body)
	case "admin":
		return doSSO(t, f.srv, http.MethodPost, path, ssoSession(t, c.sub, c.sub+"@corp.example", oidc.RoleAdmin), body)
	case "security_admin":
		return doSSO(t, f.srv, http.MethodPost, path, ssoSession(t, c.sub, c.sub+"@corp.example", oidc.RoleSecurityAdmin), body)
	default: // member_owner / member_other
		return doSSO(t, f.srv, http.MethodPost, path, ssoSession(t, c.sub, c.sub+"@corp.example", oidc.RoleMember), body)
	}
}

// f1Want is the decide-authz matrix in ONE readable function: who may decide
// what, on whose run, at which scope. It is the executable form of
// authorizeMemberDecision (the member gate, whose security-tier arm passes
// every kind on every run) and resolveAlwaysTarget (`always` is operator-only),
// both in approvals.go, plus isSecurityOperator in http.go (its no-OIDC-human
// arm = admin token).
func f1Want(c f1Caller, kind types.ApprovalKind, ownRun bool, scope types.ApprovalScope) int {
	switch c.tier {
	case "anonymous":
		return http.StatusUnauthorized
	case "admin", "security_admin", "admin_token":
		// Every kind, every run, every scope. NOTE for R1: this includes a
		// security_admin approving a CREDENTIAL approval on a run THEY created
		// (see trace doc H5) — the matrix pins current behavior, it does not bless it.
		return http.StatusOK
	case "member_owner":
		if kind != types.ApprovalEgressDomain || !ownRun {
			return http.StatusNotFound // byte-identical 404: no kind/existence oracle
		}
		if scope == types.ScopeAlways {
			return http.StatusForbidden // rule 6: operator-only, and a 403 (not 404) is correct here
		}
		return http.StatusOK
	default: // member_other never owns anything
		return http.StatusNotFound
	}
}

// TestF1_DecideAuthzMatrix walks caller x kind x ownership x scope and asserts
// the status f1Want predicts. Every cell builds a FRESH fixture, so a decided
// row can never shadow a 404 with a 409 (the L3 hazard authz_test.go names).
func TestF1_DecideAuthzMatrix(t *testing.T) {
	callers := []f1Caller{
		{"admin", "sub-admin-f1m"},
		{"security_admin", "sub-secadmin-f1m"},
		{"member_owner", ""}, // sub filled from the fixture (the run's creator)
		{"member_other", "sub-other-f1m"},
		{"admin_token", ""},
		{"anonymous", ""},
	}
	kinds := []types.ApprovalKind{types.ApprovalEgressDomain, types.ApprovalCredential}
	scopes := []types.ApprovalScope{"", types.ScopeAlways}

	for _, c := range callers {
		for _, kind := range kinds {
			for _, own := range []bool{true, false} {
				for _, scope := range scopes {
					if scope == types.ScopeAlways && kind != types.ApprovalEgressDomain {
						continue // rule 4 (400) is a validation cell, not an authz one — covered by TestDecideScope_NonEgressKindRejectsScope
					}
					name := c.tier + "/" + string(kind) + "/own=" + strconv.FormatBool(own) + "/scope=" + string(scope)
					t.Run(name, func(t *testing.T) {
						f := newScopeFixture(t)
						caller := c
						if caller.tier == "member_owner" {
							caller.sub = f.memberID
						}
						// Every run gets a primary workspace so `always` can only fail
						// on AUTHZ, never on rule 5 (400).
						ws := f1SeedWorkspace(f, "ws-matrix", f.memberID)
						runID := f.runID
						if !own {
							runID = uuid.New()
							f.store.mu.Lock()
							f.store.runs[runID] = types.AgentRun{ID: runID, CreatedBy: "someone-else-f1m", State: types.RunRunning}
							f.store.mu.Unlock()
						}
						f1LinkRun(f, runID, []uuid.UUID{ws}, nil)
						apID := f1SeedApproval(f, runID, kind)

						body := `{"reason":"f1"}`
						if scope != "" {
							body = decideBody(t, scope, nil)
						}
						for _, verb := range []string{"approve", "deny"} {
							// A fresh approval per verb: the first verb decides the row.
							id := apID
							if verb == "deny" {
								id = f1SeedApproval(f, runID, kind)
							}
							resp := f1Do(t, f, caller, "/api/v1/approvals/"+id.String()+"/"+verb, body)
							want := f1Want(caller, kind, own, scope)
							if resp.Code != want {
								t.Errorf("%s: status = %d, want %d; body=%s", verb, resp.Code, want, resp.Body.String())
							}
							// A refused decision must leave the row PENDING (one-way FSM).
							if want != http.StatusOK {
								f.approval.mu.Lock()
								st := f.approval.byID[id].State
								f.approval.mu.Unlock()
								if st != types.ApprovalPending {
									t.Errorf("%s: refused with %d but the row moved to %q", verb, resp.Code, st)
								}
							}
						}
					})
				}
			}
		}
	}
}

// f1SeedDecidedAlways seeds an already-DECIDED always-scoped egress approval
// with an explicit DecidedAt, modelling what a live decide() left behind.
func f1SeedDecidedAlways(f *scopeFixture, state types.ApprovalState, decidedAt time.Time) uuid.UUID {
	id := uuid.New()
	at := decidedAt
	f.approval.mu.Lock()
	f.approval.byID[id] = types.ApprovalRequest{
		ID: id, RunID: f.runID, Kind: types.ApprovalEgressDomain,
		RequestedScope: json.RawMessage(`{"host":` + strconv.Quote(f1Host) + `}`),
		State:          state, DecisionScope: types.ScopeAlways, DecidedAt: &at,
		RequestedAt: decidedAt.Add(-time.Minute),
	}
	f.approval.mu.Unlock()
	return id
}

// TestF1_ReconcileDoesNotReverseNewerDecision — EXPECTED RED on fa910735 (H2).
//
// Sequence an operator can produce in two clicks: deny·always H (t1), then,
// having changed their mind, approve·always H (t2 > t1). The live write-backs
// leave the workspace with H on approved_egress and off denied_egress. The boot
// heal (ReconcileWorkspaceEgressDecisions in approvals.go) then walks
// states in the fixed order [APPROVED, DENIED] — never by decided_at — so the
// OLDER deny is applied LAST and silently reverses the operator's newest
// decision on every restart, with no audit event.
func TestF1_ReconcileDoesNotReverseNewerDecision(t *testing.T) {
	f := newScopeFixture(t)
	ws := f1SeedWorkspace(f, "ws-reconcile", f.memberID)
	f1LinkRun(f, f.runID, []uuid.UUID{ws}, nil)
	// Live state after the two decisions: approved wins because it was last.
	f.store.mu.Lock()
	w := f.store.workspaces[ws]
	w.ApprovedEgress, w.DeniedEgress = []string{f1Host}, nil
	f.store.workspaces[ws] = w
	f.store.mu.Unlock()

	t1 := time.Now().UTC().Add(-2 * time.Hour)
	f1SeedDecidedAlways(f, types.ApprovalDenied, t1)                  // older
	f1SeedDecidedAlways(f, types.ApprovalApproved, t1.Add(time.Hour)) // newer

	if _, err := f.srv.ReconcileWorkspaceEgressDecisions(t.Context()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	approved, denied := f.egressLists(t, ws)
	if !slices.Contains(approved, f1Host) || slices.Contains(denied, f1Host) {
		t.Fatalf("boot reconcile REVERSED the operator's newest decision: approved=%v denied=%v (want %q approved — the newer approve·always must win over the older deny·always)",
			approved, denied, f1Host)
	}
}

// TestF1_ReconcileDoesNotResurrectRemovedHost — EXPECTED RED on fa910735 (H3).
//
// resolveAlwaysTarget (approvals.go) promises an `always` is "reversible via the
// denied-egress/approved-egress PUTs". An operator who approve·always'd H and
// later removed it through PUT /workspaces/{id}/approved-egress (the documented
// undo, handleSetApprovedEgress in workspaces.go) gets H back on the allowlist
// at the next boot: the approval row still says APPROVED/always and reconcile
// re-applies it (in ReconcileWorkspaceEgressDecisions). That is a durable,
// fail-OPEN widening of a workspace the operator explicitly narrowed, and
// nothing audits it.
func TestF1_ReconcileDoesNotResurrectRemovedHost(t *testing.T) {
	f := newScopeFixture(t)
	ws := f1SeedWorkspace(f, "ws-undo", f.memberID)
	f1LinkRun(f, f.runID, []uuid.UUID{ws}, nil)
	f1SeedDecidedAlways(f, types.ApprovalApproved, time.Now().UTC().Add(-time.Hour))
	// The operator's undo: the full-replace PUT semantics, host removed.
	if _, err := f.store.SetWorkspaceApprovedEgress(t.Context(), ws, nil); err != nil {
		t.Fatalf("undo PUT: %v", err)
	}
	if _, err := f.srv.ReconcileWorkspaceEgressDecisions(t.Context()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	approved, _ := f.egressLists(t, ws)
	if slices.Contains(approved, f1Host) {
		t.Fatalf("boot reconcile RESURRECTED %q after the operator removed it via the approved-egress PUT: approved=%v", f1Host, approved)
	}
}
