// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"maps"
	"reflect"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// policyKnobFields is the top-level json name of each types.RunPolicySpec
// field, by field index ("" for one with none): the keys of a run's provenance.
var policyKnobFields = func() []string {
	t := reflect.TypeFor[types.RunPolicySpec]()
	out := make([]string, t.NumField())
	for i := range t.NumField() {
		if name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ","); name != "-" {
			out[i] = name
		}
	}
	return out
}()

// knobProvenance is which layer set each knob of the policy a run resolves.
// It is written by the steps that decide a value, as each one runs (start, then
// setBy around every narrowing step of resolveRunPolicyFacts), never rebuilt
// from the final spec. What launch adds after resolution (workspace, component
// and model hosts) and what dispatch re-asserts are not layers; GET
// /runs/{id}/policy lists them under changes. Contacts are never held here,
// because the map rides the run.policy.resolve row and the audit records no
// contact; withContacts adds them on read.
type knobProvenance map[string]policyref.Provenance

// startProvenance gives every knob rec: the layer the spec started from.
func startProvenance(rec policyref.Provenance) knobProvenance {
	k := make(knobProvenance, len(policyKnobFields))
	for _, name := range policyKnobFields {
		if name != "" {
			k[name] = rec
		}
	}
	return k
}

// step runs f, one step that may change *spec, and records rec on every knob it
// changed: the knobs that step decided.
func (k knobProvenance) step(rec policyref.Provenance, spec *types.RunPolicySpec, f func()) {
	before := spec.Clone()
	f()
	k.setBy(rec, before, *spec)
}

// setBy records rec on every knob whose value differs from before to after.
// Both sides are compared canonical (canonicalKnobs), so a step that only
// reorders, re-cases or dedupes a list, or normalizes an unset
// first_use_approval, changed nothing.
func (k knobProvenance) setBy(rec policyref.Provenance, before, after types.RunPolicySpec) {
	if k == nil {
		return
	}
	before, after = canonicalKnobs(before), canonicalKnobs(after)
	bv, av := reflect.ValueOf(before), reflect.ValueOf(after)
	for i, name := range policyKnobFields {
		if name != "" && !reflect.DeepEqual(bv.Field(i).Interface(), av.Field(i).Interface()) {
			k[name] = rec
		}
	}
}

// resetUnchanged records rec on every knob whose value is the same at after as at before, compared
// as setBy compares: a value the steps between them changed and changed back is before's again.
func (k knobProvenance) resetUnchanged(rec policyref.Provenance, before, after types.RunPolicySpec) {
	changed := knobProvenance{}
	changed.setBy(rec, before, after)
	for name := range k {
		if _, ok := changed[name]; !ok {
			k[name] = rec
		}
	}
}

// canonicalKnobs is a clone of s in the form composer's merges leave it: host
// and method lists case-folded, trimmed, sorted and deduped (composer.union),
// push_rules paths sorted and deduped by exact bytes (composer.unionPaths), an
// empty list absent, and first_use_approval normalized.
func canonicalKnobs(s types.RunPolicySpec) types.RunPolicySpec {
	s = s.Clone()
	fold := func(xs []string) []string {
		for i, x := range xs {
			xs[i] = strings.ToLower(strings.TrimSpace(x))
		}
		slices.Sort(xs)
		return slices.Compact(xs)
	}
	s.AllowedDomains, s.DeniedDomains, s.AllowedMethods = fold(s.AllowedDomains), fold(s.DeniedDomains), fold(s.AllowedMethods)
	if s.PushRules != nil {
		slices.Sort(s.PushRules.DenyPaths)
		slices.Sort(s.PushRules.RequireReviewPaths)
		s.PushRules.DenyPaths, s.PushRules.RequireReviewPaths = slices.Compact(s.PushRules.DenyPaths), slices.Compact(s.PushRules.RequireReviewPaths)
	}
	s.FirstUseApproval = s.FirstUseApproval.Normalize()
	return s
}

// The records a resolution writes. None is locked here: markLocked marks the knobs a lock above the
// run's unit fixes. The name is the leaf profile's, never a profile it is built from.
var (
	memberProvenance     = policyref.Provenance{Layer: policyref.LayerMember}
	operatorProvenance   = policyref.Provenance{Layer: policyref.LayerOperator}
	deploymentProvenance = policyref.Provenance{Layer: policyref.LayerDeployment, Name: policyref.SourceDeployment}
)

// ownProvenance is the record of a value the run's own policy set: the
// operator's for a principal no ceiling binds, else the member's.
func (c governanceCeiling) ownProvenance() policyref.Provenance {
	if c.Operator {
		return operatorProvenance
	}
	return memberProvenance
}

// provenance is the record of the layer this ceiling is: operator for a
// principal no ceiling binds, else the leaf profile, else the deployment.
func (c governanceCeiling) provenance() policyref.Provenance {
	switch {
	case c.Operator:
		return operatorProvenance
	case c.Unit != nil && !restrictionDefaultUnit(c.Unit):
		return policyref.Provenance{Layer: policyref.LayerUnit, Name: c.Unit.Name}
	default:
		return deploymentProvenance
	}
}

// lockOf is the record of the lock that fixes knob for the runs of unit u: the nearest strict
// ancestor of u, then the boundary b in hard_limits, that lists knob in locked_knobs, named for that
// locker (DESIGN-TREE §4.3). The zero record, not locked, when none does. It is derived on every read
// and never stored on a unit.
func lockOf(u *ResolvedUnit, b *types.Boundary, knob string) policyref.Provenance {
	if u != nil && len(u.Chain) > 1 {
		for _, l := range u.Chain[1:] {
			if slices.Contains(l.Locked, knob) {
				return policyref.Provenance{Layer: policyref.LayerUnit, Name: l.Name, Locked: true}
			}
		}
	}
	if b != nil && b.HardLimits() && slices.Contains(b.LockedKnobs, knob) {
		return policyref.Provenance{Layer: policyref.LayerBoundary, Name: boundaryLocker, Locked: true}
	}
	return policyref.Provenance{}
}

// lockOf is the lock fixing knob for the runs c binds; none for an operator, whom no ceiling binds.
// A ceiling refusal on knob is attributed to it (authz.Decision.OnKnob, writeErrorReasonPolicy).
func (c governanceCeiling) lockOf(knob string) policyref.Provenance {
	if c.Operator {
		return policyref.Provenance{}
	}
	return lockOf(c.Unit, c.boundary, knob)
}

// markLocked replaces the record of each knob a lock fixes for c's runs with that lock's (lockOf).
// The locker's contact is added on read (withLockerContacts), as every record's is.
func (k knobProvenance) markLocked(c governanceCeiling) {
	for name := range k {
		if rec := c.lockOf(name); rec.Locked {
			k[name] = rec
		}
	}
}

// runLockOf is lockOf for a live run's door, which reads the run's unit p now: the boundary is
// read with it, and a failed read leaves the boundary's lock out of a refusal that is answered anyway.
func (s *Server) runLockOf(ctx context.Context, p *ResolvedUnit, knob string) policyref.Provenance {
	b, err := s.governanceBoundary(ctx)
	if err != nil {
		return lockOf(p, nil, knob)
	}
	return lockOf(p, &b, knob)
}

// fillLockerContacts fills the contact of each locked record of k, a run's provenance as read now: a
// unit locker's from the run's unit chain now, the boundary's from the boundary. A locker no longer on
// the chain keeps none. Nothing more is read for a run with no locked knob.
func (s *Server) fillLockerContacts(ctx context.Context, run types.AgentRun, k knobProvenance) {
	if !slices.ContainsFunc(slices.Collect(maps.Values(k)), func(p policyref.Provenance) bool { return p.Locked }) {
		return
	}
	var chain []unitLink
	if p, err := s.runUnit(ctx, run); err == nil && p != nil {
		chain = p.Chain
	}
	var contact *policyref.Contact
	if b, err := s.governanceBoundary(ctx); err == nil {
		contact = b.Contact
	}
	k.withLockerContacts(chain, contact)
}

// withLockerContacts sets each locked record's contact: a unit locker's from chain (leaf first; the
// leaf itself never locks its own run), the boundary's from boundary.
func (k knobProvenance) withLockerContacts(chain []unitLink, boundary *policyref.Contact) {
	for name, rec := range k {
		if !rec.Locked {
			continue
		}
		rec.Contact = nil
		switch {
		case rec.Layer == policyref.LayerBoundary:
			rec.Contact = nonZeroContact(boundary)
		case rec.Layer == policyref.LayerUnit:
			if i := slices.IndexFunc(chain, func(l unitLink) bool { return l.Name == rec.Name }); i > 0 {
				rec.Contact = nonZeroContact(chain[i].Contact)
			}
		}
		k[name] = rec
	}
}

// withContacts is k with each ceiling record's contact filled from ref, the
// run's policy now (runPolicyRef): the leaf profile's contact for a profile
// record of the same name, the site's policy_help for a deployment record, a restriction's own for
// a restriction record (by name). The
// map is copied; a record ref does not name keeps no contact.
func (k knobProvenance) withContacts(ref *policyref.Ref, help *policyref.Contact, restrictions map[string]*policyref.Contact) knobProvenance {
	if k == nil {
		return nil
	}
	var leaf *policyref.Contact
	if ref != nil && ref.Source == policyref.SourceUnit {
		c := policyref.Contact{Owner: ref.Owner, Email: ref.Email, RequestURL: ref.RequestURL, RequestText: ref.RequestText}
		if !c.IsZero() {
			leaf = &c
		}
	}
	var site *policyref.Contact
	if p := policyref.Project(policyref.SourceDeployment, "", help); p != nil {
		if c := (policyref.Contact{Owner: p.Owner, Email: p.Email, RequestURL: p.RequestURL, RequestText: p.RequestText}); !c.IsZero() {
			site = &c
		}
	}
	out := make(knobProvenance, len(k))
	for name, rec := range k {
		if rec.Layer == legacyLayerProfile {
			rec.Layer = policyref.LayerUnit
		}
		switch {
		case rec.Layer == policyref.LayerUnit && leaf != nil && rec.Name == ref.Name:
			rec.Contact = leaf
		case rec.Layer == policyref.LayerDeployment:
			rec.Contact = site
		case rec.Layer == policyref.LayerRestriction:
			rec.Contact = restrictions[rec.Name]
		default:
			rec.Contact = nil
		}
		out[name] = rec
	}
	return out
}
