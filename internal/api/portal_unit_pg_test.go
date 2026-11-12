// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A portal can be pinned to one unit (run_in_under, set at registration): a delegated run is refused
// unless the context the run resolves to is that unit or one below it. These tests need Postgres
// (WARDYN_TEST_PG) and the whole path: a real exchange, the delegate store, the fold.

const (
	pinTeamGroup  = "plat-members"
	pinOtherGroup = "other-members"
)

type portalPinEnv struct {
	*delegationPG
	plat, team, other types.GovernanceUnit
	teamMember        types.GovernanceMembership
}

// newPortalPinEnv seeds a platform unit with a team below it and a second, unrelated unit.
// Members of pinTeamGroup are in the team; members of pinOtherGroup are in the other unit.
func newPortalPinEnv(t *testing.T) *portalPinEnv {
	t.Helper()
	e := &portalPinEnv{delegationPG: newDelegationPG(t)}
	e.plat = e.unit(t, types.GovernanceUnit{Name: "platform", Ceiling: types.RunPolicySpec{
		AllowedDomains: []string{"api.anthropic.com"}, MinConfinementClass: types.CC2}})
	e.team = e.unit(t, types.GovernanceUnit{Name: "platform-team", ParentID: &e.plat.ID, Overlay: &types.CeilingOverlay{}})
	e.other = e.unit(t, types.GovernanceUnit{Name: "other-co", Ceiling: types.RunPolicySpec{
		AllowedDomains: []string{"api.anthropic.com"}, MinConfinementClass: types.CC2}})
	e.teamMember = e.member(t, pinTeamGroup, e.team, false)
	e.member(t, pinOtherGroup, e.other, false)
	return e
}

func (e *portalPinEnv) unit(t *testing.T, u types.GovernanceUnit) types.GovernanceUnit {
	t.Helper()
	u.ID, u.CreatedBy = uuid.New(), "seed"
	got, err := e.st.UpsertGovernanceUnit(context.Background(), u)
	if err != nil {
		t.Fatalf("seed unit %q: %v", u.Name, err)
	}
	return got
}

func (e *portalPinEnv) member(t *testing.T, group string, u types.GovernanceUnit, home bool) types.GovernanceMembership {
	t.Helper()
	m, err := e.st.UpsertGovernanceMembership(context.Background(), types.GovernanceMembership{
		ID: uuid.New(), SubjectType: types.CapabilitySubjectGroup, Subject: group, UnitID: u.ID, Priority: 1, Home: home})
	if err != nil {
		t.Fatalf("seed membership %q -> %s: %v", group, u.Name, err)
	}
	return m
}

// pinnedPortal registers a portal pinned to unit (a name or an id; "" registers an unpinned one) and
// returns its id and credential. The wire field is read as JSON so the helper names no Go field.
func (e *portalPinEnv) pinnedPortal(t *testing.T, unit string) (uuid.UUID, string) {
	t.Helper()
	body := `{"name":"front end","idp_client_id":"` + delegPortalClient + `","group":"` + delegGroup + `"`
	if unit != "" {
		body += `,"run_in_under":"` + unit + `"`
	}
	w := doSSO(t, e.h.srv, http.MethodPost, "/api/v1/admin/delegates", e.admin, body+`}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("register portal pinned to %q: %d %s", unit, w.Code, w.Body.String())
	}
	var d struct {
		ID         uuid.UUID `json:"id"`
		Credential string    `json:"credential"`
		RunInUnder string    `json:"run_in_under"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if (unit == "") != (d.RunInUnder == "") {
		t.Fatalf("registered portal answers run_in_under %q for pin %q", d.RunInUnder, unit)
	}
	return d.ID, d.Credential
}

func (e *portalPinEnv) token(t *testing.T, portal uuid.UUID, cred, sub string, groups ...string) string {
	t.Helper()
	return e.delegate(t, portal, cred, sub, sub+"@corp.example", append([]string{delegGroup}, groups...)...)
}

func (e *portalPinEnv) post(t *testing.T, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, e.h.srv, http.MethodPost, path, token, body)
}

func portalScopeRefused(w *httptest.ResponseRecorder) bool {
	return w.Code == http.StatusForbidden && errorReason(w) == "portal_unit_scope"
}

const pinRunBody = `{"agent":"claude-code","task":"t"`

func pinRun(runIn string) string {
	if runIn == "" {
		return pinRunBody + `}`
	}
	return pinRunBody + `,"run_in":"` + runIn + `"}`
}

// TestPortalPinnedAllowsSubtreeUnit: a portal pinned to the platform starts a run in the team below
// it, named or picked by default, exactly as an unpinned portal would.
func TestPortalPinnedAllowsSubtreeUnit(t *testing.T) {
	e := newPortalPinEnv(t)
	pinned, pcred := e.pinnedPortal(t, e.plat.Name)
	tok := e.token(t, pinned, pcred, delegPerson, pinTeamGroup)
	for _, runIn := range []string{e.team.Name, ""} {
		if w := e.post(t, "/api/v1/runs", tok, pinRun(runIn)); w.Code != http.StatusCreated {
			t.Errorf("run_in %q under the pin: %d %s, want 201", runIn, w.Code, w.Body.String())
		}
	}
	// The pin by id is the same pin.
	byID, bcred := e.pinnedPortal(t, e.plat.ID.String())
	tok = e.token(t, byID, bcred, delegPerson, pinTeamGroup)
	if w := e.post(t, "/api/v1/runs", tok, pinRun(e.team.Name)); w.Code != http.StatusCreated {
		t.Errorf("pin by id: %d %s, want 201", w.Code, w.Body.String())
	}
}

// TestPortalPinnedRefusesOtherContext: a person in two platforms' units who names the other one
// through a pinned portal is refused; the unpinned portal lets the same request through.
func TestPortalPinnedRefusesOtherContext(t *testing.T) {
	e := newPortalPinEnv(t)
	pinned, pcred := e.pinnedPortal(t, e.plat.Name)
	free, fcred := e.pinnedPortal(t, "")
	pt := e.token(t, pinned, pcred, delegPerson, pinTeamGroup, pinOtherGroup)
	ft := e.token(t, free, fcred, delegPerson, pinTeamGroup, pinOtherGroup)
	w := e.post(t, "/api/v1/runs", pt, pinRun(e.other.Name))
	if !portalScopeRefused(w) || !strings.Contains(w.Body.String(), e.plat.Name) {
		t.Fatalf("other unit through the pinned portal: %d %s, want 403 portal_unit_scope naming %q", w.Code, w.Body.String(), e.plat.Name)
	}
	if w := e.post(t, "/api/v1/runs", ft, pinRun(e.other.Name)); w.Code != http.StatusCreated {
		t.Errorf("control, unpinned portal: %d %s, want 201", w.Code, w.Body.String())
	}
	// The refusal leaves no run behind.
	if rows := e.rows("run.create"); len(rows) != 1 {
		t.Errorf("run.create rows = %d, want 1 (only the control's)", len(rows))
	}
}

// TestPortalPinnedDefaultPickOutsideRefused: a launch that names no run_in is judged on the context it
// resolves to. A person whose home is another platform's unit, or who is in no unit at all (the
// Default context), is refused through the pinned portal.
func TestPortalPinnedDefaultPickOutsideRefused(t *testing.T) {
	e := newPortalPinEnv(t)
	e.member(t, pinOtherGroup, e.other, true)
	pinned, pcred := e.pinnedPortal(t, e.plat.Name)
	free, fcred := e.pinnedPortal(t, "")
	for name, tc := range map[string]struct {
		sub    string
		groups []string
	}{
		"home in another platform": {"home-sub", []string{pinTeamGroup, pinOtherGroup}},
		"no unit":                  {"loose-sub", nil},
	} {
		w := e.post(t, "/api/v1/runs", e.token(t, pinned, pcred, tc.sub, tc.groups...), pinRun(""))
		if !portalScopeRefused(w) {
			t.Errorf("%s: pinned portal %d %s, want 403 portal_unit_scope", name, w.Code, w.Body.String())
		}
		if w := e.post(t, "/api/v1/runs", e.token(t, free, fcred, tc.sub, tc.groups...), pinRun("")); w.Code != http.StatusCreated {
			t.Errorf("%s: control, unpinned portal %d %s, want 201", name, w.Code, w.Body.String())
		}
	}
}

// TestPortalPinnedUnitDeletedRefuses: the pin is a bare id with no foreign key. When the unit it names
// is deleted the portal keeps refusing; it never falls back to unpinned.
func TestPortalPinnedUnitDeletedRefuses(t *testing.T) {
	e := newPortalPinEnv(t)
	scratch := e.unit(t, types.GovernanceUnit{Name: "scratch", Ceiling: types.RunPolicySpec{
		AllowedDomains: []string{"api.anthropic.com"}, MinConfinementClass: types.CC2}})
	m := e.member(t, "scratch-members", scratch, false)
	pinned, pcred := e.pinnedPortal(t, scratch.Name)
	tok := e.token(t, pinned, pcred, delegPerson, "scratch-members", pinTeamGroup)
	if w := e.post(t, "/api/v1/runs", tok, pinRun(scratch.Name)); w.Code != http.StatusCreated {
		t.Fatalf("before the delete: %d %s, want 201", w.Code, w.Body.String())
	}
	ctx := context.Background()
	if _, err := e.st.DeleteGovernanceMembership(ctx, m.ID); err != nil {
		t.Fatal(err)
	}
	if err := e.st.DeleteGovernanceUnit(ctx, scratch.ID); err != nil {
		t.Fatal(err)
	}
	tok = e.token(t, pinned, pcred, delegPerson, pinTeamGroup)
	if w := e.post(t, "/api/v1/runs", tok, pinRun("")); !portalScopeRefused(w) {
		t.Errorf("after the pinned unit was deleted: %d %s, want 403 portal_unit_scope", w.Code, w.Body.String())
	}
}

type unreadablePortalStore struct{ store.PG }

func (unreadablePortalStore) GetDelegateByID(context.Context, uuid.UUID) (types.Delegate, error) {
	return types.Delegate{}, errors.New("portal row unreadable")
}

// TestPortalPinUnreadable503: a portal row the fold cannot read refuses the launch with 503, never
// as if the portal were unpinned.
func TestPortalPinUnreadable503(t *testing.T) {
	e := newPortalPinEnv(t)
	pinned, pcred := e.pinnedPortal(t, e.plat.Name)
	tok := e.token(t, pinned, pcred, delegPerson, pinTeamGroup)
	e.h.srv.cfg.Store = unreadablePortalStore{e.st}
	w := e.post(t, "/api/v1/runs", tok, pinRun(e.team.Name))
	if w.Code != http.StatusServiceUnavailable || errorReason(w) != "portal_unit_unavailable" {
		t.Errorf("unreadable portal: %d %s, want 503 portal_unit_unavailable", w.Code, w.Body.String())
	}
}

// TestPortalPinnedRestrictionStillClamps: a restriction is applied after the context is picked, so a
// run the pin admits is still clamped by the person's restriction. The refusal is the restriction's,
// the same one an unpinned portal gets, and a person it does not cover still launches.
func TestPortalPinnedRestrictionStillClamps(t *testing.T) {
	e := newPortalPinEnv(t)
	if _, err := e.st.PutRestriction(context.Background(), types.Restriction{ID: uuid.New(), Name: "no-shell",
		Limits:  types.RestrictionLimits{DenyInteractive: new(true)},
		Contact: &policyref.Contact{Owner: "no-shell-owner", Email: "no-shell@corp.example"},
		Members: []types.RestrictionMember{{SubjectType: types.CapabilitySubjectUser, Subject: delegPerson}}}, "seed"); err != nil {
		t.Fatal(err)
	}
	pinned, pcred := e.pinnedPortal(t, e.plat.Name)
	free, fcred := e.pinnedPortal(t, "")
	const interactive = `{"agent":"claude-code","task":"t","interactive":true,"run_in":"platform-team"}`
	got := e.post(t, "/api/v1/runs", e.token(t, pinned, pcred, delegPerson, pinTeamGroup), interactive)
	want := e.post(t, "/api/v1/runs", e.token(t, free, fcred, delegPerson, pinTeamGroup), interactive)
	if got.Code == http.StatusCreated || portalScopeRefused(got) {
		t.Fatalf("restricted person through the pinned portal: %d %s, want the restriction's refusal", got.Code, got.Body.String())
	}
	if got.Code != want.Code || got.Body.String() != want.Body.String() {
		t.Errorf("pinned %d %s, unpinned %d %s: want the same refusal", got.Code, got.Body.String(), want.Code, want.Body.String())
	}
	if w := e.post(t, "/api/v1/runs", e.token(t, pinned, pcred, "free-sub", pinTeamGroup), interactive); w.Code != http.StatusCreated {
		t.Errorf("person the restriction does not cover: %d %s, want 201", w.Code, w.Body.String())
	}
}

// TestPortalPinPreflightAndPreviewAgree: the check is one fold step at every door. Preflight refuses
// and admits exactly as create does. (The step runs at all three doors: TestPreflightMirrorsLaunchGates
// pins its door mask, and the preview door is not on a portal's reach list.)
func TestPortalPinPreflightAndPreviewAgree(t *testing.T) {
	e := newPortalPinEnv(t)
	pinned, pcred := e.pinnedPortal(t, e.plat.Name)
	tok := e.token(t, pinned, pcred, delegPerson, pinTeamGroup, pinOtherGroup)
	for runIn, wantRefused := range map[string]bool{e.other.Name: true, e.team.Name: false} {
		pre := e.post(t, "/api/v1/runs/preflight", tok, pinRun(runIn))
		cre := e.post(t, "/api/v1/runs", tok, pinRun(runIn))
		if portalScopeRefused(pre) != wantRefused || portalScopeRefused(cre) != wantRefused {
			t.Errorf("run_in %q: preflight %d %s, create %d %s; want portal_unit_scope refused=%v at both", runIn, pre.Code, pre.Body.String(), cre.Code, cre.Body.String(), wantRefused)
		}
		if wantRefused && pre.Body.String() != cre.Body.String() {
			t.Errorf("run_in %q: preflight and create refuse in different words:\n%s\n%s", runIn, pre.Body.String(), cre.Body.String())
		}
	}
}

// TestPortalUnpinnedUnchanged: a portal registered without run_in_under launches in every context the
// person holds, as before.
func TestPortalUnpinnedUnchanged(t *testing.T) {
	e := newPortalPinEnv(t)
	free, fcred := e.pinnedPortal(t, "")
	tok := e.token(t, free, fcred, delegPerson, pinTeamGroup, pinOtherGroup)
	for _, runIn := range []string{e.team.Name, e.other.Name, ""} {
		if w := e.post(t, "/api/v1/runs", tok, pinRun(runIn)); w.Code != http.StatusCreated {
			t.Errorf("unpinned portal, run_in %q: %d %s, want 201", runIn, w.Code, w.Body.String())
		}
	}
}

// TestPortalLaunchCarriesInlineComponent: a platform's tool travels as an inline component on the
// portal's own POST /runs, with no org component and no stored secret.
func TestPortalLaunchCarriesInlineComponent(t *testing.T) {
	e := newPortalPinEnv(t)
	pinned, pcred := e.pinnedPortal(t, e.plat.Name)
	tok := e.token(t, pinned, pcred, delegPerson, pinTeamGroup)
	body := `{"agent":"claude-code","task":"t","run_in":"platform-team","components":[{"inline":{"hosts":["tool.platform.example"]}}]}`
	if w := e.post(t, "/api/v1/runs", tok, body); w.Code != http.StatusCreated {
		t.Fatalf("delegated launch with an inline component: %d %s, want 201", w.Code, w.Body.String())
	}
	var sawInline bool
	for _, ev := range e.h.audit.snapshot() {
		sawInline = sawInline || strings.Contains(string(ev.Data), `"source":"inline"`)
	}
	if !sawInline {
		t.Error("no audit row records the inline component")
	}
}
