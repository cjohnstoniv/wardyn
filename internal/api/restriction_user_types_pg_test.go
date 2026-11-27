// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The pins of restrictions over every user type a person's sign-in matches (#4073). The person's groups
// match contractor (priority 1), intern (priority 2) and engineer (priority 10), so their session is
// stamped engineer; R names user_type contractor and denies x.example.

const utSub = "sub-ut-person"

// countingRoleMappings is the role mappings the authenticator reads, counting each read: a derivation
// reads them once.
type countingRoleMappings struct {
	rows  []oidc.RoleMapping
	reads *atomic.Int32
}

func (c countingRoleMappings) ListRoleMappings(context.Context) ([]oidc.RoleMapping, error) {
	c.reads.Add(1)
	return slices.Clone(c.rows), nil
}

type utEnv struct {
	*govEnv
	reads *atomic.Int32
	now   *time.Time
}

// newUTEnv is a govEnv whose authenticator derives over the role mappings above and the store's user
// types, with a clock the test moves. withR seeds R.
func newUTEnv(t *testing.T, mode string, withR bool, mutate ...func(*Config)) (*utEnv, types.Restriction) {
	t.Helper()
	t.Setenv(envIdentityNarrowing, mode)
	pool := throwawayPGPool(t)
	pg := store.NewPG(pool)
	for _, ut := range []types.UserType{{ID: "contractor", Priority: 1}, {ID: "intern", Priority: 2}, {ID: "engineer", Priority: 10}} {
		ut.Name, ut.CreatedBy = ut.ID, "seed"
		if _, err := pg.CreateUserType(context.Background(), ut); err != nil {
			t.Fatalf("seed user type %s: %v", ut.ID, err)
		}
	}
	reads := &atomic.Int32{}
	mappings := countingRoleMappings{reads: reads, rows: []oidc.RoleMapping{
		{Value: "ut-contractors", Role: oidc.RoleUser, UserType: "contractor"},
		{Value: "ut-interns", Role: oidc.RoleUser, UserType: "intern"},
		{Value: "ut-engineers", Role: oidc.RoleUser, UserType: "engineer"},
	}}
	auth := newAccessAuth(t, nil, "", nil, nil, func(c *oidc.Config) { c.RoleMappings, c.UserTypes = mappings, pg })
	now := time.Now().UTC()
	opts := append([]func(*Config){func(c *Config) {
		c.OIDC = auth
		c.Now = func() time.Time { return now }
	}}, mutate...)
	e := &utEnv{govEnv: newGovEnvOn(t, pool, opts...), reads: reads, now: &now}
	var r types.Restriction
	if withR {
		r = seedRestriction(t, e.govEnv, "ut-contractor-floor", types.RestrictionSpec{DeniedDomains: strs("x.example")}, types.RestrictionLimits{},
			types.RestrictionMember{SubjectType: types.CapabilitySubjectUserType, Subject: "contractor"})
	}
	return e, r
}

// utPersonCtx is the person's own request: sub, email, the three groups and the stamped type.
func utPersonCtx() context.Context {
	return treeCtx(utSub, []string{"ut-contractors", "ut-engineers", "ut-interns"}, "engineer")
}

// utTruncated is utPersonCtx with a truncated group snapshot.
func utTruncated() context.Context { return withOIDCGroupsTruncated(utPersonCtx(), true) }

type utAlertRestriction struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
}

type utAlert struct {
	UserType     string               `json:"user_type"`
	UserTypes    []string             `json:"user_types"`
	Restrictions []utAlertRestriction `json:"restrictions"`
	Unresolvable bool                 `json:"unresolvable"`
	Mode         string               `json:"mode"`
}

func (e *utEnv) alerts(t *testing.T) []utAlert {
	t.Helper()
	var out []utAlert
	for _, ev := range e.h.audit.snapshot() {
		if ev.Action != "governance.restriction.alert" {
			continue
		}
		if ev.Outcome != "success" || ev.Target != "governance.ceiling" || ev.Actor != utSub || ev.ActorType != types.ActorHuman {
			t.Errorf("alert row %+v, want success on governance.ceiling by the person", ev)
		}
		var a utAlert
		if err := json.Unmarshal(ev.Data, &a); err != nil {
			t.Fatalf("alert data %s: %v", ev.Data, err)
		}
		out = append(out, a)
	}
	return out
}

func (e *utEnv) staleRefusals() int {
	n := 0
	for _, ev := range e.h.audit.snapshot() {
		if ev.Action == authz.AuditAction && strings.Contains(string(ev.Data), `"reason":"`+string(authz.ReasonGroupsSnapshotStale)+`"`) {
			n++
		}
	}
	return n
}

func deniesX(c governanceCeiling) bool { return slices.Contains(c.Spec.DeniedDomains, "x.example") }

// assertAuditOnly: the ceiling resolves without R and one alert names R against the stamped type.
func assertAuditOnly(t *testing.T, e *utEnv, r types.Restriction) {
	t.Helper()
	c := treeCeiling(t, e.govEnv, utPersonCtx())
	if deniesX(c) || slices.Contains(c.RestrictionIDs, r.ID) {
		t.Errorf("audit: denied %v ids %v, want R not applied", c.Spec.DeniedDomains, c.RestrictionIDs)
	}
	a := e.alerts(t)
	if len(a) != 1 {
		t.Fatalf("audit: %d alert rows, want 1", len(a))
	}
	if a[0].UserType != "engineer" || !slices.Equal(a[0].UserTypes, []string{"contractor"}) || a[0].Mode != "audit" || a[0].Unresolvable ||
		len(a[0].Restrictions) != 1 || a[0].Restrictions[0].ID != r.ID || a[0].Restrictions[0].Name != r.Name {
		t.Errorf("audit: alert %+v, want R on contractor beside the stamped engineer, mode audit", a[0])
	}
}

func TestUserTypeRestrictions_LowerTypeAuditOnly(t *testing.T) {
	e, r := newUTEnv(t, identityNarrowingAudit, true)
	assertAuditOnly(t, e, r)
}

func TestUserTypeRestrictions_DefaultModeAudits(t *testing.T) {
	e, r := newUTEnv(t, "", true)
	assertAuditOnly(t, e, r)
}

func TestUserTypeRestrictions_LowerTypeEnforced(t *testing.T) {
	e, r := newUTEnv(t, identityNarrowingEnforce, true)
	c := treeCeiling(t, e.govEnv, utPersonCtx())
	if !deniesX(c) {
		t.Errorf("enforce: denied %v, want x.example", c.Spec.DeniedDomains)
	}
	if !slices.Contains(c.RestrictionIDs, r.ID) {
		t.Errorf("enforce: restriction ids %v, want R", c.RestrictionIDs)
	}
	if got := c.restrictionKnobs["denied_domains"]; got.Layer != policyref.LayerRestriction || got.Name != r.Name {
		t.Errorf("enforce: denied_domains provenance %+v, want the restriction layer naming R", got)
	}
	var run types.AgentRun
	e.srv.captureRunLimits(&run, c, nil)
	if !slices.Contains(run.RestrictionIDs, r.ID) {
		t.Errorf("enforce: the run captured %v, want R", run.RestrictionIDs)
	}
	if a := e.alerts(t); len(a) != 0 {
		t.Errorf("enforce: %d alert rows, want none", len(a))
	}
}

func TestUserTypeRestrictions_DisplayReadWritesNoAlert(t *testing.T) {
	e, _ := newUTEnv(t, identityNarrowingAudit, true)
	if c := treeCeiling(t, e.govEnv, withDisplayRead(utPersonCtx())); deniesX(c) {
		t.Errorf("display read: denied %v, want R not applied", c.Spec.DeniedDomains)
	}
	if a := e.alerts(t); len(a) != 0 {
		t.Errorf("display read: %d alert rows, want none", len(a))
	}
}

func TestUserTypeRestrictions_DisplayReadEnforceStaleWritesNoRow(t *testing.T) {
	e, _ := newUTEnv(t, identityNarrowingEnforce, true)
	if _, err := e.srv.effectiveCeiling(withDisplayRead(utTruncated())); !errors.Is(err, errGroupsSnapshotStale) {
		t.Errorf("display read, truncated: %v, want %v", err, errGroupsSnapshotStale)
	}
	if n := e.staleRefusals(); n != 0 {
		t.Errorf("display read, truncated: %d authz.denied rows, want none", n)
	}
}

func TestUserTypeRestrictions_NoOtherTypeRestrictionSkipsDerivation(t *testing.T) {
	e, _ := newUTEnv(t, identityNarrowingEnforce, false)
	seedRestriction(t, e.govEnv, "ut-engineer-floor", types.RestrictionSpec{DeniedDomains: strs("y.example")}, types.RestrictionLimits{},
		types.RestrictionMember{SubjectType: types.CapabilitySubjectUserType, Subject: "engineer"})
	c := treeCeiling(t, e.govEnv, utPersonCtx())
	if !slices.Contains(c.Spec.DeniedDomains, "y.example") {
		t.Errorf("the stamped type's restriction: denied %v, want y.example", c.Spec.DeniedDomains)
	}
	if n := e.reads.Load(); n != 0 {
		t.Errorf("only the stamped type is restricted: %d role-mapping reads, want 0 (no derivation)", n)
	}
	seedRestriction(t, e.govEnv, "ut-contractor-floor", types.RestrictionSpec{DeniedDomains: strs("x.example")}, types.RestrictionLimits{},
		types.RestrictionMember{SubjectType: types.CapabilitySubjectUserType, Subject: "contractor"})
	if c := treeCeiling(t, e.govEnv, utPersonCtx()); !deniesX(c) || e.reads.Load() == 0 {
		t.Errorf("another type restricted: denied %v after %d reads, want x.example from a derivation", c.Spec.DeniedDomains, e.reads.Load())
	}
}

func TestUserTypeRestrictions_StaleGroupsEnforceRefuses(t *testing.T) {
	e, _ := newUTEnv(t, identityNarrowingEnforce, true)
	if _, err := e.srv.effectiveCeiling(utTruncated()); !errors.Is(err, errGroupsSnapshotStale) {
		t.Errorf("truncated: %v, want %v", err, errGroupsSnapshotStale)
	}
	if n := e.staleRefusals(); n != 1 {
		t.Errorf("truncated: %d authz.denied rows naming %s, want 1", n, authz.ReasonGroupsSnapshotStale)
	}
}

func TestUserTypeRestrictions_StaleGroupsAuditAlerts(t *testing.T) {
	e, _ := newUTEnv(t, identityNarrowingAudit, true)
	c, err := e.srv.effectiveCeiling(utTruncated())
	if err != nil || deniesX(c) {
		t.Fatalf("audit, truncated: %v %v, want the ceiling resolved without R", c.Spec.DeniedDomains, err)
	}
	a := e.alerts(t)
	if len(a) != 1 || !a[0].Unresolvable || a[0].UserType != "engineer" || len(a[0].Restrictions) != 0 || a[0].Mode != "audit" {
		t.Errorf("audit, truncated: alerts %+v, want one unresolvable", a)
	}
	if n := e.staleRefusals(); n != 0 {
		t.Errorf("audit, truncated: %d refusal rows, want none", n)
	}
}

func TestUserTypeRestrictions_OwnerReviveEnforced(t *testing.T) {
	e, r := newUTEnv(t, identityNarrowingEnforce, true)
	run := types.AgentRun{ID: uuid.New(), CreatedBy: utSub, UserType: "engineer"}
	c, rerr := e.srv.reviveCeiling(utPersonCtx(), run, types.ActorHuman, utSub)
	if rerr != nil {
		t.Fatalf("owner revive: %+v", rerr)
	}
	if !slices.Contains(c.deny, "x.example") || !slices.Contains(c.restrictionIDs, r.ID) {
		t.Errorf("owner revive: deny %v ids %v, want R met", c.deny, c.restrictionIDs)
	}
}

func TestUserTypeRestrictions_NilOIDCChangesNothing(t *testing.T) {
	for _, mode := range []string{identityNarrowingAudit, identityNarrowingEnforce} {
		t.Run(mode, func(t *testing.T) {
			e, r := newUTEnv(t, mode, true, func(c *Config) { c.OIDC = nil })
			if c, err := e.srv.effectiveCeiling(utPersonCtx()); err == nil && (deniesX(c) || slices.Contains(c.RestrictionIDs, r.ID)) {
				t.Errorf("no sign-in: denied %v ids %v, want R not applied", c.Spec.DeniedDomains, c.RestrictionIDs)
			}
			q := contextQuery{users: []string{types.CanonicalUserSubject(utSub)}, groups: []string{"ut-contractors"}, userType: "engineer",
				email: utSub + "@corp.example"}
			if got, err := e.srv.userTypeRestrictions(utPersonCtx(), q, nil); err != nil || len(got) != 0 {
				t.Errorf("no sign-in: userTypeRestrictions = %v %v, want nothing added", got, err)
			}
			if a := e.alerts(t); len(a) != 0 {
				t.Errorf("no sign-in: %d alert rows, want none", len(a))
			}
			if n := e.reads.Load(); n != 0 {
				t.Errorf("no sign-in: %d role-mapping reads, want 0", n)
			}
		})
	}
}

func TestUserTypeRestrictions_AlertOncePerPersonSetAndDay(t *testing.T) {
	e, _ := newUTEnv(t, identityNarrowingAudit, true)
	treeCeiling(t, e.govEnv, utPersonCtx())
	treeCeiling(t, e.govEnv, utPersonCtx())
	if n := len(e.alerts(t)); n != 1 {
		t.Fatalf("two requests on one day: %d alert rows, want 1", n)
	}
	r2 := seedRestriction(t, e.govEnv, "ut-intern-floor", types.RestrictionSpec{DeniedDomains: strs("z.example")}, types.RestrictionLimits{},
		types.RestrictionMember{SubjectType: types.CapabilitySubjectUserType, Subject: "intern"})
	treeCeiling(t, e.govEnv, utPersonCtx())
	treeCeiling(t, e.govEnv, utPersonCtx())
	a := e.alerts(t)
	if len(a) != 2 {
		t.Fatalf("a second restriction on another matched type: %d alert rows, want 2", len(a))
	}
	if !slices.Equal(a[1].UserTypes, []string{"contractor", "intern"}) || len(a[1].Restrictions) != 2 ||
		!slices.ContainsFunc(a[1].Restrictions, func(x utAlertRestriction) bool { return x.ID == r2.ID }) {
		t.Errorf("the second alert %+v, want both restrictions on contractor and intern", a[1])
	}
	*e.now = e.now.Add(24 * time.Hour)
	treeCeiling(t, e.govEnv, utPersonCtx())
	if n := len(e.alerts(t)); n != 3 {
		t.Errorf("the next UTC day: %d alert rows, want 3", n)
	}
}
