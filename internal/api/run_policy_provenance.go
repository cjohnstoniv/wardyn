// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
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

// The records a resolution writes. None is locked: a lock is a layer forbidding
// the layers below it to change a knob (lower_may_change false), and no layer
// can lock one until org walls or per-knob delegation arrive. The name is the
// leaf profile's, never a profile it is built from.
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
	case c.Profile != nil:
		return policyref.Provenance{Layer: policyref.LayerProfile, Name: c.Profile.Name}
	default:
		return deploymentProvenance
	}
}

// withContacts is k with each ceiling record's contact filled from ref, the
// run's policy now (runPolicyRef): the leaf profile's contact for a profile
// record of the same name, the site's policy_help for a deployment record. The
// map is copied; a record ref does not name keeps no contact.
func (k knobProvenance) withContacts(ref *policyref.Ref, help *policyref.Contact) knobProvenance {
	if k == nil {
		return nil
	}
	var leaf *policyref.Contact
	if ref != nil && ref.Source == policyref.SourceProfile {
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
		switch {
		case rec.Layer == policyref.LayerProfile && leaf != nil && rec.Name == ref.Name:
			rec.Contact = leaf
		case rec.Layer == policyref.LayerDeployment:
			rec.Contact = site
		default:
			rec.Contact = nil
		}
		out[name] = rec
	}
	return out
}
