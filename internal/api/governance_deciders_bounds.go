// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A unit's deciders rule only narrows the rules above it for the same kind. The resolver meets every rule on
// the chain (deciderTree.ruleFor), and a write wider than the rules above is refused here. The bound is
// checked when a rule is written, never later:
//   - a rule above that is narrowed later refuses nothing below it, and the stored rule is narrowed at decide;
//   - a unit moved under a narrower parent keeps its rules, met at decide;
//   - a parent's write never checks the units below it.

// newDeciderTree is the rules of rows and the parent links of units.
func newDeciderTree(rows []types.UnitDeciders, units []types.GovernanceUnit) *deciderTree {
	t := &deciderTree{rules: map[uuid.UUID][]types.DeciderRule{}, parents: map[uuid.UUID]*uuid.UUID{}}
	for _, d := range rows {
		t.rules[d.UnitID] = d.Rules
	}
	for _, u := range units {
		t.parents[u.ID] = u.ParentID
	}
	return t
}

// chain is every rule for kind from unit up, nearest first, with the unit holding each. A unit the tree does
// not hold ends the walk, as a deleted unit leaves a run nothing to bind; so do a unit already walked and the
// depth cap.
func (t *deciderTree) chain(unit uuid.UUID, kind string) (rules []types.DeciderRule, units []uuid.UUID) {
	seen := map[uuid.UUID]bool{}
	cur := &unit
	for n := 0; cur != nil && !seen[*cur] && n < store.MaxUnitDepthCap; n++ {
		parent, exists := t.parents[*cur]
		if !exists {
			break
		}
		seen[*cur] = true
		if r, ok := types.DeciderRuleFor(t.rules[*cur], kind); ok {
			rules, units = append(rules, r), append(units, *cur)
		}
		cur = parent
	}
	return rules, units
}

// boundedBy is unit's view per kind: the nearest unit with a rule (from), every unit on the chain with one,
// nearest first (bounded), and their meet (effective). A kind no rule on the chain names is in none of them.
func (t *deciderTree) boundedBy(unit uuid.UUID) (from map[string]uuid.UUID, bounded map[string][]uuid.UUID, effective map[string]types.DeciderRule) {
	from, bounded, effective = map[string]uuid.UUID{}, map[string][]uuid.UUID{}, map[string]types.DeciderRule{}
	for _, k := range types.DeciderKinds {
		rules, units := t.chain(unit, k)
		if m, ok := types.MeetDeciderRules(k, rules); ok {
			from[k], bounded[k], effective[k] = units[0], units, m
		}
	}
	return from, bounded, effective
}

// deciderBoundRefusal is the 422 for rules that reach further than the rules above unit for the same
// kind: each rule's kinds are checked against the meet of the stored rules from unit's parent up. A
// unit with no rule above it for a kind is not bounded for it. Clearing rules is never refused. It binds
// every writer, the admin token included, like the deciders lock.
func (s *Server) deciderBoundRefusal(ctx context.Context, q store.Querier, unit types.GovernanceUnit, all []types.GovernanceUnit, rules []types.DeciderRule) error {
	if unit.ParentID == nil || len(rules) == 0 {
		return nil
	}
	rows, err := store.ListUnitDecidersQ(ctx, q)
	if err != nil {
		return err
	}
	t := newDeciderTree(rows, all)
	for i, r := range rules {
		if err := t.ruleWiderRefusal(i, r, *unit.ParentID, all); err != nil {
			return err
		}
	}
	return nil
}

// ruleWiderRefusal is the 422 for rules[i] when, for one of its kinds, it reaches further than the meet of the
// rules from parent up; nil when it is within them all.
func (t *deciderTree) ruleWiderRefusal(i int, r types.DeciderRule, parent uuid.UUID, all []types.GovernanceUnit) error {
	for _, k := range r.Kinds {
		chain, units := t.chain(parent, k)
		bound, ok := types.MeetDeciderRules(k, chain)
		if !ok {
			continue
		}
		if f := types.DeciderRuleWithin(k, r, bound); f != "" {
			return writeRefusal(http.StatusUnprocessableEntity, reasonDeciderRuleWiderThanParent,
				"rules[%d]: %s for %s reaches further than %s above this unit allows", i, f, k, unitsByID(all)[units[0]].Name)
		}
	}
	return nil
}
