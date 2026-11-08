// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// unitDecidersRead is one entry of GET /governance's deciders as this package reads it: the unit and its
// own rules. Inherited rules (from, effective) are the unit above's and are not read.
type unitDecidersRead struct {
	UnitID uuid.UUID     `json:"unit_id"`
	Rules  []DeciderRule `json:"rules"`
}

// decidersByUnitName is the read's deciders as a document section: each unit's own rules by unit name
// (the id when no unit of units has it). A unit that only inherits is left out; nil when no unit has rules.
func decidersByUnitName(units []GovernanceUnit, read []unitDecidersRead) map[string][]DeciderRule {
	var out map[string][]DeciderRule
	for _, d := range read {
		if len(d.Rules) == 0 {
			continue
		}
		if out == nil {
			out = map[string][]DeciderRule{}
		}
		name := d.UnitID.String()
		if i := slices.IndexFunc(units, func(u GovernanceUnit) bool { return u.ID == d.UnitID }); i >= 0 {
			name = units[i].Name
		}
		out[name] = d.Rules
	}
	return out
}

// deciderWrite is one unit's deciders write: its name, its id on the server (Nil for a unit this apply
// creates) and the canonical rules to put (none for a clear).
type deciderWrite struct {
	name  string
	id    uuid.UUID
	rules []DeciderRule
}

// deciderPlan is the deciders section's writes against current: the units whose rules change, parents
// before children, then (under prune) the units whose rules the section omits, children first.
type deciderPlan struct {
	updates, clears []deciderWrite
	unchanged       int
	parent          map[string]string // unit name → the name of the unit above it, from the document and the server
}

// newDeciderPlan matches doc's deciders section against current's. An absent section plans nothing; a unit
// the document and the server both lack, or a rule the server would refuse, is an error.
func newDeciderPlan(current, doc GovernanceDocument, prune bool) (deciderPlan, error) {
	p := deciderPlan{parent: unitParents(current, doc)}
	if doc.Deciders == nil {
		return p, nil
	}
	known := map[string]uuid.UUID{}
	for _, u := range doc.Units {
		known[u.Name] = uuid.Nil
	}
	for _, u := range current.Units {
		known[u.Name] = u.ID
	}
	for _, name := range p.byDepth(mapKeys(doc.Deciders), false) {
		id, ok := known[name]
		if !ok {
			return p, fmt.Errorf("deciders names unit %q, which neither the document nor the server has", name)
		}
		rules, err := canonicalDeciderRules(doc.Deciders[name])
		if err != nil {
			return p, fmt.Errorf("deciders of %q: %w", name, err)
		}
		if stored, err := canonicalDeciderRules(current.Deciders[name]); err == nil && reflect.DeepEqual(stored, rules) {
			p.unchanged++
			continue
		}
		p.updates = append(p.updates, deciderWrite{name: name, id: id, rules: rules})
	}
	if !prune {
		return p, nil
	}
	for _, name := range p.byDepth(mapKeys(current.Deciders), true) {
		// A key that names no unit (an id the read could not name) cannot be addressed, so it is left alone.
		if _, kept := doc.Deciders[name]; !kept && known[name] != uuid.Nil && len(current.Deciders[name]) > 0 {
			p.clears = append(p.clears, deciderWrite{name: name, id: known[name]})
		}
	}
	return p, nil
}

// canonicalDeciderRules is rules as the server stores them (types.ValidateDeciderRules), with an empty
// list as nil so an absent field and an empty one compare equal.
func canonicalDeciderRules(rules []DeciderRule) ([]DeciderRule, error) {
	out, err := types.ValidateDeciderRules(rules)
	if err != nil || len(out) == 0 {
		return nil, err
	}
	for i := range out {
		if len(out[i].EligibleRequesters) == 0 {
			out[i].EligibleRequesters = nil
		}
	}
	return out, nil
}

func mapKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// unitParents maps each unit name to the name of the unit above it: the document's parent where the
// document carries the unit, else the server's.
func unitParents(current, doc GovernanceDocument) map[string]string {
	idToName := make(map[uuid.UUID]string, len(current.Units)+len(doc.Units))
	for _, u := range current.Units {
		idToName[u.ID] = u.Name
	}
	for _, u := range doc.Units {
		if u.ID != uuid.Nil {
			idToName[u.ID] = u.Name
		}
	}
	out := map[string]string{}
	for _, units := range [][]GovernanceUnit{current.Units, doc.Units} {
		for _, u := range units {
			delete(out, u.Name)
			if u.ParentID != nil && idToName[*u.ParentID] != "" {
				out[u.Name] = idToName[*u.ParentID]
			}
		}
	}
	return out
}

// ancestors is name's units above it, nearest first, bounded by the unit count as governancePruneOrder
// bounds its walk: the server refuses cycles, and a bad document must not hang.
func (p deciderPlan) ancestors(name string) []string {
	var out []string
	for up, ok := p.parent[name]; ok && len(out) <= len(p.parent); up, ok = p.parent[up] {
		out = append(out, up)
	}
	return out
}

// byDepth is names with parents before children (deepestFirst reverses that), then by name.
func (p deciderPlan) byDepth(names []string, deepestFirst bool) []string {
	slices.SortFunc(names, func(a, b string) int {
		d := cmp.Compare(len(p.ancestors(a)), len(p.ancestors(b)))
		if deepestFirst {
			d = -d
		}
		return cmp.Or(d, strings.Compare(a, b))
	})
	return names
}

func (w deciderWrite) step(op string, id uuid.UUID) ApplyStep {
	s := ApplyStep{Kind: "deciders", Op: op, Name: w.name, Fields: []string{"rules"}}
	if op == "delete" {
		s.Fields = nil
	}
	if id != uuid.Nil {
		s.ID = &id
	}
	return s
}

// planUnitDeciders is the deciders steps an apply of doc would make against current: the updates in
// depth order, then, with prune, the clears. unitID names a unit's id on the server.
func planUnitDeciders(current, doc GovernanceDocument, unitID func(string) (uuid.UUID, bool), prune bool) ([]ApplyStep, int, error) {
	p, err := newDeciderPlan(current, doc, prune)
	if err != nil {
		return nil, 0, err
	}
	var steps []ApplyStep
	for _, w := range p.updates {
		id, _ := unitID(w.name)
		steps = append(steps, w.step("update", id))
	}
	for _, w := range p.clears {
		steps = append(steps, w.step("delete", w.id))
	}
	return steps, p.unchanged, nil
}

// applyUnitDeciders puts each changed unit's rules, parents first. A unit whose own write is pending is
// deferred. A child refused as wider than its parent while an ancestor's deciders write of this apply is
// held waits for that change (Deferred, Base the ancestor); any other refusal is the unit's failed step.
func (c *Client) applyUnitDeciders(ctx context.Context, p deciderPlan, unitID func(string) (uuid.UUID, bool), pendingProfile map[string]bool, res *GovernanceApplyResult) error {
	held := map[string]string{} // unit → the unit whose held change its write waits on (itself when its own is held)
	for _, w := range p.updates {
		if pendingProfile[w.name] {
			res.Deferred = append(res.Deferred, GovernanceDeferredWrite{Kind: "deciders", Unit: w.name})
			continue
		}
		id, ok := unitID(w.name)
		step := w.step("update", id)
		if !ok {
			err := fmt.Errorf("apply deciders of %q: no unit named %q in the document or on the server", w.name, w.name)
			res.Steps = append(res.Steps, step.withOutcome(false, err))
			return err
		}
		_, pending, err := c.PutUnitDeciders(ctx, id, w.rules)
		if base := p.heldAncestor(w.name, held); base != "" && widerThanParent(err) {
			step.Outcome, step.Error = "deferred", fmt.Sprintf("waits for %s's held change; run set again after it is approved", base)
			res.Steps = append(res.Steps, step)
			res.Deferred = append(res.Deferred, GovernanceDeferredWrite{Kind: "deciders", Unit: w.name, Base: base})
			held[w.name] = base
			continue
		}
		if err != nil {
			err = fmt.Errorf("apply deciders of %q: %w", w.name, err)
		}
		res.Steps = append(res.Steps, step.withOutcome(pending != nil, err))
		if err != nil {
			return err
		}
		if pending != nil {
			res.Pending = append(res.Pending, *pending)
			held[w.name] = w.name
		}
	}
	return nil
}

// heldAncestor is the unit whose held deciders change in this apply the nearest held or deferred unit
// above name waits on, "" when there is none.
func (p deciderPlan) heldAncestor(name string, held map[string]string) string {
	for _, up := range p.ancestors(name) {
		if base := held[up]; base != "" {
			return base
		}
	}
	return ""
}

func widerThanParent(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusUnprocessableEntity && apiErr.Reason == "decider_rule_wider_than_parent"
}

// pruneUnitDeciders clears the rules of every unit the section omits, children first. It stops at the
// first clear the server holds (recorded in res.Pending) and reports skipped.
func (c *Client) pruneUnitDeciders(ctx context.Context, p deciderPlan, res *GovernanceApplyResult) (skipped bool, err error) {
	for _, w := range p.clears {
		_, pending, err := c.PutUnitDeciders(ctx, w.id, nil)
		if err != nil {
			err = fmt.Errorf("prune deciders of %q: %w", w.name, err)
		}
		res.Steps = append(res.Steps, w.step("delete", w.id).withOutcome(pending != nil, err))
		if err != nil {
			return false, err
		}
		if pending != nil {
			res.Pending = append(res.Pending, *pending)
			return true, nil
		}
	}
	return false, nil
}

// applyUnitSections is an apply's unit admins, then its deciders rules (after the unit and membership
// writes, before the restrictions).
func (c *Client) applyUnitSections(ctx context.Context, admins adminPlan, docAdmins map[string][]GovernanceAdminSubject, deciders deciderPlan,
	pendingProfile map[string]bool, res *GovernanceApplyResult) error {
	if err := c.applyUnitAdmins(ctx, admins, docAdmins, res); err != nil {
		return err
	}
	return c.applyUnitDeciders(ctx, deciders, admins.unitID, pendingProfile, res)
}

// pruneUnitSections is a prune's revokes of the omitted unit admins, then its clears of the omitted
// deciders rules, before any membership or unit delete, so a unit about to be pruned is cleared first. It
// reports skipped at the first write the server holds, as pruneUnitAdmins does.
func (c *Client) pruneUnitSections(ctx context.Context, admins adminPlan, docAdmins map[string][]GovernanceAdminSubject, deciders deciderPlan,
	res *GovernanceApplyResult) (skipped bool, err error) {
	if skipped, err = c.pruneUnitAdmins(ctx, admins, docAdmins, res); skipped || err != nil {
		return skipped, err
	}
	return c.pruneUnitDeciders(ctx, deciders, res)
}

// PlanGovernanceUnder and ApplyGovernanceResultUnder are PlanGovernance and ApplyGovernanceResult narrowed to
// the unit under names (a name or an id) and every unit below it.
func (c *Client) PlanGovernanceUnder(ctx context.Context, doc GovernanceDocument, under string, prune bool) (GovernancePlan, error) {
	current, err := c.GetGovernance(ctx)
	if err != nil {
		return GovernancePlan{}, fmt.Errorf("read current governance state: %w", err)
	}
	narrowed, err := narrowUnder(current, withoutExpiring(doc), under)
	if err != nil {
		return GovernancePlan{}, err
	}
	return planGovernance(narrowed, doc, prune)
}

// ApplyGovernanceResultUnder: see PlanGovernanceUnder. A document entry outside the subtree is refused
// before any write, and --prune only ever compares against the subtree's rows.
func (c *Client) ApplyGovernanceResultUnder(ctx context.Context, doc GovernanceDocument, under string, prune bool) (GovernanceApplyResult, error) {
	doc = withoutExpiring(doc) // elevations are granted through requests, never written or pruned here
	current, err := c.GetGovernance(ctx)
	if err != nil {
		return GovernanceApplyResult{}, fmt.Errorf("read current governance state: %w", err)
	}
	narrowed, err := narrowUnder(withoutExpiring(current), doc, under)
	if err != nil {
		return GovernanceApplyResult{}, err
	}
	res, err := c.applyGovernanceTo(ctx, narrowed, doc, prune)
	if sub, serr := subtreeOf(res.Document, under); serr == nil { // the read after the apply, narrowed as `get --under` prints it
		res.Document = sub.narrow(res.Document)
	}
	return res, err
}

// GetGovernanceUnder is GetGovernance narrowed the same way (what `get --under` prints).
func (c *Client) GetGovernanceUnder(ctx context.Context, under string) (GovernanceDocument, error) {
	current, err := c.GetGovernance(ctx)
	if err != nil {
		return GovernanceDocument{}, err
	}
	sub, err := subtreeOf(current, under)
	if err != nil {
		return GovernanceDocument{}, err
	}
	return sub.narrow(current), nil
}

// unitSubtree is a unit and every unit below it on the server, by id and by name.
type unitSubtree struct {
	root  GovernanceUnit
	ids   map[uuid.UUID]bool
	names map[string]bool
}

// subtreeOf resolves under among current's units (an id match wins over a name) and walks its subtree.
func subtreeOf(current GovernanceDocument, under string) (unitSubtree, error) {
	i := -1
	if id, err := uuid.Parse(under); err == nil {
		i = slices.IndexFunc(current.Units, func(u GovernanceUnit) bool { return u.ID == id })
	}
	if i < 0 {
		i = slices.IndexFunc(current.Units, func(u GovernanceUnit) bool { return u.Name == under })
	}
	if i < 0 {
		return unitSubtree{}, fmt.Errorf("no unit %q in the governance you can read", under)
	}
	byID := make(map[uuid.UUID]GovernanceUnit, len(current.Units))
	for _, u := range current.Units {
		byID[u.ID] = u
	}
	sub := unitSubtree{root: current.Units[i], ids: map[uuid.UUID]bool{}, names: map[string]bool{}}
	for _, u := range current.Units {
		at := u
		for d := 0; d <= len(current.Units); d++ { // bounded: the server refuses cycles, a bad read must not hang
			if at.ID == sub.root.ID {
				sub.ids[u.ID], sub.names[u.Name] = true, true
				break
			}
			next, ok := GovernanceUnit{}, false
			if at.ParentID != nil {
				next, ok = byID[*at.ParentID]
			}
			if !ok {
				break
			}
			at = next
		}
	}
	return sub, nil
}

// narrow is current with only the subtree's units, memberships, admins, deciders and grants, and no
// restrictions (they are organisation-wide).
func (s unitSubtree) narrow(current GovernanceDocument) GovernanceDocument {
	out := GovernanceDocument{MaxDepth: current.MaxDepth, Units: []GovernanceUnit{}, Memberships: []GovernanceMembership{}}
	for _, u := range current.Units {
		if s.ids[u.ID] {
			out.Units = append(out.Units, u)
		}
	}
	for _, m := range current.Memberships {
		if s.ids[m.UnitID] {
			out.Memberships = append(out.Memberships, m)
		}
	}
	for _, g := range current.grants {
		if s.ids[g.UnitID] {
			out.grants = append(out.grants, g)
		}
	}
	out.Admins = filterKeys(current.Admins, s.names)
	out.Deciders = filterKeys(current.Deciders, s.names)
	return out
}

// filterKeys is m with only the keys keep holds; nil when none is left.
func filterKeys[V any](m map[string]V, keep map[string]bool) map[string]V {
	var out map[string]V
	for k, v := range m {
		if keep[k] {
			if out == nil {
				out = map[string]V{}
			}
			out[k] = v
		}
	}
	return out
}

// narrowUnder is current narrowed to under's subtree, after refusing every document entry outside it.
func narrowUnder(current, doc GovernanceDocument, under string) (GovernanceDocument, error) {
	sub, err := subtreeOf(current, under)
	if err != nil {
		return GovernanceDocument{}, err
	}
	inside := sub.acceptNewUnits(current, doc)
	var refused []string
	refused = append(refused, sub.outsideUnits(current, doc, inside)...)
	refused = append(refused, sub.outsideEntries(current, doc, inside)...)
	if len(doc.Restrictions) > 0 {
		refused = append(refused, "restrictions are organisation-wide; write them without --under")
	}
	if !slices.ContainsFunc(doc.Units, func(u GovernanceUnit) bool { return u.Name == sub.root.Name }) {
		refused = append(refused, fmt.Sprintf("the document must include %q, the unit --under names", sub.root.Name))
	}
	if len(refused) > 0 {
		return GovernanceDocument{}, fmt.Errorf("the document reaches outside %q:\n  %s", sub.root.Name, strings.Join(refused, "\n  "))
	}
	return sub.narrow(current), nil
}

// acceptNewUnits is the subtree's unit names plus every document unit the server lacks whose parent is in
// the subtree or is such a unit itself.
func (s unitSubtree) acceptNewUnits(current, doc GovernanceDocument) map[string]bool {
	inside := make(map[string]bool, len(s.names))
	for name := range s.names {
		inside[name] = true
	}
	onServer := map[string]bool{}
	for _, u := range current.Units {
		onServer[u.Name] = true
	}
	parents := unitParents(current, doc)
	for changed := true; changed; {
		changed = false
		for _, u := range doc.Units {
			if !onServer[u.Name] && !inside[u.Name] && inside[parents[u.Name]] {
				inside[u.Name], changed = true, true
			}
		}
	}
	return inside
}

// outsideUnits names each document unit outside the subtree: one the server holds outside it, or a new
// one whose parent is not in it.
func (s unitSubtree) outsideUnits(current, doc GovernanceDocument, inside map[string]bool) []string {
	onServer := map[string]bool{}
	for _, u := range current.Units {
		onServer[u.Name] = true
	}
	var out []string
	for _, u := range doc.Units {
		switch {
		case inside[u.Name]:
		case onServer[u.Name]:
			out = append(out, fmt.Sprintf("unit %q is outside %q", u.Name, s.root.Name))
		default:
			out = append(out, fmt.Sprintf("unit %q is new and its parent is not %q or a unit below it", u.Name, s.root.Name))
		}
	}
	return out
}

// outsideEntries names each membership, admins key and deciders key on a unit outside the subtree.
func (s unitSubtree) outsideEntries(current, doc GovernanceDocument, inside map[string]bool) []string {
	idToName := map[uuid.UUID]string{}
	for _, units := range [][]GovernanceUnit{current.Units, doc.Units} {
		for _, u := range units {
			if u.ID != uuid.Nil {
				idToName[u.ID] = u.Name
			}
		}
	}
	var out []string
	for _, m := range doc.Memberships {
		if name := cmpOr(idToName[m.UnitID], m.UnitID.String()); !inside[name] {
			out = append(out, fmt.Sprintf("membership %s %q is on unit %q, outside %q", m.SubjectType, m.Subject, name, s.root.Name))
		}
	}
	for _, name := range sortedUnitNames(doc.Admins) {
		if !inside[name] {
			out = append(out, fmt.Sprintf("admins names unit %q, outside %q", name, s.root.Name))
		}
	}
	keys := mapKeys(doc.Deciders)
	slices.Sort(keys)
	for _, name := range keys {
		if !inside[name] {
			out = append(out, fmt.Sprintf("deciders names unit %q, outside %q", name, s.root.Name))
		}
	}
	return out
}
