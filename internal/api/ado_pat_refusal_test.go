// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// refusalStore is the audit, People and token reads the route makes, over a
// fixed list of audit rows (newest first, as the store returns them). It applies
// the caller's AuditFilter, so a window the route forgets to pass shows here.
type refusalStore struct {
	rbacStore
	events []types.AuditEvent
	people map[string]types.Person
	tokens map[string][]types.APIToken
}

func (s *refusalStore) QueryAuditEventsFilteredPage(_ context.Context, _ *uuid.UUID, f store.AuditFilter, p store.Page) ([]types.AuditEvent, error) {
	out := f.Keep(s.events)
	if p.Limit > 0 && len(out) > p.Limit {
		out = out[:p.Limit]
	}
	return out, nil
}
func (*refusalStore) ListRunsPage(context.Context, store.Page) ([]types.AgentRun, error) {
	return nil, nil
}
func (*refusalStore) ListPoliciesPage(context.Context, store.Page) ([]types.RunPolicy, error) {
	return nil, nil
}
func (*refusalStore) ListWorkspacesPage(context.Context, store.Page) ([]types.Workspace, error) {
	return nil, nil
}
func (*refusalStore) ListApprovalsPage(context.Context, types.ApprovalState, store.Page) ([]types.ApprovalRequest, error) {
	return nil, nil
}
func (*refusalStore) QueryAuditEventsPage(context.Context, uuid.UUID, store.Page) ([]types.AuditEvent, error) {
	return nil, nil
}
func (*refusalStore) QueryRecentAuditEventsPage(context.Context, store.Page) ([]types.AuditEvent, error) {
	return nil, nil
}
func (*refusalStore) ListUserDriveGrantsPage(context.Context, store.Page) ([]types.UserDriveGrant, error) {
	return nil, nil
}

func (s *refusalStore) GetPerson(_ context.Context, principal string) (types.Person, error) {
	if p, ok := s.people[principal]; ok {
		return p, nil
	}
	return types.Person{}, store.ErrNotFound
}
func (*refusalStore) CreatePerson(context.Context, types.Person) (types.Person, bool, error) {
	return types.Person{}, false, nil
}
func (*refusalStore) ListPeople(context.Context) ([]types.Person, error)          { return nil, nil }
func (*refusalStore) MarkPersonSignedIn(context.Context, string, time.Time) error { return nil }
func (s *refusalStore) ListAPITokensByPrincipal(_ context.Context, principal string) ([]types.APIToken, error) {
	return s.tokens[principal], nil
}

var refusalNow = time.Now().UTC().Truncate(time.Second)

// deniedMint is an ado_pat.mint.denied row as mintRunPAT writes it.
func deniedMint(age time.Duration, row, owner, refusal string) types.AuditEvent {
	data, _ := json.Marshal(map[string]any{
		"reason": "launch", "owner": owner, "provider_row": row, "organisation": "acme-org",
		"scope": "vso.code", "refusal": refusal, "status": 400, "pat_token_error": "accessDenied",
	})
	return types.AuditEvent{
		ID: uuid.New(), Time: refusalNow.Add(-age), ActorType: types.ActorSystem, Actor: "wardynd",
		Action: adoPATAuditMintDenied, Outcome: "failure", Data: data,
	}
}

const refusalRow = "ado-row"

func refusalServer(t *testing.T, st *refusalStore) *Server {
	t.Helper()
	h := newHarness(t)
	cfg := baseTestConfig(h, st)
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Now = func() time.Time { return refusalNow }
	return New(cfg)
}

func getRefusal(t *testing.T, srv *Server, as *http.Cookie, row string) *httptest.ResponseRecorder {
	t.Helper()
	return doSSO(t, srv, http.MethodGet, "/api/v1/workspace-providers/git/"+row+"/ado-pat-refusal", as, "")
}

func TestADOPATRefusal_AdminSeesTheNewestRefusedPerson(t *testing.T) {
	st := &refusalStore{
		events: []types.AuditEvent{
			deniedMint(2*time.Hour, refusalRow, "sub-priya", reasonADOPATPolicyBlocked),
			deniedMint(30*time.Hour, refusalRow, "sub-older", reasonADOPATPolicyBlocked),
		},
		people: map[string]types.Person{
			"sub-priya": {Principal: "sub-priya", Email: "priya@corp.example"},
			"sub-older": {Principal: "sub-older", Email: "older@corp.example"},
		},
	}
	srv := refusalServer(t, st)
	w := getRefusal(t, srv, ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin), refusalRow)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d body %q", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["person"] != "priya@corp.example" || got["at"] != refusalNow.Add(-2*time.Hour).Format(time.RFC3339) || len(got) != 2 {
		t.Fatalf("answer = %v, want exactly the newest person and time", got)
	}
	// Never the subject, the organisation, the scope or Azure DevOps' own error.
	for _, leak := range []string{"sub-priya", "acme-org", "vso.code", "accessDenied", "owner", "provider_row"} {
		if strings.Contains(w.Body.String(), leak) {
			t.Errorf("body %q leaks %q", w.Body.String(), leak)
		}
	}
}

func TestADOPATRefusal_PersonComesFromAnAPITokenWhenThereIsNoPeopleRecord(t *testing.T) {
	st := &refusalStore{
		events: []types.AuditEvent{deniedMint(time.Hour, refusalRow, "sub-tok", reasonADOPATPolicyBlocked)},
		tokens: map[string][]types.APIToken{"sub-tok": {{Principal: "sub-tok", Email: "tok@corp.example"}}},
	}
	w := getRefusal(t, refusalServer(t, st), ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin), refusalRow)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"person":"tok@corp.example"`) {
		t.Fatalf("status %d body %q", w.Code, w.Body.String())
	}
}

// A person Wardyn holds no email for is passed over, never named by subject:
// the next older refusal with an email is the answer, and with none, 204.
func TestADOPATRefusal_APersonWithNoEmailIsSkippedNotNamedBySubject(t *testing.T) {
	st := &refusalStore{
		events: []types.AuditEvent{
			deniedMint(time.Hour, refusalRow, "sub-nameless", reasonADOPATPolicyBlocked),
			deniedMint(5*time.Hour, refusalRow, "sub-known", reasonADOPATPolicyBlocked),
		},
		people: map[string]types.Person{"sub-known": {Principal: "sub-known", Email: "known@corp.example"}},
	}
	admin := ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)
	w := getRefusal(t, refusalServer(t, st), admin, refusalRow)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "known@corp.example") || strings.Contains(w.Body.String(), "sub-") {
		t.Fatalf("status %d body %q", w.Code, w.Body.String())
	}
	st.events = st.events[:1]
	if w := getRefusal(t, refusalServer(t, st), admin, refusalRow); w.Code != http.StatusNoContent || w.Body.Len() != 0 {
		t.Fatalf("no email on file: status %d body %q, want 204 and nothing", w.Code, w.Body.String())
	}
}

func TestADOPATRefusal_NothingWhenNoRefusalInSevenDays(t *testing.T) {
	people := map[string]types.Person{"sub-p": {Principal: "sub-p", Email: "p@corp.example"}}
	admin := ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)
	cases := map[string][]types.AuditEvent{
		"no rows":              nil,
		"8 days old":           {deniedMint(8*24*time.Hour, refusalRow, "sub-p", reasonADOPATPolicyBlocked)},
		"another refusal":      {deniedMint(time.Hour, refusalRow, "sub-p", reasonADOPATLifespanPolicy)},
		"another provider row": {deniedMint(time.Hour, "other-row", "sub-p", reasonADOPATPolicyBlocked)},
		"not the mint.denied action": {func() types.AuditEvent {
			ev := deniedMint(time.Hour, refusalRow, "sub-p", reasonADOPATPolicyBlocked)
			ev.Action = adoPATAuditMint
			return ev
		}()},
	}
	for name, events := range cases {
		t.Run(name, func(t *testing.T) {
			w := getRefusal(t, refusalServer(t, &refusalStore{events: events, people: people}), admin, refusalRow)
			if w.Code != http.StatusNoContent || w.Body.Len() != 0 {
				t.Fatalf("status %d body %q, want 204 and nothing", w.Code, w.Body.String())
			}
		})
	}
	// Six days is still inside the window.
	in := &refusalStore{events: []types.AuditEvent{deniedMint(6*24*time.Hour, refusalRow, "sub-p", reasonADOPATPolicyBlocked)}, people: people}
	if w := getRefusal(t, refusalServer(t, in), admin, refusalRow); w.Code != http.StatusOK {
		t.Fatalf("6 days old: status %d, want 200", w.Code)
	}
}

// Newer denials of other kinds (a person retrying a launch re-mints on every
// resolve) must not push the policy-blocked refusal out of the read.
func TestADOPATRefusal_NewerOtherDenialsDoNotHideIt(t *testing.T) {
	var events []types.AuditEvent // newest first
	for i := 0; i < 150; i++ {
		events = append(events, deniedMint(time.Duration(i+1)*time.Minute, refusalRow, "sub-priya", reasonADOPATLifespanPolicy))
	}
	for i := 0; i < 100; i++ {
		events = append(events, deniedMint(time.Duration(i+1)*time.Minute, "other-row", "sub-priya", reasonADOPATPolicyBlocked))
	}
	events = append(events, deniedMint(6*time.Hour, refusalRow, "sub-priya", reasonADOPATPolicyBlocked))
	st := &refusalStore{events: events, people: map[string]types.Person{"sub-priya": {Principal: "sub-priya", Email: "priya@corp.example"}}}
	w := getRefusal(t, refusalServer(t, st), ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin), refusalRow)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "priya@corp.example") {
		t.Fatalf("status %d body %q, want 200 naming priya behind 250 newer other denials", w.Code, w.Body.String())
	}
}

// A member never learns another member was refused: the same constant refusal
// the organisation check gives, with or without a refusal to find.
func TestADOPATRefusal_MemberIsRefusedLikeTheOrgCheck(t *testing.T) {
	st := &refusalStore{
		events: []types.AuditEvent{deniedMint(time.Hour, refusalRow, "sub-priya", reasonADOPATPolicyBlocked)},
		people: map[string]types.Person{"sub-priya": {Principal: "sub-priya", Email: "priya@corp.example"}},
	}
	srv := refusalServer(t, st)
	member := ssoSession(t, "sub-member", "member@corp.example", oidc.RoleUser)
	got := getRefusal(t, srv, member, refusalRow)
	want := doSSO(t, srv, http.MethodPost, "/api/v1/workspace-providers/git/"+refusalRow+"/org-check", member, "")
	if got.Code != http.StatusForbidden || got.Code != want.Code || got.Body.String() != want.Body.String() {
		t.Fatalf("member: %d %q, org check: %d %q, want the same refusal", got.Code, got.Body.String(), want.Code, want.Body.String())
	}
	if strings.Contains(got.Body.String(), "priya") || strings.Contains(got.Body.String(), "sub-priya") {
		t.Fatalf("member's answer names the refused person: %q", got.Body.String())
	}
}

// A laptop may forward any action it likes (POST /devices/{id}/audit), so a
// forwarded ado_pat.mint.denied row must never read as the organisation's own
// refusal: it would put a false "<person> was refused" banner in front of an
// admin. The forged row here is stored the way IngestDeviceAudit stores one
// (device-prefixed actor, device_origin mark); the genuine older row still wins.
func TestADOPATRefusal_AForwardedRowIsNeverAnOrganisationRefusal(t *testing.T) {
	people := map[string]types.Person{
		"sub-forged":  {Principal: "sub-forged", Email: "forged@corp.example"},
		"sub-genuine": {Principal: "sub-genuine", Email: "genuine@corp.example"},
	}
	dev := uuid.New()
	forged := deniedMint(time.Minute, refusalRow, "sub-forged", reasonADOPATPolicyBlocked)
	forged.Actor = store.FederatedActor(dev, "wardynd")
	forged.Data, _ = json.Marshal(map[string]any{
		"refusal": reasonADOPATPolicyBlocked, "provider_row": refusalRow, "owner": "sub-forged",
		"device_origin": map[string]any{"device_id": dev.String()},
	})
	admin := ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)

	only := &refusalStore{events: []types.AuditEvent{forged}, people: people}
	if w := getRefusal(t, refusalServer(t, only), admin, refusalRow); w.Code != http.StatusNoContent || w.Body.Len() != 0 {
		t.Fatalf("a forwarded row alone: status %d body %q, want 204 and nothing", w.Code, w.Body.String())
	}
	both := &refusalStore{events: []types.AuditEvent{forged, deniedMint(time.Hour, refusalRow, "sub-genuine", reasonADOPATPolicyBlocked)}, people: people}
	w := getRefusal(t, refusalServer(t, both), admin, refusalRow)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "genuine@corp.example") || strings.Contains(w.Body.String(), "forged") {
		t.Fatalf("forged row newer than a genuine one: status %d body %q, want the genuine person", w.Code, w.Body.String())
	}
	// Written by something other than wardynd (a human admin, or an agent) is not
	// the mint's own row either, whatever its data says.
	for name, mut := range map[string]func(*types.AuditEvent){
		"human actor": func(e *types.AuditEvent) { e.ActorType = types.ActorHuman },
		"other actor": func(e *types.AuditEvent) { e.Actor = "someone@corp.example" },
	} {
		t.Run(name, func(t *testing.T) {
			ev := deniedMint(time.Minute, refusalRow, "sub-forged", reasonADOPATPolicyBlocked)
			mut(&ev)
			st := &refusalStore{events: []types.AuditEvent{ev}, people: people}
			if w := getRefusal(t, refusalServer(t, st), admin, refusalRow); w.Code != http.StatusNoContent {
				t.Fatalf("status %d body %q, want 204", w.Code, w.Body.String())
			}
		})
	}
}
