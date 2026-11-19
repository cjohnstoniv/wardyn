// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// pinTree is Root > Prod > ProdEU, and Dev beside Prod, over a store that serves the unit list.
type pinTree struct {
	s                   *Server
	root, prod, eu, dev types.GovernanceUnit
	inRoot, inEU, inDev []store.MembershipMatch
	prodContact         *policyref.Contact
}

func newPinTree() pinTree {
	var p pinTree
	p.prodContact = &policyref.Contact{Owner: "Prod team"}
	p.root = types.GovernanceUnit{ID: uuid.New(), Name: "Root", Kind: types.UnitKindUnit}
	p.prod = types.GovernanceUnit{ID: uuid.New(), Name: "Prod", Kind: types.UnitKindUnit, ParentID: &p.root.ID, Contact: p.prodContact}
	p.eu = types.GovernanceUnit{ID: uuid.New(), Name: "ProdEU", Kind: types.UnitKindUnit, ParentID: &p.prod.ID}
	p.dev = types.GovernanceUnit{ID: uuid.New(), Name: "Dev", Kind: types.UnitKindUnit, ParentID: &p.root.ID}
	p.s = &Server{cfg: Config{Store: treeUnitsStore{units: []types.GovernanceUnit{p.root, p.prod, p.eu, p.dev}}}}
	p.inRoot = []store.MembershipMatch{treeMatch(p.root, types.CapabilitySubjectUser)}
	p.inEU = []store.MembershipMatch{treeMatch(p.eu, types.CapabilitySubjectUser)}
	p.inDev = []store.MembershipMatch{treeMatch(p.dev, types.CapabilitySubjectUser)}
	return p
}

func (p pinTree) componentPin(name string) contextPin {
	return contextPin{Unit: p.prod.ID, Component: name}
}

// TestComponentPinRefusesOutsideUnit: a member of Dev attaching a component pinned to Prod is refused
// component_unit_pinned, naming Prod with its contact; a member of ProdEU runs in ProdEU.
func TestComponentPinRefusesOutsideUnit(t *testing.T) {
	p := newPinTree()
	ctx := context.Background()
	choice := contextChoice{StrictPins: []contextPin{p.componentPin("Prod Tool")}}
	_, _, err := p.s.pickContext(ctx, p.inDev, choice)
	var notYours *errWorkspaceUnitNotYours
	if !errors.As(err, &notYours) || notYours.Unit != p.prod.ID || notYours.component != "Prod Tool" {
		t.Fatalf("a Dev member under a Prod component pin: %v, want the component pin refused", err)
	}
	ref := contextRunRefusal(err)
	if ref == nil || ref.status != 403 || ref.body.Reason != reasonComponentUnitPinned {
		t.Fatalf("the refusal = %+v, want 403 %s", ref, reasonComponentUnitPinned)
	}
	if !strings.Contains(ref.body.Error, `"Prod Tool"`) || !strings.Contains(ref.body.Error, `"Prod"`) || ref.body.Policy == nil {
		t.Errorf("the refusal = %q policy %+v, want the component, the unit Prod and its contact", ref.body.Error, ref.body.Policy)
	}
	pick, _, err := p.s.pickContext(ctx, p.inEU, choice)
	if err != nil || pick.unit.ID != p.eu.ID {
		t.Errorf("a ProdEU member: %s %v, want ProdEU", pick.unit.Name, err)
	}
}

// TestComponentPinStrictAgainstAncestor: a member whose only context is Root, above the pin, is refused:
// a component pin has no ancestor mapping.
func TestComponentPinStrictAgainstAncestor(t *testing.T) {
	p := newPinTree()
	_, _, err := p.s.pickContext(context.Background(), p.inRoot, contextChoice{StrictPins: []contextPin{p.componentPin("Prod Tool")}})
	if ref := contextRunRefusal(err); ref == nil || ref.body.Reason != reasonComponentUnitPinned {
		t.Errorf("a Root-only member under a Prod component pin: %v, want %s", err, reasonComponentUnitPinned)
	}
	// A strict workspace pin answers with the workspace's reason and the strict text.
	_, _, err = p.s.pickContext(context.Background(), p.inRoot, contextChoice{StrictPins: []contextPin{{Unit: p.prod.ID}}})
	ref := contextRunRefusal(err)
	if ref == nil || ref.body.Reason != reasonWorkspaceUnitNotYours || !strings.Contains(ref.body.Error, "only runs in that unit or a unit below it") {
		t.Errorf("a Root-only member under a strict workspace pin: %+v, want %s with the strict text", ref, reasonWorkspaceUnitNotYours)
	}
}

// TestStrictPinsJudgedBeforePlain: a plain workspace pin at Prod would map a Root member down to Prod;
// a component pinned to Prod as well must still refuse them. Without the component, the plain pin
// runs them in Prod as before.
func TestStrictPinsJudgedBeforePlain(t *testing.T) {
	p := newPinTree()
	ctx := context.Background()
	both := contextChoice{OwningUnits: []uuid.UUID{p.prod.ID}, StrictPins: []contextPin{p.componentPin("Prod Tool")}}
	if _, _, err := p.s.pickContext(ctx, p.inRoot, both); contextRunRefusal(err) == nil || contextRunRefusal(err).body.Reason != reasonComponentUnitPinned {
		t.Errorf("a Root member with a plain Prod workspace pin and a Prod component pin: %v, want %s", err, reasonComponentUnitPinned)
	}
	pick, _, err := p.s.pickContext(ctx, p.inRoot, contextChoice{OwningUnits: []uuid.UUID{p.prod.ID}})
	if err != nil || pick.unit.ID != p.prod.ID {
		t.Errorf("a Root member with only the plain pin: %s %v, want Prod (unchanged)", pick.unit.Name, err)
	}
	// An unknown strict pin refuses rather than passing.
	if _, _, err := p.s.pickContext(ctx, p.inEU, contextChoice{StrictPins: []contextPin{{Unit: uuid.New()}}}); contextRunRefusal(err) == nil {
		t.Errorf("an unknown strict pin: %v, want refused", err)
	}
}

// TestContextChoiceKeysOnStrictPins: two choices that differ only in their strict pins are two memo keys.
func TestContextChoiceKeysOnStrictPins(t *testing.T) {
	a := contextChoice{StrictPins: []contextPin{{Unit: uuid.New()}}}
	if a.key() == (contextChoice{}).key() {
		t.Error("a strict pin does not change the memo key")
	}
}

// TestQueuedStartKeepsStrictPins (no PG half): the queued launch carries the strict pins through its
// JSON, and a launch stored before them decodes to none.
func TestQueuedStartKeepsStrictPins(t *testing.T) {
	p := newPinTree()
	pin := p.componentPin("Prod Tool")
	raw, err := json.Marshal(queuedLaunch{StrictPins: []contextPin{pin}})
	if err != nil {
		t.Fatal(err)
	}
	var back queuedLaunch
	if err := json.Unmarshal(raw, &back); err != nil || !slices.Equal(back.StrictPins, []contextPin{pin}) {
		t.Fatalf("round trip = %+v %v, want the pin kept", back.StrictPins, err)
	}
	var old queuedLaunch
	if err := json.Unmarshal([]byte(`{"request":{},"owning_units":[]}`), &old); err != nil || old.StrictPins != nil {
		t.Errorf("an older launch = %+v %v, want no strict pins", old.StrictPins, err)
	}
	ctx := context.Background()
	if pick, _, err := p.s.pickContext(ctx, p.inEU, contextChoice{StrictPins: back.StrictPins}); err != nil || pick.unit.ID != p.eu.ID {
		t.Errorf("a queued start for a ProdEU member: %s %v, want ProdEU", pick.unit.Name, err)
	}
	if _, _, err := p.s.pickContext(ctx, p.inDev, contextChoice{StrictPins: back.StrictPins}); contextRunRefusal(err) == nil {
		t.Errorf("a queued start for a Dev member: %v, want refused as at create", err)
	}
}

// TestWorkspaceStrictPinState: the base state carries the strict key only when strict, so a change
// proposed before the switch hashes as it did.
func TestWorkspaceStrictPinState(t *testing.T) {
	u := uuid.New()
	if computeETag(owningUnitState(&u, false)) != computeETag(map[string]any{"owning_unit_id": &u}) {
		t.Error("a plain pin's state is not the older build's state")
	}
	if computeETag(owningUnitState(&u, true)) == computeETag(owningUnitState(&u, false)) {
		t.Error("strictness does not change the state")
	}
	if owningUnitStrictNext(nil, true, &u) != true || owningUnitStrictNext(nil, true, nil) || owningUnitStrictNext(boolPtr(false), true, &u) {
		t.Error("strict resolution: absent keeps, a null unit clears, false turns it off")
	}
	units := []types.GovernanceUnit{{ID: u}}
	if !workspacePinNarrows(units, &u, &u, false, true) || workspacePinNarrows(units, &u, &u, true, false) {
		t.Error("plain to strict narrows, strict to plain does not")
	}
}
