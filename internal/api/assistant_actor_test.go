// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// recordingGroupsStore answers the sign-in record the tool context re-reads, and counts reads.
type recordingGroupsStore struct {
	store.Store
	rec   store.PrincipalGroups
	reads int
}

func (f *recordingGroupsStore) GetPrincipalGroups(context.Context, string, string) (store.PrincipalGroups, error) {
	f.reads++
	return f.rec, nil
}

func assistantTestServer(rec store.PrincipalGroups) (*Server, *recordingGroupsStore) {
	fake := &recordingGroupsStore{rec: rec}
	return &Server{cfg: Config{Store: fake}}, fake
}

// The role a tool call acts at, one case for each row of the actor table.
func TestAssistantEffectiveRole(t *testing.T) {
	s := &Server{}
	cases := []struct {
		name          string
		snap, fresh   string
		snapTruncated bool
		want          string
		denied        bool
	}{
		{"admin and admin", oidc.RoleAdmin, oidc.RoleAdmin, false, oidc.RoleSecurityAdmin, false},
		{"admin then security admin", oidc.RoleAdmin, oidc.RoleSecurityAdmin, false, oidc.RoleSecurityAdmin, false},
		{"security admin then admin", oidc.RoleSecurityAdmin, oidc.RoleAdmin, false, oidc.RoleSecurityAdmin, false},
		{"admin then user", oidc.RoleAdmin, oidc.RoleUser, false, oidc.RoleUser, false},
		{"security admin then user", oidc.RoleSecurityAdmin, oidc.RoleUser, false, oidc.RoleUser, false},
		{"user then admin", oidc.RoleUser, oidc.RoleAdmin, false, oidc.RoleUser, false},
		{"denied", oidc.RoleAdmin, accessDeniedRole, false, "", true},
		{"denied beats a truncated snapshot", oidc.RoleAdmin, accessDeniedRole, true, "", true},
		{"unknown now", oidc.RoleAdmin, roleUnknown, false, oidc.RoleUser, false},
		{"truncated snapshot", oidc.RoleAdmin, oidc.RoleAdmin, true, oidc.RoleUser, false},
		{"no SSO", "", "", false, oidc.RoleUser, false},
	}
	for _, c := range cases {
		got, denied := s.assistantEffectiveRole(c.snap, c.fresh, c.snapTruncated)
		if got != c.want || denied != c.denied {
			t.Errorf("%s: got %q denied=%v, want %q denied=%v", c.name, got, denied, c.want, c.denied)
		}
	}
}

func TestAssistantGroups(t *testing.T) {
	if g, trunc := assistantGroups([]string{"a", "b"}, []string{"b", "c"}, false, false); !slices.Equal(g, []string{"b"}) || trunc {
		t.Errorf("intersection = %v truncated %v, want [b] false", g, trunc)
	}
	for _, c := range [][2]bool{{true, false}, {false, true}} {
		if g, trunc := assistantGroups([]string{"a"}, []string{"a"}, c[0], c[1]); len(g) != 0 || g == nil || !trunc {
			t.Errorf("a truncated side gave %v truncated %v, want [] true", g, trunc)
		}
	}
}

// An empty or erased principal is refused before any read, whatever the role.
func TestAssistantToolCtxRefusesNoPerson(t *testing.T) {
	s, fake := assistantTestServer(store.PrincipalGroups{})
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, who := range []string{"", types.AssistantErasedPrincipal} {
		_, _, err := s.assistantToolCtx(r, assistantSnapshot{Principal: who, Role: oidc.RoleAdmin})
		if !errors.Is(err, errAssistantRoleDenied) {
			t.Errorf("principal %q: err = %v, want errAssistantRoleDenied", who, err)
		}
	}
	if fake.reads != 0 {
		t.Errorf("a store read happened before the refusal: %d", fake.reads)
	}
}

// A context with the assistant marker and no human is no operator of either tier.
func TestAssistantMarkerWithoutHumanIsNeverOperator(t *testing.T) {
	s := &Server{}
	ctx := withAssistantActor(context.Background())
	if !neverOperator(ctx) || s.isOperator(ctx) || s.isSecurityOperator(ctx) {
		t.Errorf("neverOperator=%v isOperator=%v isSecurityOperator=%v, want true false false", neverOperator(ctx), s.isOperator(ctx), s.isSecurityOperator(ctx))
	}
	if neverOperator(context.Background()) {
		t.Error("an unmarked bare context became never-operator")
	}
}

// The tool context is built on a fresh base: a run-token request's claims and live run do not reach it.
func TestAssistantToolCtxHasNoRunClaims(t *testing.T) {
	s, _ := assistantTestServer(store.PrincipalGroups{Email: "pat@corp.example", Groups: []string{"g1"}})
	reqCtx := withHumanIdentity(context.Background(), "sub-pat", "pat@corp.example", oidc.RoleUser, "", []string{"g1"}, false)
	runID := uuid.New()
	reqCtx = context.WithValue(reqCtx, claimsCtxKey{}, &identity.Claims{RunID: runID})
	r := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(contextWithLiveRun(reqCtx, &types.AgentRun{ID: runID}))
	if _, err := claimsFromContext(r); err != nil {
		t.Fatalf("the request is not run-token shaped: %v", err)
	}
	tctx, release, err := s.assistantToolCtx(r, assistantSnapshotOf(reqCtx))
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	tr := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(tctx)
	if _, err := claimsFromContext(tr); err == nil {
		t.Error("the tool context carries run claims")
	}
	if _, ok := liveRunFromContext(tctx); ok {
		t.Error("the tool context carries the live run")
	}
	if !assistantActorFromContext(tctx) || oidcHumanFromContext(tctx) != "sub-pat" {
		t.Error("the tool context lost the marker or the person")
	}
}

// The user type and the e-mail reach the tool context from both auth branches, so the capability subjects answer as
// on the person's direct request.
func TestAssistantToolCtxCarriesTypeAndEmail(t *testing.T) {
	const email = "pat.contractor@corp.example"
	s, _ := assistantTestServer(store.PrincipalGroups{Email: email, Groups: []string{"g1"}})
	sources := map[string]context.Context{
		// the SSO branch publishes the verified session's values
		"sso": withHumanIdentity(context.Background(), "sub-pat", email, oidc.RoleUser, "contractor", []string{"g1"}, false),
		// the api-token branch publishes the token's stamp after the cli_access clamp
		"api token": withHumanIdentity(context.Background(), "sub-pat", email, oidc.RoleUser, "contractor", []string{"g1"}, false),
	}
	for name, reqCtx := range sources {
		r := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(reqCtx)
		tctx, release, err := s.assistantToolCtx(r, assistantSnapshotOf(reqCtx))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got, want := oidcUserTypeFromContext(tctx), oidcUserTypeFromContext(reqCtx); got != want || got != "contractor" {
			t.Errorf("%s: user type = %q, want %q", name, got, want)
		}
		if got, want := oidcEmailFromContext(tctx), oidcEmailFromContext(reqCtx); got != want || got != email {
			t.Errorf("%s: email = %q, want %q", name, got, want)
		}
		tu, tg, ts := capabilitySubjects(tctx)
		ru, rg, rs := capabilitySubjects(reqCtx)
		if !slices.Equal(tu, ru) || !slices.Equal(tg, rg) || ts != rs || !slices.Contains(tu, email) {
			t.Errorf("%s: capability subjects = %v %v %v, want %v %v %v (with the e-mail)", name, tu, tg, ts, ru, rg, rs)
		}
		release()
	}
}

// A tool call answers as the person's own request does: the unit a group membership selects, and the unit a
// user-type membership selects, are the same through the tool context as through the direct one. The fixture's
// stored groups equal the snapshot's, so the intersection is the whole snapshot and the two contexts differ in
// nothing but their base. Skips without WARDYN_TEST_PG.
func TestAssistantToolCtxAnswersAsThePerson_PG(t *testing.T) {
	const principal, email = "sub-pat", "pat@corp.example"
	h := newHarness(t)
	pool := throwawayPGPool(t)
	pg := store.NewPG(pool)
	h.srv.cfg.Store = pg
	ctx := context.Background()
	if _, err := pg.CreateUserType(ctx, types.UserType{ID: "contractor", Name: "Contractor"}); err != nil {
		t.Fatal(err)
	}
	unit, err := pg.UpsertGovernanceUnit(ctx, types.GovernanceUnit{ID: uuid.New(), Name: "team-a", CreatedBy: "seed",
		Ceiling: types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com"}, MinConfinementClass: types.CC2}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pg.UpsertGovernanceMembership(ctx, types.GovernanceMembership{ID: uuid.New(), SubjectType: types.CapabilitySubjectUserType,
		Subject: "contractor", UnitID: unit.ID, CreatedBy: "seed"}); err != nil {
		t.Fatal(err)
	}
	groups := []string{"g1", "g2"}
	if err := pg.PutPrincipalGroups(ctx, principal, "", email, groups, false, time.Time{}); err != nil {
		t.Fatal(err)
	}
	direct := withHumanIdentity(context.Background(), principal, email, oidc.RoleUser, "contractor", groups, false)
	r := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(direct)
	tctx, release, err := h.srv.assistantToolCtx(r, assistantSnapshotOf(direct))
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	wu, wg, ws := capabilitySubjects(direct)
	gu, gg, gs := capabilitySubjects(tctx)
	if !slices.Equal(wu, gu) || !slices.Equal(wg, gg) || ws != gs {
		t.Errorf("capability subjects on the tool context = %v %v %v, want %v %v %v", gu, gg, gs, wu, wg, ws)
	}
	want, err := h.srv.effectiveCeiling(direct)
	if err != nil {
		t.Fatal(err)
	}
	got, err := h.srv.effectiveCeiling(tctx)
	if err != nil {
		t.Fatal(err)
	}
	if want.Unit == nil || want.Unit.ID != unit.ID {
		t.Fatalf("the direct request resolved %+v, want the user-type unit %s", want.Unit, unit.ID)
	}
	if got.Unit == nil || got.Unit.ID != want.Unit.ID || got.Operator != want.Operator {
		t.Errorf("the tool context resolved %+v operator=%v, want unit %s operator=%v", got.Unit, got.Operator, want.Unit.ID, want.Operator)
	}
}
