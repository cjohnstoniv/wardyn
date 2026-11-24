// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/assistant"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// assistantToolSpy counts the reads a tool makes: targetReads the target's own records (templates, a change, a
// unit's chain, a policy, a run), allReads those and the caller's unit, so a refused call can be shown to have read
// nothing. templateReads is the templates' reads alone: a call that ends its session also reads its own run to
// stop it. fresh answers the fresh role's sign-in read with a record, which with no role mapping derives the
// admin role, so the snapshot's role is the one that counts.
type assistantToolSpy struct {
	*assistantFake
	smu           sync.Mutex
	targetReads   int
	allReads      int
	templateReads int
	fresh         bool
}

func (f *assistantToolSpy) count(target bool) {
	f.smu.Lock()
	defer f.smu.Unlock()
	f.allReads++
	if target {
		f.targetReads++
	}
}

func (f *assistantToolSpy) ListTemplatesVisible(ctx context.Context, p string, g []string) ([]types.RunTemplate, error) {
	f.count(true)
	f.smu.Lock()
	f.templateReads++
	f.smu.Unlock()
	return f.assistantFake.ListTemplatesVisible(ctx, p, g)
}

func (f *assistantToolSpy) GetGovernanceChange(ctx context.Context, id uuid.UUID) (types.GovernanceChange, error) {
	f.count(true)
	return f.assistantFake.GetGovernanceChange(ctx, id)
}

func (f *assistantToolSpy) GetGovernanceUnitChain(ctx context.Context, id uuid.UUID) ([]types.GovernanceUnit, error) {
	f.count(true)
	return f.assistantFake.GetGovernanceUnitChain(ctx, id)
}

func (f *assistantToolSpy) GetPolicy(ctx context.Context, id uuid.UUID) (types.RunPolicy, error) {
	f.count(true)
	return f.assistantFake.GetPolicy(ctx, id)
}

func (f *assistantToolSpy) GetRun(ctx context.Context, id uuid.UUID) (types.AgentRun, error) {
	f.count(true)
	return f.assistantFake.GetRun(ctx, id)
}

func (f *assistantToolSpy) ResolveGovernanceUnit(ctx context.Context, users, groups []string, userType string) (*types.GovernanceUnit, types.CapabilitySubjectType, error) {
	f.count(false)
	return f.assistantFake.ResolveGovernanceUnit(ctx, users, groups, userType)
}

func (f *assistantToolSpy) GetPrincipalGroups(ctx context.Context, principal, portal string) (store.PrincipalGroups, error) {
	rec, err := f.assistantFake.GetPrincipalGroups(ctx, principal, portal)
	if f.fresh {
		return store.PrincipalGroups{Email: principal + "@corp.example", Groups: []string{}}, nil
	}
	return rec, err
}

// TemplateGroupAdminGroups answers no group: the template caller of a new template draft asks it.
func (f *assistantToolSpy) TemplateGroupAdminGroups(context.Context, []string) ([]string, error) {
	return nil, nil
}

func (f *assistantToolSpy) reads() int {
	f.smu.Lock()
	defer f.smu.Unlock()
	return f.targetReads
}

func (f *assistantToolSpy) all() int {
	f.smu.Lock()
	defer f.smu.Unlock()
	return f.allReads
}

// assistantToolCall posts one tool call on the run's token, as the proxy forwards it.
func assistantToolCall(t *testing.T, srv *Server, run uuid.UUID, name string, session uuid.UUID, args string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"session_id":"` + session.String() + `","args":` + args + `}`
	r := assistantRunRequest(http.MethodPost, "/api/v1/internal/assistant/tools/"+name, body, run)
	rc := chi.NewRouteContext()
	rc.URLParams.Add("name", name)
	r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rc))
	w := httptest.NewRecorder()
	srv.handleAssistantToolCall(w, r)
	return w
}

// assistantToolSession seeds a live session with a run for a fresh person, on a site where compose and review
// are on and ready.
func assistantToolSession(t *testing.T, purpose types.AssistantPurpose) (*assistantToolSpy, *Server, types.AssistantSession, uuid.UUID) {
	t.Helper()
	site, bind := assistantTwoProviders()
	site.Assistant.Review = true
	site.Assistant.Providers[0].Review, site.Assistant.Providers[1].Review = true, true
	f := &assistantToolSpy{assistantFake: newAssistantFake(site)}
	srv, _ := assistantSessionServer(t, f.assistantFake)
	srv.cfg.Store = f
	person := "sub-tools-" + uuid.NewString()
	run := f.seedRun(t, person)
	s := assistantLiveSession(person, &run, bind)
	if purpose == types.AssistantReview {
		change := uuid.New()
		f.changes[change] = types.GovernanceChange{ID: change, TargetKind: govKindRestriction, ProposedBy: person}
		s.Purpose, s.TargetKind, s.TargetKey = types.AssistantReview, types.AssistantTargetChange, change.String()
	}
	f.seed(s)
	return f, srv, s, run
}

func assistantToolNamesOf(t *testing.T, w *httptest.ResponseRecorder) []string {
	t.Helper()
	var list []map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("tool list = %d %s: %v", w.Code, w.Body, err)
	}
	var names []string
	for _, tool := range list {
		var n string
		_ = json.Unmarshal(tool["name"], &n)
		names = append(names, n)
		if len(tool["input_schema"]) == 0 {
			t.Errorf("tool %s has no input_schema: the relay reads the schema from that field", n)
		}
	}
	return names
}

// The tool list keys on the run token's run id as the turn poll does: a run with no session is 404. Both lists are
// non-empty, a review session's leaves out draft, and every name has the relay's shape.
func TestAssistantToolListServesInputSchema(t *testing.T) {
	_, srv, _, run := assistantToolSession(t, types.AssistantCompose)
	w := httptest.NewRecorder()
	srv.handleAssistantToolList(w, assistantRunRequest(http.MethodGet, "/api/v1/internal/assistant/tools", "", uuid.New()))
	wantReason(t, w, http.StatusNotFound, reasonAssistantSessionNotFound)

	w = httptest.NewRecorder()
	srv.handleAssistantToolList(w, assistantRunRequest(http.MethodGet, "/api/v1/internal/assistant/tools", "", run))
	compose := assistantToolNamesOf(t, w)
	_, rsrv, _, rrun := assistantToolSession(t, types.AssistantReview)
	w = httptest.NewRecorder()
	rsrv.handleAssistantToolList(w, assistantRunRequest(http.MethodGet, "/api/v1/internal/assistant/tools", "", rrun))
	review := assistantToolNamesOf(t, w)
	if len(compose) == 0 || len(review) == 0 {
		t.Fatalf("compose %v, review %v: neither list may be empty", compose, review)
	}
	if !slices.Contains(compose, types.AssistantToolDraft) || slices.Contains(review, types.AssistantToolDraft) {
		t.Errorf("compose %v, review %v: draft is a compose tool only", compose, review)
	}
	for _, n := range append(compose, review...) {
		if !types.AssistantToolNameValid(n) {
			t.Errorf("tool name %q does not match the relay's pattern", n)
		}
	}
}

// A call keys on the run's session: a run with none is 404; another session's id is 409; an unknown tool is 404;
// draft in a review session is 403. Every call that names the live session leaves a tool_record.
func TestAssistantToolCallRefusals(t *testing.T) {
	f, srv, s, run := assistantToolSession(t, types.AssistantCompose)
	read := `{"kind":"templates"}`
	wantReason(t, assistantToolCall(t, srv, uuid.New(), types.AssistantToolRead, s.ID, read), http.StatusNotFound, reasonAssistantSessionNotFound)
	wantReason(t, assistantToolCall(t, srv, run, types.AssistantToolRead, uuid.New(), read), http.StatusConflict, reasonAssistantSessionNotFound)
	wantReason(t, assistantToolCall(t, srv, run, "nosuchtool", s.ID, `{}`), http.StatusNotFound, reasonAssistantToolUnknown)
	if w := assistantToolCall(t, srv, run, types.AssistantToolRead, s.ID, read); w.Code != http.StatusOK {
		t.Fatalf("read templates = %d %s, want 200", w.Code, w.Body)
	}
	var names []string
	for _, e := range f.eventsOf(s.ID) {
		if e.Kind == types.AssistantEventToolRecord {
			var p struct {
				Name   string `json:"name"`
				OK     bool   `json:"ok"`
				DryRun bool   `json:"dry_run"`
			}
			_ = json.Unmarshal(e.Payload, &p)
			if !p.DryRun {
				t.Errorf("tool_record %s has no dry_run mark", e.Payload)
			}
			names = append(names, p.Name)
		}
	}
	if want := []string{"nosuchtool", types.AssistantToolRead}; !slices.Equal(names, want) {
		t.Errorf("tool records = %v, want %v (none for a call that named no live session)", names, want)
	}

	_, rsrv, rs, rrun := assistantToolSession(t, types.AssistantReview)
	wantReason(t, assistantToolCall(t, rsrv, rrun, types.AssistantToolDraft, rs.ID, `{"target_kind":"change","parts":[{"section":"name","value":"x"}]}`),
		http.StatusForbidden, reasonAssistantToolRefused)
}

// The purpose turned off between two calls: the second answers 409, reads nothing of the target, and ends the
// session as turned_off.
func TestAssistantToolTurnedOffBetweenCalls(t *testing.T) {
	f, srv, s, run := assistantToolSession(t, types.AssistantCompose)
	if w := assistantToolCall(t, srv, run, types.AssistantToolRead, s.ID, `{"kind":"templates"}`); w.Code != http.StatusOK {
		t.Fatalf("first call = %d %s, want 200", w.Code, w.Body)
	}
	// The target's reads alone: ending the session as turned_off stops its run, which reads that run.
	templates := func() int { f.smu.Lock(); defer f.smu.Unlock(); return f.templateReads }
	before := templates()
	if before == 0 {
		t.Fatal("the first call read no template, so the refused call's count proves nothing")
	}
	f.site.Assistant.Compose = false
	wantReason(t, assistantToolCall(t, srv, run, types.AssistantToolRead, s.ID, `{"kind":"templates"}`), http.StatusConflict, reasonAssistantOff)
	if got := templates(); got != before {
		t.Errorf("the refused call read the templates %d times", got-before)
	}
	if got := f.session(s.ID); got.Live() || got.EndReason != types.AssistantEndTurnedOff {
		t.Errorf("session = %s/%s, want ended as turned_off", got.State, got.EndReason)
	}
}

// A never-sent kind is refused by name; run history is absent while the organisation keeps it back.
func TestAssistantToolNeverSentAndRunHistoryOff(t *testing.T) {
	f, srv, s, run := assistantToolSession(t, types.AssistantCompose)
	f.site.Assistant.RunHistoryCounts = false
	wantReason(t, assistantToolCall(t, srv, run, types.AssistantToolRead, s.ID, `{"kind":"run_history"}`), http.StatusForbidden, reasonAssistantToolRefused)
	wantReason(t, assistantToolCall(t, srv, run, types.AssistantToolSimulate, s.ID, `{"target_kind":"template","draft":{}}`), http.StatusForbidden, reasonAssistantToolRefused)
	f.site.Assistant.NeverSentKinds = []string{govKindUnit}
	// A unit kind on the never-sent list also stops the session on its next call only when the target is a unit;
	// here the target is a template, so the unit read alone is refused.
	wantReason(t, assistantToolCall(t, srv, run, types.AssistantToolRead, s.ID, `{"kind":"unit"}`), http.StatusConflict, reasonAssistantKindNeverSent)
}

// A turn_end after two tool calls yields a suggestion naming exactly those two tools.
func TestAssistantToolSuggestionNamesTheToolRecords(t *testing.T) {
	f, srv, s, run := assistantToolSession(t, types.AssistantCompose)
	for _, c := range []struct{ name, args string }{{types.AssistantToolRead, `{"kind":"templates"}`}, {types.AssistantToolRead, `{"kind":"target"}`}} {
		if w := assistantToolCall(t, srv, run, c.name, s.ID, c.args); w.Code != http.StatusOK {
			t.Fatalf("%s = %d %s", c.name, w.Code, w.Body)
		}
	}
	w := httptest.NewRecorder()
	srv.handleAssistantPostEvent(w, assistantRunRequest(http.MethodPost, "/api/v1/internal/assistant/events",
		`{"session_id":"`+s.ID.String()+`","kind":"`+types.AssistantEventTurnEnd+`","payload":{}}`, run))
	if w.Code != http.StatusAccepted {
		t.Fatalf("turn_end = %d %s", w.Code, w.Body)
	}
	for _, e := range f.eventsOf(s.ID) {
		if e.Kind == types.AssistantEventSuggestion {
			var p struct {
				Tools []string `json:"tools"`
			}
			_ = json.Unmarshal(e.Payload, &p)
			if want := []string{types.AssistantToolRead, types.AssistantToolRead}; !slices.Equal(p.Tools, want) {
				t.Errorf("suggestion tools = %v, want %v", p.Tools, want)
			}
			return
		}
	}
	t.Error("no suggestion event after turn_end")
}

// A result over 64 KiB is cut and marked truncated; every string passes the session's letters.
func TestAssistantToolResultCapAndLetters(t *testing.T) {
	letters := assistant.LettersFromMap(map[string]string{"Person A": "pat@corp.example"})
	raw, ref := assistantToolResult(map[string]any{"who": "ask pat@corp.example", "n": 3}, letters)
	if ref != nil || strings.Contains(string(raw), "pat@corp.example") || !strings.Contains(string(raw), "Person A") {
		t.Errorf("lettered result = %s, want the address replaced by its letter", raw)
	}
	raw, ref = assistantToolResult(map[string]any{"big": strings.Repeat("x", 70<<10)}, letters)
	if ref != nil || len(raw) > assistantToolResultMax {
		t.Fatalf("capped result is %d bytes", len(raw))
	}
	var out struct {
		Truncated bool `json:"truncated"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || !out.Truncated {
		t.Errorf("a result over the cap = %.80s, want truncated: true", raw)
	}
}

// assistantStoredTemplate is a saved template of person whose document still carries a repository URL with a
// token in it (a row from before the save refused one) and a mount's host path in its inline policy.
func assistantStoredTemplate(person string) types.RunTemplate {
	doc := `{"api_version":"v1","kind":"Template","intent":{"repo_url":"https://x-access-token:TOKEN-CANARY-7781@git.example/o/r.git",` +
		`"inline_policy":{"allowed_domains":["pypi.org"],"min_confinement_class":"CC2",` +
		`"workspace_mounts":[{"source":"/srv/template-mount-canary","target":"/home/agent/t"}]}}}`
	return types.RunTemplate{ID: uuid.New(), Owner: types.TemplateOwner{Scope: types.TemplateScopePerson, Person: person},
		Kind: types.TemplateKindTemplate, Name: "stored", Description: "ask pat@corp.example", Revision: 1, Document: json.RawMessage(doc)}
}

// draft on a saved template proposes only what the model proposed: a stored field it did not propose, the
// document with its token and host path among them, reaches neither the answer nor the proposal event, and read
// target sends neither. The template's description reaches the model through the session's letters.
func TestAssistantToolDraftNeverEchoesTheStoredTemplate(t *testing.T) {
	f, srv, s, run := assistantToolSession(t, types.AssistantCompose)
	tpl := assistantStoredTemplate(s.Principal)
	f.templates = []types.RunTemplate{tpl}
	s.TargetKey, s.PersonLetters = tpl.ID.String(), map[string]string{"Person A": "pat@corp.example"}
	f.seed(s)
	canaries := []string{"TOKEN-CANARY-7781", "/srv/template-mount-canary"}
	w := assistantToolCall(t, srv, run, types.AssistantToolDraft, s.ID, `{"target_kind":"template","parts":[{"section":"name","value":"renamed"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("draft = %d %s, want 200", w.Code, w.Body)
	}
	var p assistantProposal
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil || len(p.Parts) != 1 || p.Parts[0].Section != "name" {
		t.Errorf("parts = %+v (%v), want the one proposed section, name", p.Parts, err)
	}
	read := assistantToolCall(t, srv, run, types.AssistantToolRead, s.ID, `{"kind":"target"}`)
	var proposals []string
	for _, e := range f.eventsOf(s.ID) {
		if e.Kind == types.AssistantEventProposal {
			proposals = append(proposals, string(e.Payload))
		}
	}
	if len(proposals) != 1 {
		t.Fatalf("proposal events = %d, want 1", len(proposals))
	}
	for name, body := range map[string]string{"draft": w.Body.String(), "proposal event": proposals[0], "read target": read.Body.String()} {
		for _, c := range canaries {
			if strings.Contains(body, c) {
				t.Errorf("%s carries %q: %s", name, c, body)
			}
		}
	}
	if !strings.Contains(read.Body.String(), "Person A") || strings.Contains(read.Body.String(), "pat@corp.example") {
		t.Errorf("read target = %s, want the description's address as its letter", read.Body)
	}
}

// The template view is the template read's: no repository URL credential, no mount host path.
func TestAssistantTemplateViewIsTheReadView(t *testing.T) {
	srv := &Server{}
	for _, role := range []string{oidc.RoleUser, oidc.RoleSecurityAdmin} {
		ctx := withHumanIdentity(context.Background(), "sub-view", "view@corp.example", role, "", nil, false)
		view := srv.assistantTemplateView(ctx, assistantStoredTemplate("sub-view"))
		for _, c := range []string{"TOKEN-CANARY-7781", "/srv/template-mount-canary"} {
			if strings.Contains(string(view.Document), c) {
				t.Errorf("%s: the view carries %q: %s", role, c, view.Document)
			}
		}
	}
}

// read answers at the person's tier, capped at the security tier, never the run token's: a member's read of their
// unit cuts the workspace secret names; a super admin's read of a unit keeps them (the security tier) but never a
// mount's host path, which only the super admin tier reads; no tier reads a secret value.
func TestAssistantToolReadAtTheCappedTier(t *testing.T) {
	unit := types.GovernanceUnit{ID: uuid.New(), Name: "canary-unit", Ceiling: types.RunPolicySpec{AllowedDomains: []string{"pypi.org"},
		MinConfinementClass: types.CC2, WorkspaceMounts: []types.WorkspaceMount{{Source: "/srv/unit-mount-canary", Target: "/home/agent/m"}},
		LLMInspection: &types.LLMInspectionSpec{Mode: "alert", WorkspaceSecretNames: []string{"SECRET-NAME-CANARY"},
			WorkspaceSecretValues: []string{"secret-value-canary"}}}}
	for _, c := range []struct {
		name, role, kind, key string
		names                 bool
	}{
		{"member, their own unit", oidc.RoleUser, types.AssistantTargetTemplate, "", false},
		{"super admin, capped", oidc.RoleAdmin, types.AssistantTargetUnit, unit.ID.String(), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, srv, s, run := assistantToolSession(t, types.AssistantCompose)
			f.fresh = true
			f.govProfile, f.govGraph = &unit, []types.GovernanceUnit{unit}
			s.Role, s.TargetKind, s.TargetKey = c.role, c.kind, c.key
			f.seed(s)
			w := assistantToolCall(t, srv, run, types.AssistantToolRead, s.ID, `{"kind":"unit"}`)
			if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "canary-unit") {
				t.Fatalf("read unit = %d %s, want the unit", w.Code, w.Body)
			}
			body := w.Body.String()
			if strings.Contains(body, "/srv/unit-mount-canary") || strings.Contains(body, "secret-value-canary") {
				t.Errorf("read unit = %s: a host path or a secret value reached the model", body)
			}
			if got := strings.Contains(body, "SECRET-NAME-CANARY"); got != c.names {
				t.Errorf("secret names read = %v, want %v: %s", got, c.names, body)
			}
		})
	}
}

// draft validates the draft clamped to the member's ceiling but echoes its read cut: the ceiling's inspection
// secret names, which the clamp copies in and the member's read withholds, reach neither the answer nor the
// proposal event, on a run's llm_inspection and on a template's document.
func TestAssistantToolDraftEchoesTheMembersCut(t *testing.T) {
	unit := types.GovernanceUnit{ID: uuid.New(), Name: "canary-unit", Ceiling: types.RunPolicySpec{AllowedDomains: []string{"pypi.org"},
		MinConfinementClass: types.CC2, LLMInspection: &types.LLMInspectionSpec{Mode: "alert", DetectSecrets: true,
			WorkspaceSecretNames: []string{"ceiling-secret-name-canary"}}}}
	for _, c := range []struct{ kind, args string }{
		{types.AssistantTargetRun, `{"target_kind":"run","parts":[{"section":"llm_inspection","value":{"mode":"alert","detect_secrets":true}}]}`},
		{types.AssistantTargetTemplate, `{"target_kind":"template","parts":[{"section":"document","value":{"api_version":"v1","kind":"Template",` +
			`"intent":{"inline_policy":{"allowed_domains":["pypi.org"],"llm_inspection":{"mode":"alert","detect_secrets":true}}}}}]}`},
	} {
		t.Run(c.kind, func(t *testing.T) {
			f, srv, s, run := assistantToolSession(t, types.AssistantCompose)
			f.fresh = true
			f.govProfile, f.govGraph = &unit, []types.GovernanceUnit{unit}
			s.TargetKind = c.kind
			f.seed(s)
			w := assistantToolCall(t, srv, run, types.AssistantToolDraft, s.ID, c.args)
			if w.Code != http.StatusOK {
				t.Fatalf("draft = %d %s, want 200", w.Code, w.Body)
			}
			var p assistantProposal
			if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil || len(p.Parts) != 1 {
				t.Fatalf("parts = %+v (%v), want the one proposed section", p.Parts, err)
			}
			if !strings.Contains(string(p.Parts[0].Value), "redacted") {
				t.Errorf("part = %s, want the inspection as the member reads it", p.Parts[0].Value)
			}
			bodies := map[string]string{"draft": w.Body.String()}
			for _, e := range f.eventsOf(s.ID) {
				bodies[string(e.Kind)+" event"] += string(e.Payload)
			}
			if _, ok := bodies[string(types.AssistantEventProposal)+" event"]; !ok {
				t.Fatal("no proposal event")
			}
			for name, body := range bodies {
				if strings.Contains(body, "ceiling-secret-name-canary") {
					t.Errorf("%s carries the ceiling's secret name: %s", name, body)
				}
			}
		})
	}
}

// A validator message that names a withheld value is replaced; any other is kept.
func TestAssistantScrubWithheld(t *testing.T) {
	full := types.RunPolicySpec{LLMInspection: &types.LLMInspectionSpec{WorkspaceSecretNames: []string{"Bad-Name"}},
		EligibleGrants: []types.GrantSpec{{Kind: types.GrantGitPAT, Scope: json.RawMessage(`{"host":"git.example","secret_name":"pat-canary"}`)}}}
	read := redactSpecForRead(full, specReadMember)
	withheld := assistantWithheld(full, read)
	if !slices.Contains(withheld, "Bad-Name") || !slices.Contains(withheld, "pat-canary") {
		t.Fatalf("withheld = %v, want the inspection name and the grant's secret", withheld)
	}
	if got := assistantWithheld(full, full); len(got) != 0 {
		t.Errorf("withheld from a full read = %v, want none", got)
	}
	errs := assistantScrubWithheld([]assistantFieldError{
		{Message: `llm_inspection.workspace_secret_names[0]: "Bad-Name" is not a valid secret name`},
		{Message: "allowed_domains: too many"},
	}, withheld)
	if strings.Contains(errs[0].Message, "Bad-Name") || errs[1].Message != "allowed_domains: too many" {
		t.Errorf("scrubbed = %+v", errs)
	}
}
