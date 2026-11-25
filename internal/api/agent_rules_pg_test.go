// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/testutil"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The agent rules routes over Postgres (agent_rules.go): the write rules, the audit rows, the strict-audit
// door, the second-person switch and its break glass, and the seal a run's dispatch carries.

const (
	agentRulesPath  = "/api/v1/agent-rules"
	agentRulesBody  = `{"spec":{"denied_domains":["evil.example.com"]},"version":0}`
	agentRulesBody2 = `{"spec":{"denied_domains":["evil.example.com","worse.example.com"]},"inner_walls":{"sandbox_mode":"read-only"},"version":1}`
)

// agentRulesEnv is a governance environment with the second-person switch off, so a write applies at once.
func agentRulesEnv(t *testing.T) *govEnv {
	t.Helper()
	t.Setenv(envGovernanceSecondHuman, "false")
	return newGovEnvOn(t, throwawayPGPool(t))
}

func (e *govEnv) agentRulesList() []types.AgentRules {
	e.t.Helper()
	w := e.call(e.alice, http.MethodGet, agentRulesPath, "")
	var body struct {
		Rules []types.AgentRules `json:"rules"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &body) != nil || body.Rules == nil {
		e.t.Fatalf("GET /agent-rules = %d %s, want 200 with a rules list", w.Code, w.Body)
	}
	return body.Rules
}

func (e *govEnv) putAgentRules(c *http.Cookie, harness, body string) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.call(c, http.MethodPut, agentRulesPath+"/"+harness, body)
}

// TestAgentRulesRoutes_CRUD_PG: a create is version 1, a write names the version it replaces, a stale
// version is 409 and changes nothing, a delete names its version, and the list is never null.
func TestAgentRulesRoutes_CRUD_PG(t *testing.T) {
	e := agentRulesEnv(t)
	if got := e.agentRulesList(); len(got) != 0 {
		t.Fatalf("a fresh install lists %+v, want none", got)
	}
	w := e.putAgentRules(e.alice, "claude-code", agentRulesBody)
	var created types.AgentRules
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &created) != nil || created.Version != 1 || created.Harness != "claude-code" ||
		created.UpdatedBy != govSubAlice || created.Spec.DeniedDomains == nil {
		t.Fatalf("create = %d %s, want 200 at version 1 by alice", w.Code, w.Body)
	}
	if w := e.putAgentRules(e.alice, "claude-code", agentRulesBody); w.Code != http.StatusConflict || wireReason(t, w) != reasonAgentRulesVersionConflict {
		t.Errorf("a create over existing rules = %d %s, want 409 %s", w.Code, w.Body, reasonAgentRulesVersionConflict)
	}
	w = e.putAgentRules(e.alice, "claude-code", agentRulesBody2)
	var updated types.AgentRules
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &updated) != nil || updated.Version != 2 || updated.InnerWalls == nil ||
		updated.InnerWalls.SandboxMode != types.InnerSandboxReadOnly {
		t.Fatalf("update = %d %s, want 200 at version 2 with the inner walls", w.Code, w.Body)
	}
	if w := e.putAgentRules(e.alice, "claude-code", agentRulesBody2); w.Code != http.StatusConflict {
		t.Errorf("an update at a stale version = %d %s, want 409", w.Code, w.Body)
	}
	if got := e.agentRulesList(); len(got) != 1 || got[0].Version != 2 {
		t.Errorf("list = %+v, want one row at version 2", got)
	}
	for name, tc := range map[string]struct {
		query string
		want  int
	}{"no version": {"", http.StatusBadRequest}, "not a number": {"?version=x", http.StatusBadRequest}, "stale": {"?version=1", http.StatusConflict}} {
		if w := e.call(e.alice, http.MethodDelete, agentRulesPath+"/claude-code"+tc.query, ""); w.Code != tc.want {
			t.Errorf("delete, %s = %d %s, want %d", name, w.Code, w.Body, tc.want)
		}
	}
	if w := e.call(e.alice, http.MethodDelete, agentRulesPath+"/claude-code?version=2", ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s, want 204", w.Code, w.Body)
	}
	if w := e.call(e.alice, http.MethodDelete, agentRulesPath+"/claude-code?version=2", ""); w.Code != http.StatusNotFound {
		t.Errorf("delete of absent rules = %d %s, want 404", w.Code, w.Body)
	}
	if got := e.agentRulesList(); len(got) != 0 {
		t.Errorf("list after the delete = %+v, want none", got)
	}
}

// TestAgentRulesRoutes_Audited_PG: a write and a delete each leave a row with the version, spec and inner
// walls before and after.
func TestAgentRulesRoutes_Audited_PG(t *testing.T) {
	e := agentRulesEnv(t)
	e.putAgentRules(e.alice, "claude-code", agentRulesBody)
	e.putAgentRules(e.alice, "claude-code", agentRulesBody2)
	e.call(e.alice, http.MethodDelete, agentRulesPath+"/claude-code?version=2", "")
	writes := e.audits("agent_rules.write")
	if len(writes) != 2 {
		t.Fatalf("agent_rules.write rows = %d, want 2", len(writes))
	}
	first, second := auditData(t, writes[0]), auditData(t, writes[1])
	if writes[0].Target != "claude-code" || writes[0].Actor != govSubAlice || first["before_version"] != float64(0) || first["after_version"] != float64(1) || first["before"] != nil {
		t.Errorf("the create row = %s %+v, want claude-code by alice from version 0 to 1 with no before", writes[0].Target, first)
	}
	if second["before_version"] != float64(1) || second["after_version"] != float64(2) || second["before"] == nil || second["inner_walls_after"] == nil || second["inner_walls_before"] != nil {
		t.Errorf("the update row = %+v, want version 1 to 2 with both specs and the inner walls added", second)
	}
	dels := e.audits("agent_rules.delete")
	if len(dels) != 1 || dels[0].Target != "claude-code" || auditData(t, dels[0])["version"] != float64(2) {
		t.Errorf("agent_rules.delete rows = %+v, want one for claude-code at version 2", dels)
	}
}

// TestAgentRulesRoutes_MemberRefused_PG: a member reads and writes nothing.
func TestAgentRulesRoutes_MemberRefused_PG(t *testing.T) {
	e := agentRulesEnv(t)
	for _, c := range []struct{ method, path, body string }{
		{http.MethodGet, agentRulesPath, ""},
		{http.MethodPut, agentRulesPath + "/claude-code", agentRulesBody},
		{http.MethodDelete, agentRulesPath + "/claude-code?version=1", ""},
	} {
		if w := e.call(e.user, c.method, c.path, c.body); w.Code != http.StatusForbidden {
			t.Errorf("%s %s as a member = %d %s, want 403", c.method, c.path, w.Code, w.Body)
		}
	}
	if got := e.agentRulesList(); len(got) != 0 {
		t.Errorf("the refused write stored %+v", got)
	}
}

// TestAgentRulesWrite_RefusesWidening_PG: a rule that widens, or names a knob that is not enforced for it yet,
// is 422 agent_rules_invalid and stores nothing.
func TestAgentRulesWrite_RefusesWidening_PG(t *testing.T) {
	e := agentRulesEnv(t)
	for name, body := range map[string]string{
		"allow_all_egress true":     `{"spec":{"allow_all_egress":true},"version":0}`,
		"inner sandbox off":         `{"spec":{"denied_domains":["a.example"]},"inner_walls":{"sandbox_mode":"off"},"version":0}`,
		"inner approvals none":      `{"spec":{"denied_domains":["a.example"]},"inner_walls":{"approvals":"none"},"version":0}`,
		"an unknown sandbox mode":   `{"spec":{"denied_domains":["a.example"]},"inner_walls":{"sandbox_mode":"loose"},"version":0}`,
		"github capabilities":       `{"spec":{"github_capabilities":["code_read"]},"version":0}`,
		"azure devops capabilities": `{"spec":{"azure_devops_capabilities":["code_read"]},"version":0}`,
	} {
		if w := e.putAgentRules(e.alice, "claude-code", body); w.Code != http.StatusUnprocessableEntity || wireReason(t, w) != reasonAgentRulesInvalid {
			t.Errorf("%s = %d %s, want 422 %s", name, w.Code, w.Body, reasonAgentRulesInvalid)
		}
	}
	if got := e.agentRulesList(); len(got) != 0 {
		t.Errorf("a refused write stored %+v", got)
	}
}

// TestAgentRulesWrite_UnknownHarness_PG: rules name a harness from the catalogue, and "Your own tools" is not
// one.
func TestAgentRulesWrite_UnknownHarness_PG(t *testing.T) {
	e := agentRulesEnv(t)
	for _, h := range []string{"nonesuch", "none"} {
		if w := e.putAgentRules(e.alice, h, agentRulesBody); w.Code != http.StatusUnprocessableEntity || wireReason(t, w) != reasonAgentRulesHarnessUnknown {
			t.Errorf("PUT %s = %d %s, want 422 %s", h, w.Code, w.Body, reasonAgentRulesHarnessUnknown)
		}
	}
}

// TestAgentRulesWrite_RefusesEmpty_PG: rules that name nothing are refused; the answer is to delete them.
func TestAgentRulesWrite_RefusesEmpty_PG(t *testing.T) {
	e := agentRulesEnv(t)
	for _, body := range []string{`{"spec":{},"version":0}`, `{"spec":{},"inner_walls":{},"version":0}`} {
		w := e.putAgentRules(e.alice, "claude-code", body)
		if w.Code != http.StatusUnprocessableEntity || wireReason(t, w) != reasonAgentRulesInvalid || !strings.Contains(w.Body.String(), "Delete them instead") {
			t.Errorf("PUT %s = %d %s, want 422 %s asking to delete", body, w.Code, w.Body, reasonAgentRulesInvalid)
		}
	}
}

// TestAgentRulesWrite_StrictAuditRefusesWithoutIntent_PG: with strict audit on and the intent row unwritable,
// a write is refused 503 and changes nothing.
func TestAgentRulesWrite_StrictAuditRefusesWithoutIntent_PG(t *testing.T) {
	e := agentRulesEnv(t)
	e.srv.cfg.AuditStrict = &strictRecorder{err: errors.New("the store is down")}
	if w := e.putAgentRules(e.alice, "claude-code", agentRulesBody); w.Code != http.StatusServiceUnavailable || wireReason(t, w) != reasonAuditUnavailable {
		t.Errorf("PUT with the intent write failing = %d %s, want 503 %s", w.Code, w.Body, reasonAuditUnavailable)
	}
	e.srv.cfg.AuditStrict = nil
	if got := e.agentRulesList(); len(got) != 0 {
		t.Errorf("the refused write stored %+v", got)
	}
	if rows := e.audits("agent_rules.write"); len(rows) != 0 {
		t.Errorf("the handler ran and wrote %v", rows)
	}
}

// TestAgentRulesWrite_HeldUnderSecondHuman_PG: with the switch on, a person's PUT and DELETE are held (202)
// and change nothing until another person approves; a held delete of absent rules is 404 and at the wrong
// version 409 before anything is held; an approval after a direct change is stale.
func TestAgentRulesWrite_HeldUnderSecondHuman_PG(t *testing.T) {
	e := newGovEnv(t)
	ch := e.pending(e.putAgentRules(e.alice, "claude-code", agentRulesBody))
	if ch.TargetKind != govKindAgentRules || ch.TargetKey != "claude-code" || ch.Op != "create" {
		t.Errorf("held change %s/%s %s, want %s/claude-code create", ch.TargetKind, ch.TargetKey, ch.Op, govKindAgentRules)
	}
	if got := e.agentRulesList(); len(got) != 0 {
		t.Fatalf("a held write stored %+v", got)
	}
	if w := e.call(e.alice, http.MethodDelete, agentRulesPath+"/claude-code?version=1", ""); w.Code != http.StatusNotFound {
		t.Errorf("a held delete of absent rules = %d %s, want 404", w.Code, w.Body)
	}
	if w := e.call(e.alice, http.MethodPost, approvePath(ch.ID), ""); w.Code == http.StatusOK {
		t.Errorf("the proposer approved their own change: %d %s", w.Code, w.Body)
	}
	e.approve(ch.ID)
	got := e.agentRulesList()
	if len(got) != 1 || got[0].Version != 1 || got[0].UpdatedBy != govSubAlice {
		t.Fatalf("after the approval = %+v, want version 1 written as the proposer", got)
	}
	if rows := e.audits("agent_rules.write"); len(rows) != 1 || rows[0].Target != "claude-code" {
		t.Errorf("agent_rules.write rows after the approval = %+v, want one", rows)
	}
	if w := e.call(e.alice, http.MethodDelete, agentRulesPath+"/claude-code?version=7", ""); w.Code != http.StatusConflict || wireReason(t, w) != reasonAgentRulesVersionConflict {
		t.Errorf("a held delete at the wrong version = %d %s, want 409 %s", w.Code, w.Body, reasonAgentRulesVersionConflict)
	}
	if w := e.putAgentRules(e.alice, "claude-code", `{"spec":{"denied_domains":["x.example"]},"version":9}`); w.Code != http.StatusConflict {
		t.Errorf("a held write at the wrong version = %d %s, want 409", w.Code, w.Body)
	}
	del := e.pending(e.call(e.alice, http.MethodDelete, agentRulesPath+"/claude-code?version=1", ""))
	if del.Op != "delete" {
		t.Errorf("held change op = %s, want delete", del.Op)
	}
	// The admin token's direct write between the proposal and the approval makes the approval stale.
	if w := e.admin(http.MethodPut, agentRulesPath+"/claude-code", `{"spec":{"denied_domains":["y.example"]},"version":1}`); w.Code != http.StatusOK {
		t.Fatalf("the admin-token write = %d %s, want 200", w.Code, w.Body)
	}
	if w := e.call(e.bob, http.MethodPost, approvePath(del.ID), ""); w.Code != http.StatusConflict || wireReason(t, w) != reasonGovernanceChangeStale {
		t.Errorf("approving after a direct change = %d %s, want 409 %s", w.Code, w.Body, reasonGovernanceChangeStale)
	}
	if got := e.agentRulesList(); len(got) != 1 || got[0].Version != 2 {
		t.Errorf("the stale approval changed the rules: %+v", got)
	}
	// A held delete approved at the version it saw removes the rules.
	del = e.pending(e.call(e.alice, http.MethodDelete, agentRulesPath+"/claude-code?version=2", ""))
	e.approve(del.ID)
	if got := e.agentRulesList(); len(got) != 0 {
		t.Errorf("after the approved delete = %+v, want none", got)
	}
}

// TestAgentRulesWrite_BreakGlassIsAudited_PG: with the switch on, the admin token writes at once, leaving its
// agent_rules row and a bypass row; a personal API token's write is held.
func TestAgentRulesWrite_BreakGlassIsAudited_PG(t *testing.T) {
	e := newGovEnv(t)
	if w := e.admin(http.MethodPut, agentRulesPath+"/claude-code", agentRulesBody); w.Code != http.StatusOK {
		t.Fatalf("the admin-token PUT = %d %s, want 200", w.Code, w.Body)
	}
	if w := e.admin(http.MethodDelete, agentRulesPath+"/claude-code?version=1", ""); w.Code != http.StatusNoContent {
		t.Fatalf("the admin-token DELETE = %d %s, want 204", w.Code, w.Body)
	}
	if rows := e.audits("agent_rules.write"); len(rows) != 1 {
		t.Errorf("agent_rules.write rows = %d, want 1", len(rows))
	}
	if rows := e.audits("agent_rules.delete"); len(rows) != 1 {
		t.Errorf("agent_rules.delete rows = %d, want 1", len(rows))
	}
	bypass := e.audits("governance.change.bypass")
	if len(bypass) != 2 {
		t.Fatalf("bypass rows = %d, want one per write", len(bypass))
	}
	for _, ev := range bypass {
		if d := auditData(t, ev); d["reason"] != "admin_token_break_glass" || d["target_kind"] != govKindAgentRules || ev.Target != "claude-code" {
			t.Errorf("bypass row %s %+v, want admin_token_break_glass on agent_rules/claude-code", ev.Target, d)
		}
	}
	_, token := e.mintToken("sub-token-admin", "token@corp.example", oidc.RoleSecurityAdmin)
	if w := do(t, e.srv, http.MethodPut, agentRulesPath+"/claude-code", token, agentRulesBody); w.Code != http.StatusAccepted {
		t.Errorf("a personal API token's PUT = %d %s, want 202 held", w.Code, w.Body)
	}
}

// TestAgentRulesSeal_DeniesAndWithheldReassertedAtDispatch_PG: a rule denies a host and withholds a grant kind;
// a run that includes the harness is sealed with the host denied and the grant withheld, and the
// run.ceiling.reassert row says the agent rules acted.
func TestAgentRulesSeal_DeniesAndWithheldReassertedAtDispatch_PG(t *testing.T) {
	pool := throwawayPGPool(t)
	testutil.SetBoundaryNoLimits(t, pool)
	t.Setenv(envGovernanceSecondHuman, "false")
	e := newGovEnvOn(t, pool)
	ctx := context.Background()
	body := `{"spec":{"denied_domains":["evil.example.com"],"withheld_grants":[{"kind":"ssh_key"}]},"version":0}`
	if w := e.putAgentRules(e.alice, "claude-code", body); w.Code != http.StatusOK {
		t.Fatalf("PUT = %d %s", w.Code, w.Body)
	}
	w := e.call(e.user, http.MethodPost, "/api/v1/runs", `{"agent":"claude-code","task":"t"}`)
	var created createRunResponse
	if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &created) != nil {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	run, err := e.pg.GetRun(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	policy := types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com", "evil.example.com"}}
	layers := e.srv.sealRunPolicy(ctx, run, &policy, knobProvenance{})
	if !slices.Contains(policy.DeniedDomains, "evil.example.com") {
		t.Errorf("sealed denied %v, want the rule's evil.example.com", policy.DeniedDomains)
	}
	ssh := types.GrantSpec{Kind: types.GrantSSHKey, Scope: json.RawMessage(`{"host":"git.corp.example","key_secret_ref":"deploy-key"}`)}
	if !(withholding{layers: layers}).credentialWithheld(ssh) {
		t.Error("dispatch: the rule's withheld ssh_key was not withheld")
	}
	injections := []runner.InjectionGrant{componentRule("api.anthropic.com"), componentRule("evil.example.com")}
	var p dispatchParams
	var llm llmTransport
	var bedrock []string
	e.srv.reassertCeilingDenies(ctx, run, &policy, &injections, dispatchCeiling{resolved: true}, layers, nil, &p, map[string]string{}, &llm, &bedrock, nil)
	ev := findAudit(e.h.audit.snapshot(), run.ID, "run.ceiling.reassert", "success")
	if ev == nil || !strings.Contains(string(ev.Data), `"agent_rules":true`) {
		t.Errorf("run.ceiling.reassert = %v, want a row carrying agent_rules true", ev)
	}
	// The same run launched without the agent carries no such layer.
	if got := e.srv.agentRulesSealLayers(ctx, types.AgentRun{ID: uuid.New(), Agent: "codex-cli"}); len(got) != 0 {
		t.Errorf("a run of another agent is sealed by %+v, want no agent rules layer", got)
	}
}
