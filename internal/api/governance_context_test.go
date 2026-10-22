// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// treeUnitsStore serves a unit list, for the workspace pins.
type treeUnitsStore struct {
	store.Store
	units []types.GovernanceUnit
}

func (s treeUnitsStore) ListGovernanceUnits(context.Context) ([]types.GovernanceUnit, error) {
	return s.units, nil
}

func treeMatch(u types.GovernanceUnit, st types.CapabilitySubjectType) store.MembershipMatch {
	return store.MembershipMatch{Membership: types.GovernanceMembership{SubjectType: st, UnitID: u.ID}, Unit: u}
}

// TestUnitContextRunInMustBeACandidate: a RunIn outside the candidates is errUnitNotAContext, a
// workspace pin no candidate is on the path of is errWorkspaceUnitNotYours, and a pin maps an
// ancestor candidate to itself while a descendant candidate keeps its own unit.
func TestUnitContextRunInMustBeACandidate(t *testing.T) {
	parent := types.GovernanceUnit{ID: uuid.New(), Name: "pin-parent", Kind: types.UnitKindUnit}
	child := types.GovernanceUnit{ID: uuid.New(), Name: "pin-child", Kind: types.UnitKindUnit, ParentID: &parent.ID, Overlay: &types.CeilingOverlay{}}
	off := types.GovernanceUnit{ID: uuid.New(), Name: "pin-off", Kind: types.UnitKindUnit, Contact: &policyref.Contact{Owner: "Off team"}}
	s := &Server{cfg: Config{Store: treeUnitsStore{units: []types.GovernanceUnit{parent, child, off}}}}
	ctx := context.Background()
	inParent := []store.MembershipMatch{treeMatch(parent, types.CapabilitySubjectUser)}
	inChild := []store.MembershipMatch{treeMatch(child, types.CapabilitySubjectUser)}

	if _, _, err := s.pickContext(ctx, inParent, contextChoice{RunIn: off.ID}); !errors.Is(err, errUnitNotAContext) {
		t.Errorf("RunIn outside the candidates: %v, want errUnitNotAContext", err)
	}
	if _, _, err := s.pickContext(ctx, inParent, contextChoice{RunIn: uuid.New()}); !errors.Is(err, errUnitNotAContext) {
		t.Errorf("RunIn naming no unit: %v, want errUnitNotAContext (the same answer)", err)
	}
	if p, _, err := s.pickContext(ctx, inParent, contextChoice{RunIn: parent.ID}); err != nil || p.unit.ID != parent.ID {
		t.Errorf("RunIn naming a candidate: %v %v, want pin-parent", p.unit.Name, err)
	}
	if p, _, err := s.pickContext(ctx, inParent, contextChoice{OwningUnits: []uuid.UUID{child.ID}}); err != nil || p.unit.ID != child.ID {
		t.Errorf("an ancestor candidate under a pin: %v %v, want the pin pin-child", p.unit.Name, err)
	}
	if p, _, err := s.pickContext(ctx, inChild, contextChoice{OwningUnits: []uuid.UUID{parent.ID}}); err != nil || p.unit.ID != child.ID {
		t.Errorf("a descendant candidate under a pin: %v %v, want itself pin-child", p.unit.Name, err)
	}
	_, _, err := s.pickContext(ctx, inChild, contextChoice{OwningUnits: []uuid.UUID{off.ID}})
	var notYours *errWorkspaceUnitNotYours
	if !errors.As(err, &notYours) || notYours.Unit != off.ID {
		t.Fatalf("a pin no candidate is on the path of: %v, want errWorkspaceUnitNotYours{pin-off}", err)
	}
	if ref := contextRunRefusal(err); ref == nil || ref.body.Policy == nil || ref.body.Reason != reasonWorkspaceUnitNotYours {
		t.Errorf("the refusal = %+v, want %s with pin-off's contact on its policy", ref, reasonWorkspaceUnitNotYours)
	}
}

// TestUnitContextMemoKeysOnChoice: one request's memo answers each choice once, and two choices are
// two resolves with two answers.
func TestUnitContextMemoKeysOnChoice(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	m := &ceilingMemo{}
	calls := 0
	resolve := func(ctx context.Context) (governanceCeiling, error) {
		calls++
		return governanceCeiling{Unit: &ResolvedUnit{ID: contextChoiceFrom(ctx).RunIn}}, nil
	}
	ctx := context.Background()
	for range 2 {
		ga, _ := m.do(withContextChoice(ctx, contextChoice{RunIn: a}), resolve)
		gb, _ := m.do(withContextChoice(ctx, contextChoice{RunIn: b}), resolve)
		if ga.Unit.ID != a || gb.Unit.ID != b {
			t.Fatalf("answers %s and %s, want %s and %s", ga.Unit.ID, gb.Unit.ID, a, b)
		}
	}
	if calls != 2 {
		t.Errorf("%d resolves for two choices asked twice each, want 2", calls)
	}
}

// TestUnitContextChoiceSlot: a choice set through the slot after the request was handed on is what
// a later resolve on that request reads, and the two choice refusals answer 403 with their reasons on
// the path create, preflight and the preview share (ceilingRunRefusal).
func TestUnitContextChoiceSlot(t *testing.T) {
	r := withContextChoiceSlot(httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil))
	if got := contextChoiceFrom(r.Context()); got.RunIn != uuid.Nil {
		t.Fatalf("an empty slot reads %+v, want no choice", got)
	}
	want := contextChoice{RunIn: uuid.New(), OwningUnits: []uuid.UUID{uuid.New()}}
	setContextChoice(r.Context(), want)
	if got := contextChoiceFrom(r.Context()); got.key() != want.key() {
		t.Errorf("after setContextChoice the request reads %+v, want %+v", got, want)
	}
	m := &ceilingMemo{}
	c, _ := m.do(r.Context(), func(ctx context.Context) (governanceCeiling, error) {
		return governanceCeiling{Unit: &ResolvedUnit{ID: contextChoiceFrom(ctx).RunIn}}, nil
	})
	if c.Unit.ID != want.RunIn {
		t.Errorf("a resolve on the request answered for %s, want the slot's %s", c.Unit.ID, want.RunIn)
	}
	for err, reason := range map[error]string{
		errUnitNotAContext: reasonUnitNotAContext,
		&errWorkspaceUnitNotYours{Unit: uuid.New(), name: "ws-unit", contact: &policyref.Contact{Owner: "WS team"}}: reasonWorkspaceUnitNotYours,
	} {
		w := httptest.NewRecorder()
		ceilingRunRefusal(err).write(&Server{}, w, r)
		var body errorBody
		if w.Code != http.StatusForbidden || json.Unmarshal(w.Body.Bytes(), &body) != nil || body.Reason != reason {
			t.Errorf("%v answered %d %s, want 403 %s", err, w.Code, w.Body, reason)
		}
	}
}
