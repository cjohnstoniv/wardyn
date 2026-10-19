// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"reflect"
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
// Both sides are compared cloned, so an empty list and an absent one are one
// value, as an unset first_use_approval and its normalized mode are.
func (k knobProvenance) setBy(rec policyref.Provenance, before, after types.RunPolicySpec) {
	if k == nil {
		return
	}
	before, after = before.Clone(), after.Clone()
	before.FirstUseApproval, after.FirstUseApproval = before.FirstUseApproval.Normalize(), after.FirstUseApproval.Normalize()
	bv, av := reflect.ValueOf(before), reflect.ValueOf(after)
	for i, name := range policyKnobFields {
		if name != "" && !reflect.DeepEqual(bv.Field(i).Interface(), av.Field(i).Interface()) {
			k[name] = rec
		}
	}
}

// The records a resolution writes. A ceiling's is locked: the person cannot
// widen what it set (until per-knob delegation, that is every value a ceiling
// set). The name is the leaf profile's, never a profile it is built from.
var (
	memberProvenance     = policyref.Provenance{Layer: policyref.LayerMember}
	operatorProvenance   = policyref.Provenance{Layer: policyref.LayerOperator}
	deploymentProvenance = policyref.Provenance{Layer: policyref.LayerDeployment, Name: policyref.SourceDeployment, Locked: true}
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
		return policyref.Provenance{Layer: policyref.LayerProfile, Name: c.Profile.Name, Locked: true}
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
