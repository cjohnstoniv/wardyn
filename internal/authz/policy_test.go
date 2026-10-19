// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"reflect"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/policyref"
)

// A policy rides the wire body; the audit datum takes only the layer kind that
// refused and the leaf profile's name (#2951), so a contact's owner or email can
// never reach an authz.denied row through a Decision.
func TestWithPolicyStampsTheLayerAndKeepsTheContactOutOfTheDatum(t *testing.T) {
	ref := &policyref.Ref{Source: policyref.SourceProfile, Name: "walled", Owner: "Platform team", Email: "platform@example.com"}
	d := Deny(ReasonGovernanceProfile, "runs.task_mode", "no")
	withPolicy := d.WithPolicy(ref)
	if withPolicy.Policy == nil || withPolicy.Policy.Layer != policyref.LayerProfile || d.Policy != nil || ref.Layer != "" {
		t.Fatalf("WithPolicy must return a copy stamped with the layer: got %+v, original %+v, ref %+v", withPolicy.Policy, d.Policy, ref)
	}
	if want := (policyref.Ref{Source: ref.Source, Name: ref.Name, Owner: ref.Owner, Email: ref.Email, Layer: policyref.LayerProfile}); *withPolicy.Policy != want {
		t.Errorf("policy = %+v, want %+v", *withPolicy.Policy, want)
	}
	p := Principal{Subject: "sub-1"}
	base, got := Datum(d, p, "POST"), Datum(withPolicy, p, "POST")
	base["layer"], base["layer_name"] = "profile", "walled"
	if !reflect.DeepEqual(base, got) {
		t.Errorf("datum = %v, want the bare datum plus layer and layer_name: %v", got, base)
	}
	for _, v := range got {
		if v == ref.Owner || v == ref.Email {
			t.Errorf("datum carries the contact: %v", got)
		}
	}
	dep := Datum(d.WithPolicy(&policyref.Ref{Source: policyref.SourceDeployment}), p, "POST")
	if dep["layer"] != "deployment" || dep["layer_name"] != nil {
		t.Errorf("deployment datum = %v, want layer deployment and no name", dep)
	}
	if got := d.WithPolicy(&policyref.Ref{Source: "walls"}); got.Policy.Layer != "" || Datum(got, p, "")["layer"] != nil {
		t.Errorf("a source the wire does not know got a layer: %+v", got.Policy)
	}
	if got := d.WithPolicy(nil); got.Policy != nil {
		t.Errorf("WithPolicy(nil) = %+v, want none", got.Policy)
	}
	forged := d.With("layer", "member").With("layer_name", "x")
	if m := Datum(forged, p, ""); m["layer"] != nil || m["layer_name"] != nil {
		t.Errorf("a detail forged the layer: %v", m)
	}
}
