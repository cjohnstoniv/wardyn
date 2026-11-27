// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/assistant"
	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// assistantPGEnv is the unit-admin tree (newUnitAdminEnv) with the assistant on for compose and review, run
// history counts on, and providers a and b ready on model m1.
type assistantPGEnv struct {
	*unitAdminEnv
	bind types.AssistantBinding
}

func newAssistantPGEnv(t *testing.T, e *unitAdminEnv) *assistantPGEnv {
	t.Helper()
	site, bind := assistantTwoProviders()
	site.Assistant.Review = true
	site.Assistant.RunHistoryCounts = true
	site.Assistant.Providers[0].Review, site.Assistant.Providers[1].Review = true, true
	if _, err := e.pg.PutSiteConfig(context.Background(), site); err != nil {
		t.Fatalf("seed the assistant settings: %v", err)
	}
	// The audit chain marks a dry run's rows, as cmd/wardynd's does.
	e.srv.cfg.Audit = audit.DryRunRecorder{Inner: e.h.audit}
	return &assistantPGEnv{unitAdminEnv: e, bind: bind}
}

// session is a live session of person on the target, with the run that hosts it.
func (a *assistantPGEnv) session(person, role, userType string, purpose types.AssistantPurpose, kind, key string) (types.AssistantSession, uuid.UUID) {
	a.t.Helper()
	ctx, now := context.Background(), time.Now().UTC()
	// One live session per person and purpose: a later case of the same person replaces the earlier one.
	if old, err := a.pg.LiveAssistantSession(ctx, person, purpose); err == nil {
		if _, err := a.pg.EndAssistantSession(ctx, old.ID, types.AssistantEndPerson); err != nil {
			a.t.Fatal(err)
		}
	}
	run, err := a.pg.CreateRun(ctx, types.AgentRun{ID: uuid.New(), CreatedAt: now, UpdatedAt: now, CreatedBy: person, Agent: "claude-code",
		Task: "assistant", ConfinementClass: types.CC2, State: types.RunRunning, RunnerTarget: "docker"})
	if err != nil {
		a.t.Fatalf("seed the assistant's run: %v", err)
	}
	s, err := a.pg.CreateAssistantSession(ctx, types.AssistantSession{ID: uuid.New(), RunID: &run.ID, Principal: person,
		Email: person + "@corp.example", Role: role, UserType: userType, Groups: []string{}, Purpose: purpose, TargetKind: kind,
		TargetKey: key, Binding: a.bind, State: types.AssistantSessionReady, CreatedAt: now})
	if err != nil {
		a.t.Fatalf("seed the session: %v", err)
	}
	return s, run.ID
}

// tool calls name on the session's run, as the proxy forwards it.
func (a *assistantPGEnv) tool(s types.AssistantSession, run uuid.UUID, name, args string) *httptest.ResponseRecorder {
	a.t.Helper()
	return assistantToolCall(a.t, a.srv, run, name, s.ID, args)
}

// assistantDraftArgs is a tool's arguments: the target kind and the draft.
func assistantDraftArgs(kind string, draft any) string {
	raw, _ := json.Marshal(map[string]any{"target_kind": kind, "draft": draft})
	return string(raw)
}

// assistantUnitDraft is a unit's save body.
func assistantUnitDraft(name string, parent uuid.UUID, domains ...string) map[string]any {
	return map[string]any{"name": name, "parent_id": parent, "overlay": map[string]any{"allowed_domains": domains}}
}

func assistantDecode(t *testing.T, w *httptest.ResponseRecorder, v any) {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d %s, want 200", w.Code, w.Body)
	}
	if err := json.Unmarshal(w.Body.Bytes(), v); err != nil {
		t.Fatalf("decode %s: %v", w.Body, err)
	}
}

// assistantDBDigest is a hash of every table's rows but assistant_session_events (migration 0401), which
// assistantEventRows reads apart: any write anywhere else changes it,
// whatever store method or querier made it.
func assistantDBDigest(t *testing.T, pool *pgxpool.Pool) map[string]string {
	t.Helper()
	ctx := context.Background()
	rows, err := pool.Query(ctx, `SELECT table_name FROM information_schema.tables
		WHERE table_schema = current_schema() AND table_type = 'BASE TABLE' AND table_name <> 'assistant_session_events'`)
	if err != nil {
		t.Fatal(err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string]string, len(names))
	for _, n := range names {
		var d string
		q := fmt.Sprintf(`SELECT md5(coalesce(string_agg(x::text, E'\n' ORDER BY x::text), '')) FROM %s x`, pgx.Identifier{n}.Sanitize())
		if err := pool.QueryRow(ctx, q).Scan(&d); err != nil {
			t.Fatalf("digest %s: %v", n, err)
		}
		out[n] = d
	}
	return out
}

// assistantEventRows is every assistant event row, session and seq to kind.
func assistantEventRows(t *testing.T, pool *pgxpool.Pool) map[string]string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT session_id::text || '/' || seq::text, kind FROM assistant_session_events`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, kind string
		if err := rows.Scan(&k, &kind); err != nil {
			t.Fatal(err)
		}
		out[k] = kind
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func assistantChangeRows(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM governance_changes`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Every tool in the registry, on a new template's session and on a unit admin's session over their unit,
// answers 200 and writes nothing: every table but assistant_session_events hashes the same before and after,
// every event row it adds is this session's, a tool_record per call and draft's one proposal, and no audit row
// is written. draft on a widening the save
// would hold keeps it, held, and no governance_changes row appears.
func TestAssistantToolsWriteNothing_PG(t *testing.T) {
	a := newAssistantPGEnv(t, newUnitAdminEnv(t, false))
	widen := assistantUnitDraft("ua-a1", a.a.ID, "pypi.org", "github.com", "npmjs.org")
	tplDraft := map[string]any{"scope": "person", "name": "t1", "document": map[string]any{"api_version": "v1", "kind": "Template",
		"intent": map[string]any{"inline_policy": map[string]any{"allowed_domains": []string{"pypi.org"}, "min_confinement_class": "CC2"}}}}
	parts := func(m map[string]any) string {
		var ps []map[string]any
		for _, k := range slices.Sorted(maps.Keys(m)) {
			ps = append(ps, map[string]any{"section": k, "value": m[k]})
		}
		raw, _ := json.Marshal(ps)
		return string(raw)
	}
	cases := []struct {
		name, person, kind, key string
		draft                   map[string]any
	}{
		{"new template", "sub-ua-member", types.AssistantTargetTemplate, "", tplDraft},
		{"unit admin on their unit", uaSubAdminA, types.AssistantTargetUnit, a.a1.ID.String(), widen},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, run := a.session(c.person, oidc.RoleUser, types.UserTypeStandard, types.AssistantCompose, c.kind, c.key)
			before, changes, audits := assistantDBDigest(t, a.pool), assistantChangeRows(t, a.pool), len(a.h.audit.snapshot())
			eventsBefore := assistantEventRows(t, a.pool)
			for _, name := range slices.Sorted(maps.Keys(assistantToolRegistry())) {
				args := assistantDraftArgs(c.kind, c.draft)
				switch name {
				case types.AssistantToolRead:
					args = `{"kind":"target"}`
				case types.AssistantToolDraft:
					args = `{"target_kind":"` + c.kind + `","parts":` + parts(c.draft) + `}`
				case types.AssistantToolBuildFiles:
					args = `{}`
				}
				w := a.tool(s, run, name, args)
				if w.Code != http.StatusOK {
					t.Errorf("%s = %d %s, want 200", name, w.Code, w.Body)
					continue
				}
				if name == types.AssistantToolDraft && c.kind == types.AssistantTargetUnit {
					var p assistantProposal
					_ = json.Unmarshal(w.Body.Bytes(), &p)
					if !p.Held || !strings.Contains(w.Body.String(), "npmjs.org") {
						t.Errorf("draft on a held widening = %s, want the widening kept and held", w.Body)
					}
				}
			}
			if after := assistantDBDigest(t, a.pool); !maps.Equal(before, after) {
				for k := range before {
					if before[k] != after[k] {
						t.Errorf("a tool wrote to %s", k)
					}
				}
			}
			if n := assistantChangeRows(t, a.pool); n != changes {
				t.Errorf("governance_changes rows = %d, want %d: draft proposes, it never queues", n, changes)
			}
			if n := len(a.h.audit.snapshot()); n != audits {
				t.Errorf("audit rows = %d, want %d", n, audits)
			}
			kinds := map[string]int{}
			for k, kind := range assistantEventRows(t, a.pool) {
				if _, old := eventsBefore[k]; old {
					continue
				}
				if !strings.HasPrefix(k, s.ID.String()+"/") {
					t.Errorf("a tool appended event %s (%s) to another session", k, kind)
				}
				kinds[kind]++
			}
			calls := len(assistantToolRegistry())
			if kinds[types.AssistantEventToolRecord] != calls || kinds[types.AssistantEventProposal] != 1 || len(kinds) != 2 {
				t.Errorf("events added = %v, want %d %s and 1 %s", kinds, calls, types.AssistantEventToolRecord, types.AssistantEventProposal)
			}
		})
	}
}

// A session on a new unit is held to the unit tier POST /governance/units sits behind, and its draft's parent to
// the caller's scope, as the save checks them: a member is refused, a unit admin naming a parent outside their
// units is told the draft is invalid, one naming a parent they govern passes; moving an existing unit under a
// parent outside their units is invalid too.
func TestAssistantNewUnitNeedsTheUnitTier_PG(t *testing.T) {
	a := newAssistantPGEnv(t, newUnitAdminEnv(t, false))
	type validation struct {
		Valid  bool                  `json:"valid"`
		Errors []assistantFieldError `json:"errors"`
	}
	s, run := a.session("sub-ua-member", oidc.RoleUser, types.UserTypeStandard, types.AssistantCompose, types.AssistantTargetUnit, "")
	wantReason(t, a.tool(s, run, types.AssistantToolValidate, assistantDraftArgs(types.AssistantTargetUnit, assistantUnitDraft("ua-new", a.a.ID))),
		http.StatusForbidden, "unit_scope_denied")
	for _, c := range []struct {
		name, person, key string
		parent            uuid.UUID
		valid             bool
	}{
		{"new unit under a parent outside the caller's units", uaSubAdminA1, "", a.b.ID, false},
		{"new unit under a parent the caller governs", uaSubAdminA, "", a.a1.ID, true},
		{"an existing unit moved outside the caller's units", uaSubAdminA1, a.a1.ID.String(), a.b.ID, false},
	} {
		s, run := a.session(c.person, oidc.RoleUser, types.UserTypeStandard, types.AssistantCompose, types.AssistantTargetUnit, c.key)
		var v validation
		assistantDecode(t, a.tool(s, run, types.AssistantToolValidate, assistantDraftArgs(types.AssistantTargetUnit,
			assistantUnitDraft("ua-a1", c.parent, "pypi.org"))), &v)
		if v.Valid != c.valid {
			t.Errorf("%s: valid = %v (%+v), want %v", c.name, v.Valid, v.Errors, c.valid)
		}
		if !c.valid && (len(v.Errors) == 0 || !strings.Contains(v.Errors[0].Message, "outside the units you govern")) {
			t.Errorf("%s: errors = %+v, want the parent named as outside the caller's units", c.name, v.Errors)
		}
	}
}

// The person routes check a unit target as PUT /governance/units/{id} does, on both routes: a member with no
// grant gets the tier refusal (its authz row marked dry_run), a unit admin of the unit passes, a unit admin of a
// sibling gets unit_scope_denied. A template the caller may not read and a change they may not see answer as
// GET /templates/{id} and GET /governance/changes/{id} answer that caller, byte for byte.
func TestAssistantPersonRouteTargets_PG(t *testing.T) {
	a := newAssistantPGEnv(t, newUnitAdminEnv(t, false))
	unitBody := assistantChecksBody(types.AssistantTargetUnit, a.a1.ID.String(), json.RawMessage(a.restate(a.a1, nil)))
	routes := []string{"/api/v1/assistant/checks", "/api/v1/assistant/replay"}
	for _, path := range routes {
		wantReason(t, a.call(a.member, http.MethodPost, path, unitBody), http.StatusForbidden, "unit_scope_denied")
		if w := a.call(a.adminA, http.MethodPost, path, unitBody); w.Code != http.StatusOK {
			t.Errorf("%s by the unit's admin = %d %s, want 200", path, w.Code, w.Body)
		}
		wantReason(t, a.call(a.adminB, http.MethodPost, path, unitBody), http.StatusForbidden, "unit_scope_denied")
	}
	marked := false
	for _, ev := range a.audits("authz.denied") {
		marked = marked || strings.Contains(string(ev.Data), `"dry_run":true`)
	}
	if !marked {
		t.Error("no authz.denied row carries dry_run: true")
	}

	now := time.Now().UTC()
	tpl, err := a.pg.CreateTemplate(context.Background(), types.RunTemplate{ID: uuid.New(), Kind: types.TemplateKindTemplate, Name: "private",
		Owner: types.TemplateOwner{Scope: types.TemplateScopePerson, Person: "sub-ua-other"}, Revision: 1, CreatedAt: now, UpdatedAt: now,
		CreatedBy: "sub-ua-other", UpdatedBy: "sub-ua-other", Document: json.RawMessage(`{"api_version":"v1","kind":"Template","intent":{}}`)}, nil)
	if err != nil {
		t.Fatalf("seed a template: %v", err)
	}
	ch := a.heldOn(a.b)
	for _, c := range []struct {
		name, kind, target, direct string
		who                        *http.Cookie
	}{
		{"a template another person owns", types.AssistantTargetTemplate, tpl.ID.String(), "/api/v1/templates/" + tpl.ID.String(), a.member},
		{"a change outside the caller's units", types.AssistantTargetChange, ch.ID.String(), "/api/v1/governance/changes/" + ch.ID.String(), a.adminA1},
	} {
		direct := a.call(c.who, http.MethodGet, c.direct, "")
		if direct.Code == http.StatusOK {
			t.Fatalf("%s: GET %s = 200, want a refusal to compare with", c.name, c.direct)
		}
		for _, path := range routes {
			w := a.call(c.who, http.MethodPost, path, assistantChecksBody(c.kind, c.target, map[string]any{}))
			if w.Code != direct.Code || w.Body.String() != direct.Body.String() {
				t.Errorf("%s on %s = %d %s, want GET's %d %s", c.name, path, w.Code, w.Body, direct.Code, direct.Body)
			}
		}
	}
}

// With the switch off, a unit admin's unit save is still held (unitWriteMode): the checks strip answers a review
// and approvals_needed equal to requiredApprovals' required, and explain gives the review reviewFor gives on the
// person's capped context with their caller.
func TestAssistantUnitAdminHeldWithTheSwitchOff_PG(t *testing.T) {
	a := newAssistantPGEnv(t, newUnitAdminEnv(t, false))
	widen := assistantUnitDraft("ua-a1", a.a.ID, "pypi.org", "github.com", "npmjs.org")
	var ans struct {
		Review          *changeReview `json:"review"`
		ApprovalsNeeded int           `json:"approvals_needed"`
	}
	assistantDecode(t, a.call(a.adminA, http.MethodPost, "/api/v1/assistant/checks",
		assistantChecksBody(types.AssistantTargetUnit, a.a1.ID.String(), widen)), &ans)
	required, _, err := a.srv.requiredApprovals(context.Background(), govKindUnit, a.a1.ID.String())
	if err != nil {
		t.Fatal(err)
	}
	if ans.Review == nil || required < 1 || ans.ApprovalsNeeded != required {
		t.Errorf("checks = review %v, approvals %d; want a review and %d (>= 1)", ans.Review, ans.ApprovalsNeeded, required)
	}

	s, run := a.session(uaSubAdminA, oidc.RoleUser, types.UserTypeStandard, types.AssistantCompose, types.AssistantTargetUnit, a.a1.ID.String())
	var got struct {
		Review json.RawMessage `json:"review"`
	}
	assistantDecode(t, a.tool(s, run, types.AssistantToolExplain, assistantDraftArgs(types.AssistantTargetUnit, widen)), &got)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	tctx, release, err := a.srv.assistantToolCtx(r, assistantSnapshotOfSession(s))
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	draft, _ := json.Marshal(widen)
	want, err := a.srv.reviewFor(tctx, govKindUnit, a.a1.ID.String(), nil, draft, reviewCaller{unitAdmin: true})
	if err != nil {
		t.Fatal(err)
	}
	if wantRaw, _ := json.Marshal(want); string(got.Review) != string(wantRaw) {
		t.Errorf("explain's review = %s, want reviewFor's %s", got.Review, wantRaw)
	}
}

// A super admin's session is judged at security-admin reach: a draft that sends a mount's host path is refused
// as for any caller below the super admin tier, through validate and explain, where reviewFor with super: true
// would accept it. The host path never appears in an answer.
func TestAssistantCappedSuperAdminHostPath_PG(t *testing.T) {
	e, _, standalone, _ := newUnitAdminSecretEnv(t)
	a := newAssistantPGEnv(t, e)
	const person = "sub-assist-super"
	if err := a.pg.PutPrincipalGroups(context.Background(), person, "", person+"@corp.example", []string{}, false, time.Time{}); err != nil {
		t.Fatal(err)
	}
	draft := map[string]any{"name": standalone.Name, "ceiling": map[string]any{"allowed_domains": standalone.Ceiling.AllowedDomains,
		"min_confinement_class": "CC2", "workspace_mounts": []map[string]any{{"source": pinMountSrc, "target": pinTgtA}}}}
	s, run := a.session(person, oidc.RoleAdmin, types.UserTypeStandard, types.AssistantCompose, types.AssistantTargetUnit, standalone.ID.String())
	for _, name := range []string{types.AssistantToolValidate, types.AssistantToolExplain} {
		var v struct {
			Valid  bool                  `json:"valid"`
			Errors []assistantFieldError `json:"errors"`
		}
		w := a.tool(s, run, name, assistantDraftArgs(types.AssistantTargetUnit, draft))
		assistantDecode(t, w, &v)
		if v.Valid || len(v.Errors) == 0 || !strings.Contains(v.Errors[0].Message, "only a super admin may set a mount's host path") {
			t.Errorf("%s = %s, want the host path refused at security-admin reach", name, w.Body)
		}
		if strings.Contains(w.Body.String(), pinMountSrc) {
			t.Errorf("%s answered the host path: %s", name, w.Body)
		}
	}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	tctx, release, err := a.srv.assistantToolCtx(r, assistantSnapshotOfSession(s))
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	raw, _ := json.Marshal(draft)
	if _, err := a.srv.reviewFor(tctx, govKindUnit, standalone.ID.String(), nil, raw, reviewCaller{super: true}); err != nil {
		t.Errorf("reviewFor with super: true = %v, want it to accept the host path the tools refuse", err)
	}
}

// impact's answer, from the tool itself on a unit session, carries counts only: no id, address or person label.
func TestAssistantToolImpactCarriesCountsOnly_PG(t *testing.T) {
	a := newAssistantPGEnv(t, newUnitAdminEnv(t, false))
	treeMember(t, a.govEnv, a.a1, types.CapabilitySubjectUser, "sub-ua-impact@corp.example", 1, false)
	// A live run in the unit, so a planted run list would carry its id.
	live := uuid.New()
	if _, err := a.pg.CreateRun(context.Background(), types.AgentRun{ID: live, CreatedAt: time.Now(), CreatedBy: "sub-ua-impact", Agent: "claude-code",
		ConfinementClass: types.CC2, State: types.RunRunning, RunnerTarget: "docker", Task: "t", Title: "impact canary",
		SPIFFEID: "spiffe://wardyn.local/agent-run/" + live.String(), SandboxRef: "wardyn-agent-" + live.String(), UnitID: &a.a1.ID}); err != nil {
		t.Fatal(err)
	}
	s, run := a.session(uaSubAdminA, oidc.RoleUser, types.UserTypeStandard, types.AssistantCompose, types.AssistantTargetUnit, a.a1.ID.String())
	w := a.tool(s, run, types.AssistantToolImpact, assistantDraftArgs(types.AssistantTargetUnit, assistantUnitDraft("ua-a1", a.a.ID, "pypi.org")))
	if w.Code != http.StatusOK {
		t.Fatalf("impact = %d %s", w.Code, w.Body)
	}
	var counts struct {
		LiveRunsTotal int `json:"live_runs_total"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &counts); err != nil || counts.LiveRunsTotal < 1 {
		t.Errorf("impact = %s (%v), want live_runs_total >= 1 for the run in the narrowed unit", w.Body, err)
	}
	if strings.Contains(w.Body.String(), "impact canary") {
		t.Errorf("impact = %s, want no run title", w.Body)
	}
	leak := regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}|@|Person `)
	if leak.Match(w.Body.Bytes()) {
		t.Errorf("impact = %s, want counts only", w.Body)
	}
}

// The type and the email reach every tool: a person whose only unit membership is keyed on their user type reads
// that unit, and a user_type deny grant on a host drops that host from draft and from the checks strip.
func TestAssistantUserTypeReachesTheTools_PG(t *testing.T) {
	a := newAssistantPGEnv(t, newUnitAdminEnv(t, false))
	ctx := context.Background()
	if _, err := a.pg.CreateUserType(ctx, types.UserType{ID: "contractor", Name: "Contractor"}); err != nil {
		t.Fatal(err)
	}
	unit := treeChild(t, a.govEnv, uaUnitBody("ua-contractors", a.a.ID, uaDomains("pypi.org"), nil))
	treeMember(t, a.govEnv, unit, types.CapabilitySubjectUserType, "contractor", 1, false)
	if _, err := a.pg.UpsertCapabilityGrant(ctx, types.CapabilityGrant{ID: uuid.New(), SubjectType: types.CapabilitySubjectUserType,
		Subject: "contractor", Capability: capEgressHost, Value: "blocked.example", Effect: types.CapabilityDeny, CreatedBy: "seed"}); err != nil {
		t.Fatal(err)
	}
	const person = "sub-ua-contractor"
	s, run := a.session(person, oidc.RoleUser, "contractor", types.AssistantCompose, types.AssistantTargetTemplate, "")
	var unitRead struct {
		Name string `json:"name"`
	}
	assistantDecode(t, a.tool(s, run, types.AssistantToolRead, `{"kind":"unit"}`), &unitRead)
	if unitRead.Name != unit.Name {
		t.Errorf("read unit = %q, want %q, the unit keyed on the person's user type", unitRead.Name, unit.Name)
	}
	blocked := assistant.Dropped{Host: "blocked.example", Code: assistant.DropNotGranted, AdminChangeApprovals: 0}
	doc := map[string]any{"api_version": "v1", "kind": "Template",
		"intent": map[string]any{"inline_policy": map[string]any{"allowed_domains": []string{"pypi.org", "blocked.example"}, "min_confinement_class": "CC2"}}}
	partsRaw, _ := json.Marshal([]map[string]any{{"section": "document", "value": doc}})
	var p assistantProposal
	assistantDecode(t, a.tool(s, run, types.AssistantToolDraft, `{"target_kind":"template","parts":`+string(partsRaw)+`}`), &p)
	if !slices.Contains(p.Dropped, blocked) {
		t.Errorf("draft dropped %+v, want %+v", p.Dropped, blocked)
	}
	cookie := personSessionCookie(t, person, person+"@corp.example", "contractor", []string{})
	var ans struct {
		Dropped []assistant.Dropped `json:"dropped"`
	}
	assistantDecode(t, a.call(cookie, http.MethodPost, "/api/v1/assistant/checks", assistantChecksBody(types.AssistantTargetRun, "",
		map[string]any{"allowed_domains": []string{"pypi.org", "blocked.example"}})), &ans)
	if !slices.Contains(ans.Dropped, blocked) {
		t.Errorf("checks dropped %+v, want %+v", ans.Dropped, blocked)
	}
}

// The person routes take the role the request published: a security admin through a cli_access token, and one in
// the user view, are judged as members (the caller reviewFor gets is a unit admin's, the role user), though their
// fresh role is higher; a security admin's own session keeps the security tier.
func TestAssistantPersonCtxTakesThePublishedRole_PG(t *testing.T) {
	a := newAssistantPGEnv(t, newUnitAdminEnv(t, false))
	ctx := context.Background()
	const person, email = "sub-assist-sec", "sub-assist-sec@corp.example"
	if err := a.pg.PutPrincipalGroups(ctx, person, "", email, []string{}, false, time.Time{}); err != nil {
		t.Fatal(err)
	}
	const raw = apiTokenPrefix + "assistant-cli-access"
	exp := time.Now().Add(time.Hour)
	if _, err := a.pg.CreateAPIToken(ctx, types.APIToken{ID: uuid.New(), Principal: person, Email: email, Role: oidc.RoleSecurityAdmin,
		UserType: types.UserTypeStandard, GroupsTruncated: new(bool), Name: "cli", Kind: types.APITokenKindCLIAccess, ExpiresAt: &exp}, raw); err != nil {
		t.Fatal(err)
	}
	capture := func(serve func(http.Handler) http.Handler, r *http.Request) context.Context {
		var got context.Context
		serve(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = r.Context() })).ServeHTTP(httptest.NewRecorder(), r)
		if got == nil {
			t.Fatalf("%s never reached the handler", r.URL.Path)
		}
		return got
	}
	session := func(memberMode bool) *http.Cookie {
		payload, err := json.Marshal(oidc.Session{V: oidc.SessionCodecVersion, Sub: person, Email: email, Role: oidc.RoleSecurityAdmin,
			UserType: types.UserTypeStandard, Expiry: time.Now().UTC().Add(time.Hour), Groups: []string{}, MemberMode: memberMode})
		if err != nil {
			t.Fatal(err)
		}
		return signedSessionCookie(payload)
	}
	tokReq := httptest.NewRequest(http.MethodGet, "/token", nil)
	tokReq.Header.Set("Authorization", "Bearer "+raw)
	viewReq, ownReq := httptest.NewRequest(http.MethodGet, "/view", nil), httptest.NewRequest(http.MethodGet, "/own", nil)
	viewReq.AddCookie(session(true))
	ownReq.AddCookie(session(false))
	for _, c := range []struct {
		name   string
		direct context.Context
		role   string
		caller reviewCaller
	}{
		{"cli_access token", capture(func(next http.Handler) http.Handler {
			return a.srv.apiTokenAuth(next, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("the token fell through") }))
		}, tokReq), oidc.RoleUser, reviewCaller{unitAdmin: true}},
		{"user view", capture(a.srv.humanOrAdminAuth, viewReq), oidc.RoleUser, reviewCaller{unitAdmin: true}},
		{"own session", capture(a.srv.humanOrAdminAuth, ownReq), oidc.RoleSecurityAdmin, reviewCaller{}},
	} {
		w := httptest.NewRecorder()
		tctx, ok := a.srv.assistantPersonCtx(w, httptest.NewRequest(http.MethodPost, "/api/v1/assistant/checks", nil).WithContext(c.direct))
		if !ok {
			t.Fatalf("%s: refused %d %s", c.name, w.Code, w.Body)
		}
		if got := oidcRoleFromContext(tctx); got != c.role {
			t.Errorf("%s: role = %q, want %q", c.name, got, c.role)
		}
		if got := a.srv.assistantReviewCaller(tctx); got != c.caller {
			t.Errorf("%s: caller = %+v, want %+v", c.name, got, c.caller)
		}
	}
}
