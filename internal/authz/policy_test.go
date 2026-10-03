// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package authz

import (
	"reflect"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/policyref"
)

// A policy rides the wire body only. The audit datum is built from the reason,
// the detail and the principal, so a contact's owner or email can never reach an
// authz.denied row through a Decision.
func TestWithPolicyNeverReachesTheDatum(t *testing.T) {
	ref := &policyref.Ref{Source: policyref.SourceProfile, Name: "walled", Owner: "Platform team", Email: "platform@example.com"}
	d := Deny(ReasonGovernanceProfile, "runs.task_mode", "no")
	withPolicy := d.WithPolicy(ref)
	if withPolicy.Policy != ref || d.Policy != nil {
		t.Fatalf("WithPolicy must return a copy carrying the ref: got %+v, original %+v", withPolicy.Policy, d.Policy)
	}
	p := Principal{Subject: "sub-1"}
	if a, b := Datum(d, p, "POST"), Datum(withPolicy, p, "POST"); !reflect.DeepEqual(a, b) {
		t.Errorf("datum changed with a policy: %v vs %v", a, b)
	}
	if got := d.WithPolicy(nil); got.Policy != nil {
		t.Errorf("WithPolicy(nil) = %+v, want none", got.Policy)
	}
}
