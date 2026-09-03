// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// audit returns what the fixture's server recorded so far.
func (f *scopeFixture) audit() []types.AuditEvent { return f.rec.events }

// seedCredential is seedEgress's other-kind twin: a PENDING CREDENTIAL approval
// on the same run, for asserting what the egress-only rules do NOT touch.
func (f *scopeFixture) seedCredential(t *testing.T) uuid.UUID {
	t.Helper()
	id := uuid.New()
	f.approval.mu.Lock()
	f.approval.byID[id] = types.ApprovalRequest{
		ID: id, RunID: f.runID, Kind: types.ApprovalCredential,
		RequestedScope: json.RawMessage(`{"host":"dev.azure.com","secret_name":"ado-pat"}`),
		State:          types.ApprovalPending, RequestedAt: time.Now().UTC(),
	}
	f.approval.mu.Unlock()
	return id
}

// TestSecondHuman_OffByDefault is the compatibility half: with the switch unset,
// the run's own creator decides their own egress approval exactly as before.
func TestSecondHuman_OffByDefault(t *testing.T) {
	f := newScopeFixture(t)
	creator := ssoSession(t, f.memberID, "member@corp.example", oidc.RoleAdmin)
	id := f.seedEgress(t, "registry.npmjs.org")

	w := doSSO(t, f.srv, http.MethodPost, "/api/v1/approvals/"+id.String()+"/approve", creator, `{}`)
	if w.Code != http.StatusOK {
		t.Fatalf("switch off, creator decides: status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// TestSecondHuman_RefusesTheRunsCreator is the switch doing its job: the human
// who created the run cannot be the human who approves its egress. Asserted for
// BOTH verbs — a self-DENY is not a safe direction to leave open, since a deny
// is how an operator closes an approval they would rather nobody saw.
func TestSecondHuman_RefusesTheRunsCreator(t *testing.T) {
	for _, verb := range []string{"approve", "deny"} {
		t.Run(verb, func(t *testing.T) {
			t.Setenv(envEgressSecondHuman, "1")
			f := newScopeFixture(t)
			creator := ssoSession(t, f.memberID, "member@corp.example", oidc.RoleAdmin)
			id := f.seedEgress(t, "registry.npmjs.org")

			w := doSSO(t, f.srv, http.MethodPost, "/api/v1/approvals/"+id.String()+"/"+verb, creator, `{}`)
			if w.Code != http.StatusForbidden {
				t.Fatalf("creator self-%s: status = %d, want 403; body=%s", verb, w.Code, w.Body.String())
			}
			// PENDING -> decided is one-way, so the refusal must land BEFORE
			// Decide(): a 403 over an already-decided approval would be a gate
			// nobody can take back.
			f.approval.mu.Lock()
			state := f.approval.byID[id].State
			f.approval.mu.Unlock()
			if state != types.ApprovalPending {
				t.Fatalf("approval state = %q after a refused decision, want PENDING", state)
			}
		})
	}
}

// TestSecondHuman_AdmitsADifferentHuman: the gate wants a SECOND human, not no
// human. Another admin — who did not create the run — decides normally.
func TestSecondHuman_AdmitsADifferentHuman(t *testing.T) {
	t.Setenv(envEgressSecondHuman, "1")
	f := newScopeFixture(t)
	other := ssoSession(t, "sub-someone-else", "reviewer@corp.example", oidc.RoleAdmin)
	id := f.seedEgress(t, "registry.npmjs.org")

	w := doSSO(t, f.srv, http.MethodPost, "/api/v1/approvals/"+id.String()+"/approve", other, `{}`)
	if w.Code != http.StatusOK {
		t.Fatalf("second human approves: status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// TestSecondHuman_AdminTokenBreakGlass pins the documented BYPASS, so the
// exemption is a tested property rather than an accident someone later "fixes"
// into a lockout. A bare admin-token caller is attributed system/admin-token
// precisely because a shared token carries no per-human identity — there is no
// second human to compare it against — so it passes, and the bypass is recorded
// as approval.second_human.bypass rather than left silent.
func TestSecondHuman_AdminTokenBreakGlass(t *testing.T) {
	t.Setenv(envEgressSecondHuman, "1")
	f := newScopeFixture(t)
	id := f.seedEgress(t, "registry.npmjs.org")

	w := do(t, f.srv, http.MethodPost, "/api/v1/approvals/"+id.String()+"/approve", adminToken, `{}`)
	if w.Code != http.StatusOK {
		t.Fatalf("admin-token break-glass: status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	found := false
	for _, ev := range f.audit() {
		if ev.Action == "approval.second_human.bypass" {
			found = true
			if ev.ActorType != types.ActorSystem || ev.Actor != adminTokenPrincipal {
				t.Errorf("bypass event actor = %s/%s, want %s/%s",
					ev.ActorType, ev.Actor, types.ActorSystem, adminTokenPrincipal)
			}
		}
	}
	if !found {
		t.Fatal("admin-token bypass wrote no approval.second_human.bypass event — the break-glass must not be silent")
	}
}

// TestSecondHuman_CredentialApprovalUnaffected: the switch is scoped to EGRESS
// decisions. A credential approval — already admin-only regardless of ownership
// — is not narrowed further, so an operator who happens to have created the run
// can still release its credential.
func TestSecondHuman_CredentialApprovalUnaffected(t *testing.T) {
	t.Setenv(envEgressSecondHuman, "1")
	f := newScopeFixture(t)
	creator := ssoSession(t, f.memberID, "member@corp.example", oidc.RoleAdmin)
	id := f.seedCredential(t)

	w := doSSO(t, f.srv, http.MethodPost, "/api/v1/approvals/"+id.String()+"/approve", creator, `{}`)
	if w.Code != http.StatusOK {
		t.Fatalf("credential approval under the egress switch: status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// localSecondHumanFixture is a LOCALMODE server holding one PENDING egress
// approval on a run created by createdBy. LocalMode is the no-auth bypass, so
// the request needs a loopback peer and a loopback Host and no credential at
// all — exactly what a local CLI or the console sends.
func localSecondHumanFixture(t *testing.T, createdBy string) (*Server, *authzApprovals, uuid.UUID) {
	t.Helper()
	ast := newAuthzStore()
	aap := newAuthzApprovals(ast)
	h := newHarness(t)
	cfg := baseTestConfig(h, ast)
	cfg.Approvals = aap
	cfg.LocalMode = true
	cfg.LocalOperator = "local:alice"
	srv := New(cfg)

	runID := uuid.New()
	ast.mu.Lock()
	ast.runs[runID] = types.AgentRun{ID: runID, CreatedBy: createdBy, State: types.RunRunning}
	ast.mu.Unlock()

	apID := uuid.New()
	aap.mu.Lock()
	aap.byID[apID] = types.ApprovalRequest{
		ID: apID, RunID: runID, Kind: types.ApprovalEgressDomain,
		RequestedScope: json.RawMessage(`{"host":"registry.npmjs.org"}`),
		State:          types.ApprovalPending, RequestedAt: time.Now().UTC(),
	}
	aap.mu.Unlock()
	return srv, aap, apID
}

// localDecide drives one decision as a local-mode client, optionally carrying
// the DEV-ONLY X-Wardyn-Principal attribution override.
func localDecide(t *testing.T, srv *Server, apID uuid.UUID, verb, principalHeader string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/approvals/"+apID.String()+"/"+verb, http.NoBody)
	req.Host = "127.0.0.1:8080"
	req.RemoteAddr = "127.0.0.1:54321"
	if principalHeader != "" {
		req.Header.Set("X-Wardyn-Principal", principalHeader)
	}
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

// TestSecondHuman_LocalModeRefusesTheSwitch pins the mode where the four-eyes
// gate cannot bind, and it pins WHY — because the reason decides the fix.
//
// The gate compared run.CreatedBy against actorFromRequest's principal. In
// LocalMode BOTH of those are client-supplied: actorFromRequest honors the
// DEV-ONLY X-Wardyn-Principal header there (by design, for attribution), and
// run.CreatedBy is written from that same function at create. So the mode had
// TWO ways past the gate, and the second is the one that rules out the narrow
// fix:
//
//	arm 1 — forge the DECIDER: the run's own creator adds one header and the
//	        gate passes, stamping the invented name as decided_by.
//	arm 2 — forge the CREATOR: create the run under a header, then decide it
//	        yourself with NO header at all. Comparing the INJECTED operator
//	        instead of the header (the obvious fix) still passes this one,
//	        because local:alice != local:carol.
//
// LocalMode authenticates nobody and its only non-client-supplied identity is
// one deployment-wide constant, so no request in it can ever prove a second
// human decided. The switch is therefore refused outright — 503, naming the
// incompatibility and the remedy — rather than enforced by a comparison that
// two different forgeries walk through.
func TestSecondHuman_LocalModeRefusesTheSwitch(t *testing.T) {
	const wantMsg = "cannot be enforced in local mode"

	for _, c := range []struct {
		name      string
		createdBy string
		header    string
	}{
		{"forged decider: the creator adds a header", "local:alice", "local:bob"},
		{"no header at all: the plain self-decision", "local:alice", ""},
		{"forged creator: the run was created under a header", "local:carol", ""},
		{"forged creator AND decider", "local:carol", "local:dave"},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, verb := range []string{"approve", "deny"} {
				t.Setenv(envEgressSecondHuman, "1")
				srv, aap, apID := localSecondHumanFixture(t, c.createdBy)
				w := localDecide(t, srv, apID, verb, c.header)
				if w.Code != http.StatusServiceUnavailable {
					t.Fatalf("%s: status = %d, want 503; body=%s", verb, w.Code, w.Body.String())
				}
				if !strings.Contains(w.Body.String(), wantMsg) {
					t.Errorf("%s: body = %s, want it to name the incompatibility (%q)", verb, w.Body.String(), wantMsg)
				}
				// PENDING -> decided is one-way, so the refusal must land BEFORE
				// Decide() and leave nothing stamped: the whole failure class here
				// is an audit row naming a human who did not decide.
				aap.mu.Lock()
				state, decidedBy := aap.byID[apID].State, aap.byID[apID].DecidedBy
				aap.mu.Unlock()
				if state != types.ApprovalPending {
					t.Errorf("%s: approval state = %q after a refused decision, want PENDING", verb, state)
				}
				if decidedBy != "" {
					t.Errorf("%s: decided_by = %q after a refused decision, want empty", verb, decidedBy)
				}
			}
		})
	}

	// SCOPED TO THE SWITCH. With it unset, local mode decides exactly as before
	// — the refusal must not become a local-mode-wide outage.
	t.Run("switch off: local mode decides normally", func(t *testing.T) {
		srv, _, apID := localSecondHumanFixture(t, "local:alice")
		if w := localDecide(t, srv, apID, "approve", ""); w.Code != http.StatusOK {
			t.Fatalf("switch off: status = %d, want 200; body=%s", w.Code, w.Body.String())
		}
	})

	// SCOPED TO EGRESS. The switch governs egress_domain decisions only, so a
	// credential approval in local mode is untouched even with it on — the
	// refusal sits after the kind check for exactly this reason.
	t.Run("credential approval is untouched", func(t *testing.T) {
		t.Setenv(envEgressSecondHuman, "1")
		srv, aap, _ := localSecondHumanFixture(t, "local:alice")
		credID := uuid.New()
		aap.mu.Lock()
		runID := uuid.Nil
		for _, ap := range aap.byID {
			runID = ap.RunID
		}
		aap.byID[credID] = types.ApprovalRequest{
			ID: credID, RunID: runID, Kind: types.ApprovalCredential,
			RequestedScope: json.RawMessage(`{"host":"dev.azure.com","secret_name":"ado-pat"}`),
			State:          types.ApprovalPending, RequestedAt: time.Now().UTC(),
		}
		aap.mu.Unlock()
		if w := localDecide(t, srv, credID, "approve", ""); w.Code == http.StatusServiceUnavailable {
			t.Fatalf("credential approval got the egress switch's 503: %s", w.Body.String())
		}
	})
}

// TestSecondHuman_FailsClosedWhenTheRunCannotBeRead is the gate's documented
// fail-closed half: with the switch on and the approval's run unreadable, the
// decision must 503 rather than pass, because "without the run we cannot prove
// the decider is not its creator".
//
// WHO CAN REACH IT, established by execution before this test was written,
// because an untested fail-closed branch is exactly where a fixture that cannot
// reach it hides:
//
//	SSO admin            -> 503   reachable
//	SSO security_admin   -> 503   reachable
//	MEMBER               -> 404   UNREACHABLE — authorizeMemberDecision loads the
//	                              run itself for a member and 404s first, so
//	                              haveRun is true and this block is skipped
//	admin token          -> 200   bypasses the gate entirely (break-glass)
//
// So the branch is live code on the security tier only, and a member-session
// test would have "passed" against a 404 without ever reaching it. Both arms
// below therefore drive an SSO admin.
//
// The body is asserted, not just the status, and that is the load-bearing part:
// requireSecondHuman now has TWO 503s — this one and the local-mode refusal — so
// a status-only assertion would pass if the fixture ever acquired LocalMode and
// the OTHER branch answered. The fixture is asserted non-local for the same
// reason.
func TestSecondHuman_FailsClosedWhenTheRunCannotBeRead(t *testing.T) {
	const wantMsg = "could not be read to verify a second human decided it"
	const localMsg = "cannot be enforced in local mode"

	// unreadable is the two ways the run can be unavailable, which the code
	// deliberately treats identically: a read error, and a backend with no run
	// store at all. A nil Store must not read as a pass.
	for _, c := range []struct {
		name    string
		breakIt func(t *testing.T, f *scopeFixture)
	}{
		{"GetRun errors", func(t *testing.T, f *scopeFixture) {
			f.store.mu.Lock()
			delete(f.store.runs, f.runID) // ErrNotFound from the fixture's GetRun
			f.store.mu.Unlock()
		}},
		{"no run store configured at all", func(t *testing.T, f *scopeFixture) {
			f.srv.cfg.Store = nil
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, verb := range []string{"approve", "deny"} {
				t.Setenv(envEgressSecondHuman, "1")
				f := newScopeFixture(t)
				if f.srv.cfg.LocalMode {
					t.Fatal("fixture is in LocalMode — the local-mode refusal would answer first and this test " +
						"would be asserting the wrong 503")
				}
				id := f.seedEgress(t, "registry.npmjs.org")
				c.breakIt(t, f)

				// An SSO ADMIN: the tier that actually reaches this branch.
				admin := ssoSession(t, "sub-second-admin", "admin@corp.example", oidc.RoleAdmin)
				w := doSSO(t, f.srv, http.MethodPost, "/api/v1/approvals/"+id.String()+"/"+verb, admin, `{}`)
				if w.Code != http.StatusServiceUnavailable {
					t.Fatalf("%s: status = %d, want 503 — an unreadable run must never read as a pass; body=%s",
						verb, w.Code, w.Body.String())
				}
				if !strings.Contains(w.Body.String(), wantMsg) {
					t.Errorf("%s: body = %s, want the fail-closed message %q", verb, w.Body.String(), wantMsg)
				}
				if strings.Contains(w.Body.String(), localMsg) {
					t.Errorf("%s: the LOCAL-MODE 503 answered instead of the fail-closed one — this test is "+
						"pinning the wrong branch: %s", verb, w.Body.String())
				}
				// PENDING -> decided is one-way, so the refusal has to land
				// BEFORE Decide().
				f.approval.mu.Lock()
				state := f.approval.byID[id].State
				f.approval.mu.Unlock()
				if state != types.ApprovalPending {
					t.Errorf("%s: approval state = %q after a refused decision, want PENDING", verb, state)
				}
			}
		})
	}
}

// TestSecondHuman_EmptyCreatedByPasses asserts the documented behaviour for a run
// with no human creator (system-created follow-on runs): the rule cannot apply,
// and "closed" here would mean refusing every decision on a run nobody authored,
// which no second human can ever unblock.
//
// WHAT THIS DOES NOT DO, stated because the finding asked for the
// `run.CreatedBy == ""` CLAUSE to be pinned and it cannot be. That clause only
// changes the answer when the DECIDER's principal is also "" — otherwise
// `run.CreatedBy != principal` is already true for an empty creator and the
// second half of the same condition carries it. And an empty principal is
// unreachable here: probed all three shapes through actorFromRequest, and every
// one that yields "" (no identity, an OIDC human with an empty sub) resolves to
// system/admin-token, which the gate bypasses at the top before this line. So
// the clause is REDUNDANT-BUT-DEFENSIVE today, no behavioural test can
// distinguish its presence, and the counterfactual for it correctly does not
// fire — there is nothing to break. Deleting it is not the lesson: it is the
// guard that keeps this line correct if an empty principal ever becomes
// reachable.
//
// What IS pinned is the behaviour plus a load-bearing control: the same fixture
// still refuses when created_by IS the decider, so this is a scoped pass-through
// rather than a dead gate.
func TestSecondHuman_EmptyCreatedByPasses(t *testing.T) {
	decide := func(t *testing.T, createdBy string) int {
		t.Helper()
		t.Setenv(envEgressSecondHuman, "1")
		f := newScopeFixture(t)
		f.store.mu.Lock()
		r := f.store.runs[f.runID]
		r.CreatedBy = createdBy
		f.store.runs[f.runID] = r
		f.store.mu.Unlock()
		id := f.seedEgress(t, "registry.npmjs.org")
		sess := ssoSession(t, "sub-decider", "decider@corp.example", oidc.RoleAdmin)
		return doSSO(t, f.srv, http.MethodPost, "/api/v1/approvals/"+id.String()+"/approve", sess, `{}`).Code
	}

	if got := decide(t, ""); got != http.StatusOK {
		t.Errorf("empty created_by: status = %d, want 200 — a run with no human creator has no second human "+
			"to require, and refusing would be unblockable by anyone", got)
	}
	// The control: the SAME fixture still refuses when the decider IS the
	// creator, so the arm above is a scoped pass-through and not a dead gate.
	if got := decide(t, "sub-decider"); got != http.StatusForbidden {
		t.Errorf("decider IS the creator: status = %d, want 403 — the pass-through above must be scoped to an "+
			"EMPTY created_by, not to everything", got)
	}
}
