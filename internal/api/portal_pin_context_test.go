// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// personCtx is a request of principal through the fixture portal, as the middleware builds it.
func (f *portalPinFixture) personCtx(principal string) context.Context {
	ctx := withHumanIdentity(context.Background(), principal, "", oidc.RoleUser, f.delegate.UserType, []string{}, false)
	return audit.WithDelegation(ctx, f.via)
}

// heldRow is an actual membership of the fixture person in u; expires makes it an elevation.
func (f *portalPinFixture) heldRow(u types.GovernanceUnit, home bool, expires *time.Time) store.MembershipMatch {
	return store.MembershipMatch{Unit: u, Membership: types.GovernanceMembership{ID: uuid.New(), SubjectType: types.CapabilitySubjectUser,
		Subject: f.person, UnitID: u.ID, Priority: 1, Home: home, ExpiresAt: expires}}
}

// setPortal edits the fixture portal's row in place.
func (f *portalPinFixture) setPortal(edit func(*types.Delegate)) {
	f.ps.pmu.Lock()
	defer f.ps.pmu.Unlock()
	d := f.ps.portals[f.via.Delegate]
	edit(&d)
	f.ps.portals[f.via.Delegate] = d
}

// pickFor runs the helper and then pickContext, failing on a helper error.
func (f *portalPinFixture) pickFor(t *testing.T, ctx context.Context, active []store.MembershipMatch, choice contextChoice) (contextPick, error) {
	t.Helper()
	got, err := f.srv.portalPinMemberships(ctx, active, choice)
	if err != nil {
		t.Fatalf("portalPinMemberships: %v", err)
	}
	pick, _, err := f.srv.pickContext(ctx, got, choice)
	return pick, err
}

func unitIDs(ms []store.MembershipMatch) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Unit.ID)
	}
	return out
}

// A fresh portal person with no membership has the pin and the unit under it as contexts, pin first: no run_in
// lands in the pin, run_in names the child, and a unit outside the pin is not a context.
func TestPortalPinContext_FreshPortalNoMembership(t *testing.T) {
	f := newPinFixture(t)
	ctx := f.personCtx(f.person)
	got, err := f.srv.portalPinMemberships(ctx, nil, contextChoice{})
	if err != nil {
		t.Fatal(err)
	}
	if ids := unitIDs(got); len(ids) != 2 || ids[0] != f.pin.ID || ids[1] != f.child.ID {
		t.Fatalf("pin contexts = %v, want [pin %s, child %s]", ids, f.pin.ID, f.child.ID)
	}
	for _, m := range got {
		if m.Membership.ID != uuid.Nil || m.Membership.Home || m.Membership.Legacy || m.Membership.ExpiresAt != nil ||
			m.Membership.SubjectType != types.CapabilitySubjectUser || m.Membership.Subject != types.CanonicalUserSubject(f.person) {
			t.Errorf("pin context row = %+v, want a plain, unstored user row naming the person", m.Membership)
		}
	}
	if pick, err := f.pickFor(t, ctx, nil, contextChoice{}); err != nil || pick.unit.ID != f.pin.ID {
		t.Errorf("no run_in = %s %v, want the pin", pick.unit.ID, err)
	}
	if pick, err := f.pickFor(t, ctx, nil, contextChoice{RunIn: f.child.ID}); err != nil || pick.unit.ID != f.child.ID {
		t.Errorf("run_in child = %s %v, want the child", pick.unit.ID, err)
	}
	if _, err := f.pickFor(t, ctx, nil, contextChoice{RunIn: f.outside.ID}); !errors.Is(err, errUnitNotAContext) {
		t.Errorf("run_in outside = %v, want errUnitNotAContext", err)
	}
}

// A membership outside the pin is never the default pick, but a run_in naming it keeps it, so the pin's own
// check refuses it later as portal_unit_scope.
func TestPortalPinContext_OutsideRowDroppedUnlessNamed(t *testing.T) {
	f := newPinFixture(t)
	ctx := f.personCtx(f.person)
	active := []store.MembershipMatch{f.heldRow(f.outside, true, nil)}
	if pick, err := f.pickFor(t, ctx, active, contextChoice{}); err != nil || pick.unit.ID != f.pin.ID {
		t.Errorf("no run_in with an outside home row = %s %v, want the pin", pick.unit.ID, err)
	}
	pick, err := f.pickFor(t, ctx, active, contextChoice{RunIn: f.outside.ID})
	if err != nil || pick.unit.ID != f.outside.ID || pick.row == nil || pick.row.Membership.ID != active[0].Membership.ID {
		t.Fatalf("run_in outside with the row = %s %v, want the person's own outside row", pick.unit.ID, err)
	}
	if r := f.srv.portalUnitRefusal(ctx, governanceCeiling{Unit: &ResolvedUnit{ID: f.outside.ID, Name: f.outside.Name}}, false); r == nil ||
		r.status != http.StatusForbidden || r.body.Reason != "portal_unit_scope" {
		t.Errorf("the outside pick at the pin's check = %+v, want 403 portal_unit_scope", r)
	}
}

// A home membership the person holds inside the pin wins over the pin unit when no run_in is named.
func TestPortalPinContext_InPinHomeRowWins(t *testing.T) {
	f := newPinFixture(t)
	ctx := f.personCtx(f.person)
	active := []store.MembershipMatch{f.heldRow(f.child, true, nil)}
	if pick, err := f.pickFor(t, ctx, active, contextChoice{}); err != nil || pick.unit.ID != f.child.ID || pick.row == nil || !pick.row.Membership.Home {
		t.Errorf("no run_in with a home row in the child = %s %v, want the home row", pick.unit.ID, err)
	}
}

// An elevation (a row with an end) never widens the pin and never becomes the run's context unasked: in the pin,
// run_in naming it picks the permanent pin context; outside the pin it is picked only when named, and the pin's
// check refuses it; with no run_in neither is a candidate.
func TestPortalPinContext_ElevationInAndOutOfPin(t *testing.T) {
	f := newPinFixture(t)
	ctx := f.personCtx(f.person)
	end := time.Now().Add(time.Hour)
	inPin, outside := f.heldRow(f.child, false, &end), f.heldRow(f.outside, false, &end)
	pick, err := f.pickFor(t, ctx, []store.MembershipMatch{inPin}, contextChoice{RunIn: f.child.ID})
	if err != nil || pick.unit.ID != f.child.ID || pick.row == nil || pick.row.Membership.ExpiresAt != nil {
		t.Errorf("run_in naming an in-pin elevation = %s %+v %v, want the permanent pin context (no end)", pick.unit.ID, pick.row, err)
	}
	pick, err = f.pickFor(t, ctx, []store.MembershipMatch{outside}, contextChoice{RunIn: f.outside.ID})
	if err != nil || pick.unit.ID != f.outside.ID {
		t.Fatalf("run_in naming an outside elevation = %s %v, want it picked for the pin's check", pick.unit.ID, err)
	}
	if r := f.srv.portalUnitRefusal(ctx, governanceCeiling{Unit: &ResolvedUnit{ID: f.outside.ID, Name: f.outside.Name}}, false); r == nil ||
		r.status != http.StatusForbidden || r.body.Reason != "portal_unit_scope" {
		t.Errorf("an outside elevation at the pin's check = %+v, want 403 portal_unit_scope", r)
	}
	pick, err = f.pickFor(t, ctx, []store.MembershipMatch{inPin, outside}, contextChoice{})
	if err != nil || pick.unit.ID != f.pin.ID || pick.row == nil || pick.row.Membership.ExpiresAt != nil {
		t.Errorf("no run_in with both elevations = %s %+v %v, want the pin's permanent context", pick.unit.ID, pick.row, err)
	}
}

// Requests that are not an issuer portal's own person's through a live, pinned portal get their rows back
// unchanged: a console identity, another issuer's person, a console portal, a revoked portal and a deleted pin.
func TestPortalPinContext_OtherCallersUnchanged(t *testing.T) {
	cases := map[string]func(f *portalPinFixture) context.Context{
		"console identity, delegated": func(f *portalPinFixture) context.Context { return f.personCtx("sub-console") },
		"console identity, not delegated": func(f *portalPinFixture) context.Context {
			return withHumanIdentity(context.Background(), "sub-console", "", oidc.RoleUser, types.UserTypeStandard, []string{}, false)
		},
		"another issuer's person": func(f *portalPinFixture) context.Context {
			return f.personCtx(oidc.PortalPrincipal("https://other-issuer.example", "uid"))
		},
		"console portal": func(f *portalPinFixture) context.Context {
			f.setPortal(func(d *types.Delegate) { d.Issuer, d.IssuerKey = "", "" })
			return f.personCtx(f.person)
		},
		"revoked portal": func(f *portalPinFixture) context.Context {
			at := time.Now()
			f.setPortal(func(d *types.Delegate) { d.RevokedAt = &at })
			return f.personCtx(f.person)
		},
		"deleted pin": func(f *portalPinFixture) context.Context {
			f.ps.units = []types.GovernanceUnit{{ID: store.DefaultUnitID, Name: "Default"}, f.child, f.outside}
			return f.personCtx(f.person)
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			f := newPinFixture(t)
			ctx := build(f)
			active := []store.MembershipMatch{f.heldRow(f.outside, true, nil)}
			got, err := f.srv.portalPinMemberships(ctx, active, contextChoice{})
			if err != nil || len(got) != 1 || got[0].Membership.ID != active[0].Membership.ID {
				t.Errorf("rows = %v %v, want the one actual row unchanged", unitIDs(got), err)
			}
		})
	}
}

// An unreadable portal row is never read as "no pin": the helper fails, and the launch answers 503.
func TestPortalPinContext_UnreadablePortal503(t *testing.T) {
	f := newPinFixture(t)
	f.srv.cfg.Store = unreadablePortalFake{f.ps}
	_, err := f.srv.portalPinMemberships(f.personCtx(f.person), nil, contextChoice{})
	if !errors.Is(err, errPortalUnitUnavailable) {
		t.Fatalf("an unreadable portal = %v, want errPortalUnitUnavailable", err)
	}
	if r := ceilingRunRefusal(err); r.status != http.StatusServiceUnavailable || r.body.Reason != reasonPortalUnitUnavailable {
		t.Errorf("the refusal = %d %q, want 503 %s", r.status, r.body.Reason, reasonPortalUnitUnavailable)
	}
}

// The pin contexts carry the units' full rows: a parent-less unit is composed from the row it is handed, so an
// outline would lose its walls.
func TestPortalPinContext_FullRowsCarryCeiling(t *testing.T) {
	f := newPinFixture(t)
	f.pin.Ceiling = types.RunPolicySpec{AllowedDomains: []string{"pin-only.example"}}
	f.ps.units = []types.GovernanceUnit{{ID: store.DefaultUnitID, Name: "Default"}, f.pin, f.child, f.outside}
	got, err := f.srv.portalPinMemberships(f.personCtx(f.person), nil, contextChoice{})
	if err != nil || len(got) == 0 || got[0].Unit.ID != f.pin.ID || len(got[0].Unit.Ceiling.AllowedDomains) != 1 ||
		got[0].Unit.Ceiling.AllowedDomains[0] != "pin-only.example" {
		t.Fatalf("the pin's context row = %+v %v, want the full row with its ceiling", got, err)
	}
}

// countingPinStore counts the portal row reads.
type countingPinStore struct {
	*pinPortalStore
	reads int
}

func (c *countingPinStore) GetDelegateByID(ctx context.Context, id uuid.UUID) (types.Delegate, error) {
	c.reads++
	return c.pinPortalStore.GetDelegateByID(ctx, id)
}

// A strict workspace pin or a pinned organisation component is judged on actual memberships only: the helper
// adds nothing and reads no portal row, and with no actual row the launch is refused as at 0.10.2. With an
// actual row in the pinned unit the pick is that unit.
func TestPortalPinContext_StrictPinNeedsActualMembership(t *testing.T) {
	for name, c := range map[string]struct {
		pin    contextPin
		reason string
	}{
		"strict workspace":       {contextPin{}, reasonWorkspaceUnitNotYours},
		"organisation component": {contextPin{Component: "pinned-tool"}, reasonComponentUnitPinned},
	} {
		t.Run(name, func(t *testing.T) {
			f := newPinFixture(t)
			cs := &countingPinStore{pinPortalStore: f.ps}
			f.srv.cfg.Store = cs
			ctx := f.personCtx(f.person)
			pin := c.pin
			pin.Unit = f.child.ID
			choice := contextChoice{StrictPins: []contextPin{pin}}
			got, err := f.srv.portalPinMemberships(ctx, nil, choice)
			if err != nil || len(got) != 0 || cs.reads != 0 {
				t.Fatalf("strict pin, no rows = %v %v (%d portal reads), want nothing added and no read", unitIDs(got), err, cs.reads)
			}
			_, _, err = f.srv.pickContext(ctx, got, choice)
			var notYours *errWorkspaceUnitNotYours
			if !errors.As(err, &notYours) || !notYours.strict || notYours.component != pin.Component {
				t.Fatalf("strict pin with no membership = %v, want the strict refusal", err)
			}
			if r := contextRunRefusal(err); r == nil || r.status != http.StatusForbidden || r.body.Reason != c.reason {
				t.Errorf("the refusal = %+v, want 403 %s", r, c.reason)
			}
			active := []store.MembershipMatch{f.heldRow(f.child, false, nil)}
			if pick, err := f.pickFor(t, ctx, active, choice); err != nil || pick.unit.ID != f.child.ID {
				t.Errorf("strict pin with an actual row in the child = %s %v, want the child", pick.unit.ID, err)
			}
		})
	}
}

// The owner re-check's stand-in passes only a run the live fixture portal started for its own person, with its
// pin on the run's path and no strict resource attached.
func TestPortalPinContext_PinHoldsOwner(t *testing.T) {
	f := newPinFixture(t)
	ctx := context.Background()
	via := f.delegate.ID
	run := types.AgentRun{ID: uuid.New(), CreatedBy: f.person, CreatedVia: &via, UnitID: &f.child.ID}
	path := map[uuid.UUID]bool{f.child.ID: true, f.pin.ID: true}
	if held, err := f.srv.portalPinHoldsOwner(ctx, run, path); err != nil || !held {
		t.Fatalf("a run the portal started under its pin = %v %v, want held", held, err)
	}
	plainWS, strictWS := uuid.New(), uuid.New()
	f.ast.mu.Lock()
	f.ast.workspaces[plainWS] = types.Workspace{ID: plainWS, Name: "plain", OwningUnitID: &f.child.ID}
	f.ast.workspaces[strictWS] = types.Workspace{ID: strictWS, Name: "strict", OwningUnitID: &f.child.ID, OwningUnitStrict: true}
	f.ast.mu.Unlock()
	withWS := func(ids ...uuid.UUID) types.AgentRun { r := run; r.WorkspaceIDs = ids; return r }
	if held, err := f.srv.portalPinHoldsOwner(ctx, withWS(plainWS), path); err != nil || !held {
		t.Errorf("a run with a plain-pinned workspace = %v %v, want held", held, err)
	}
	other := oidc.PortalPrincipal("https://other-issuer.example", "uid")
	for name, c := range map[string]struct {
		run  types.AgentRun
		path map[uuid.UUID]bool
	}{
		"no created_via":       {func() types.AgentRun { r := run; r.CreatedVia = nil; return r }(), path},
		"path without the pin": {run, map[uuid.UUID]bool{f.child.ID: true}},
		"console owner":        {func() types.AgentRun { r := run; r.CreatedBy = "sub-console"; return r }(), path},
		"another issuer's key": {func() types.AgentRun { r := run; r.CreatedBy = other; return r }(), path},
		"strict workspace":     {withWS(plainWS, strictWS), path},
		"deleted workspace":    {withWS(uuid.New()), path},
	} {
		if held, err := f.srv.portalPinHoldsOwner(ctx, c.run, c.path); err != nil || held {
			t.Errorf("%s = %v %v, want not held", name, held, err)
		}
	}
	at := time.Now()
	f.setPortal(func(d *types.Delegate) { d.RevokedAt = &at })
	if held, err := f.srv.portalPinHoldsOwner(ctx, run, path); err != nil || held {
		t.Errorf("a revoked portal = %v %v, want not held", held, err)
	}
}
