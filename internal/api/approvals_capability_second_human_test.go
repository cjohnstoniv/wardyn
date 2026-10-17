// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// seedAdminClassADO puts a pending Azure DevOps escalation for an admin-class
// capability on the fixture's run — the case the switch exists for — and
// widens the organisation's ceiling to admit it, so the decision under test is
// the four-eyes rule and not the ceiling's own refusal.
func seedAdminClassADO(t *testing.T, f *scopeFixture) uuid.UUID {
	t.Helper()
	row := adoEntraTestRow()
	row.Entra.CapabilityCeiling = append(row.Entra.CapabilityCeiling, adoscope.CapPolicyBypass)
	f.store.mu.Lock()
	f.store.siteCfg = adoSite(row)
	f.store.mu.Unlock()
	return seedADO(t, f, adoscope.CapPolicyBypass, false)
}

func approvalState(f *scopeFixture, id uuid.UUID) types.ApprovalState {
	f.approval.mu.Lock()
	defer f.approval.mu.Unlock()
	return f.approval.byID[id].State
}

// With the switch unset the run's creator keeps deciding their own ADO
// escalation: the opt-in changes nothing until it is turned on.
func TestCapabilitySecondHuman_OffByDefault(t *testing.T) {
	f := newADODecideFixture(t)
	creator := ssoSession(t, f.memberID, "owner@corp.example", oidc.RoleUser)
	id := seedADO(t, f, adoscope.CapPR, false)

	w := doSSO(t, f.srv, http.MethodPost, "/api/v1/approvals/"+id.String()+"/approve", creator, `{}`)
	if w.Code != http.StatusOK {
		t.Fatalf("switch off, creator decides: status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// The creator is refused on both verbs, the approval stays PENDING (the refusal
// must land before Decide(), which is one-way), and the answer is the same
// four-eyes reason egress uses.
func TestCapabilitySecondHuman_RefusesTheRunsCreator(t *testing.T) {
	for _, verb := range []string{"approve", "deny"} {
		t.Run(verb, func(t *testing.T) {
			t.Setenv(envCapabilitySecondHuman, "1")
			f := newADODecideFixture(t)
			creator := ssoSession(t, f.memberID, "owner@corp.example", oidc.RoleUser)
			id := seedADO(t, f, adoscope.CapPR, false)

			w := doSSO(t, f.srv, http.MethodPost, "/api/v1/approvals/"+id.String()+"/"+verb, creator, `{}`)
			if w.Code != http.StatusForbidden {
				t.Fatalf("creator self-%s: status = %d, want 403; body=%s", verb, w.Code, w.Body.String())
			}
			var body errorBody
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if body.Reason != "second_human_required" {
				t.Errorf("reason = %q, want second_human_required", body.Reason)
			}
			if st := approvalState(f, id); st != types.ApprovalPending {
				t.Fatalf("approval state = %q after a refused decision, want PENDING", st)
			}
		})
	}
}

// A security admin who is also the creator is refused too — the rule is about
// who decides, not their role — but a different security admin decides, admin-
// class capability included.
func TestCapabilitySecondHuman_AdmitsADifferentSecurityAdmin(t *testing.T) {
	t.Setenv(envCapabilitySecondHuman, "1")
	f := newADODecideFixture(t)
	f.store.mu.Lock()
	r := f.store.runs[f.runID]
	r.CreatedBy = "sub-sec-creator"
	f.store.runs[f.runID] = r
	f.store.mu.Unlock()
	id := seedAdminClassADO(t, f)

	self := ssoSession(t, "sub-sec-creator", "creator@corp.example", oidc.RoleSecurityAdmin)
	if w := doSSO(t, f.srv, http.MethodPost, "/api/v1/approvals/"+id.String()+"/approve", self, `{}`); w.Code != http.StatusForbidden {
		t.Fatalf("security-admin creator approves own escalation: status = %d, want 403; body=%s", w.Code, w.Body.String())
	}
	other := ssoSession(t, "sub-sec-other", "reviewer@corp.example", oidc.RoleSecurityAdmin)
	if w := doSSO(t, f.srv, http.MethodPost, "/api/v1/approvals/"+id.String()+"/approve", other, `{}`); w.Code != http.StatusOK {
		t.Fatalf("a different security admin approves: status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}

// The admin-token break-glass passes and is audited, with this switch's name.
func TestCapabilitySecondHuman_AdminTokenBreakGlass(t *testing.T) {
	t.Setenv(envCapabilitySecondHuman, "1")
	f := newADODecideFixture(t)
	id := seedAdminClassADO(t, f)

	w := do(t, f.srv, http.MethodPost, "/api/v1/approvals/"+id.String()+"/approve", adminToken, `{}`)
	if w.Code != http.StatusOK {
		t.Fatalf("admin-token break-glass: status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var rows []types.AuditEvent
	for _, ev := range f.audit() {
		if ev.Action == "approval.second_human.bypass" {
			rows = append(rows, ev)
		}
	}
	if len(rows) != 1 {
		t.Fatalf("bypass rows = %d, want exactly 1", len(rows))
	}
	ev := rows[0]
	if ev.ActorType != types.ActorSystem || ev.Actor != adminTokenPrincipal || ev.Outcome != "success" {
		t.Errorf("bypass row actor/outcome = %s/%s/%s", ev.ActorType, ev.Actor, ev.Outcome)
	}
	var data map[string]any
	_ = json.Unmarshal(ev.Data, &data)
	if data["switch"] != "WARDYN_CAPABILITY_SECOND_HUMAN" || data["reason"] != "admin_token_break_glass" {
		t.Errorf("bypass data = %v, want switch WARDYN_CAPABILITY_SECOND_HUMAN", data)
	}
}

// Each switch governs only its own kind: the egress switch leaves a creator's
// ADO escalation alone, and the capability switch leaves their egress alone.
func TestCapabilitySecondHuman_SwitchesAreIndependent(t *testing.T) {
	t.Run("egress switch does not govern ADO", func(t *testing.T) {
		t.Setenv(envEgressSecondHuman, "1")
		f := newADODecideFixture(t)
		creator := ssoSession(t, f.memberID, "owner@corp.example", oidc.RoleUser)
		id := seedADO(t, f, adoscope.CapPR, false)
		if w := doSSO(t, f.srv, http.MethodPost, "/api/v1/approvals/"+id.String()+"/approve", creator, `{}`); w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
		}
	})
	t.Run("capability switch does not govern egress", func(t *testing.T) {
		t.Setenv(envCapabilitySecondHuman, "1")
		f := newScopeFixture(t)
		creator := ssoSession(t, f.memberID, "member@corp.example", oidc.RoleAdmin)
		id := f.seedEgress(t, "registry.npmjs.org")
		if w := doSSO(t, f.srv, http.MethodPost, "/api/v1/approvals/"+id.String()+"/approve", creator, `{}`); w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
		}
	})
}

// Local mode authenticates nobody, so the capability switch cannot be enforced
// there either: every decision on an Azure DevOps escalation is a 503 naming the
// switch, and the approval stays PENDING. Pinned for both verbs, with the
// ceiling widened so the 503 is the rule under test and not the ceiling's 403.
func TestCapabilitySecondHuman_LocalModeRefusesTheSwitch(t *testing.T) {
	for _, verb := range []string{"approve", "deny"} {
		t.Run(verb, func(t *testing.T) {
			t.Setenv(envCapabilitySecondHuman, "1")
			srv, aap, runID := localCapabilityFixture(t)
			id := seedLocalADO(t, aap, runID)

			w := localDecide(t, srv, id, verb, "")
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("local mode + capability switch, %s: status = %d, want 503; body=%s", verb, w.Code, w.Body.String())
			}
			var body errorBody
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if body.Reason != reasonEgressSecondHumanLocalMode {
				t.Errorf("reason = %q, want %q", body.Reason, reasonEgressSecondHumanLocalMode)
			}
			if !strings.Contains(body.Error, envCapabilitySecondHuman) {
				t.Errorf("the 503 must name the capability switch; body=%s", w.Body.String())
			}
			aap.mu.Lock()
			st := aap.byID[id].State
			aap.mu.Unlock()
			if st != types.ApprovalPending {
				t.Fatalf("approval state = %q after the refusal, want PENDING", st)
			}
		})
	}
}

// localCapabilityFixture is a local-mode server whose one run is created by
// local:alice and whose Azure DevOps ceiling admits code_write and pr_create.
func localCapabilityFixture(t *testing.T) (*Server, *authzApprovals, uuid.UUID) {
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
	ast.runs[runID] = types.AgentRun{ID: runID, CreatedBy: "local:alice", State: types.RunRunning}
	ast.siteCfg = adoSite(adoEntraTestRow())
	ast.mu.Unlock()
	return srv, aap, runID
}

func seedLocalADO(t *testing.T, aap *authzApprovals, runID uuid.UUID) uuid.UUID {
	t.Helper()
	grant := uuid.New()
	scope, _ := json.Marshal(adoCapabilityScope{Lane: adoApprovalLane, ProviderID: "ado-row-1", Org: "contoso",
		GrantID: grant, Capability: string(adoscope.CapPR), Tool: "Azure DevOps", Cmd: "x"})
	id := uuid.New()
	aap.mu.Lock()
	aap.byID[id] = types.ApprovalRequest{ID: id, RunID: runID, Kind: types.ApprovalToolCall, GrantID: &grant,
		RequestedScope: scope, State: types.ApprovalPending, RequestedAt: time.Now().UTC()}
	aap.mu.Unlock()
	return id
}
