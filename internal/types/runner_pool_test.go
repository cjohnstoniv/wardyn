// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestRunnerPoolHostingMapsToOnePlacement(t *testing.T) {
	if RunnerPoolRemoteProvided.Placement() != PlacementRemote || RunnerPoolSelfHosted.Placement() != PlacementLocal {
		t.Fatal("each hosting type has its placement")
	}
	if RunnerPoolHosting("cloud").Valid() || RunnerPoolHosting("cloud").Placement() != "" {
		t.Fatal("an unknown hosting type is neither valid nor placed")
	}
}

func TestRunnerPoolMemberKindsAreNeverMixed(t *testing.T) {
	id := uuid.New()
	nilID := uuid.Nil
	for _, tc := range []struct {
		name    string
		m       RunnerPoolMember
		hosting RunnerPoolHosting
		ok      bool
	}{
		{"runner in a self-hosted pool", RunnerPoolMember{RunnerID: &id}, RunnerPoolSelfHosted, true},
		{"executor in a remote-provided pool", RunnerPoolMember{ExecutorID: "build-1"}, RunnerPoolRemoteProvided, true},
		{"runner in a remote-provided pool", RunnerPoolMember{RunnerID: &id}, RunnerPoolRemoteProvided, false},
		{"executor in a self-hosted pool", RunnerPoolMember{ExecutorID: "build-1"}, RunnerPoolSelfHosted, false},
		{"both targets", RunnerPoolMember{RunnerID: &id, ExecutorID: "build-1"}, RunnerPoolSelfHosted, false},
		{"no target", RunnerPoolMember{}, RunnerPoolSelfHosted, false},
		{"nil runner id", RunnerPoolMember{RunnerID: &nilID}, RunnerPoolSelfHosted, false},
		{"an address is not an executor id", RunnerPoolMember{ExecutorID: "tcp://10.0.0.1:2376"}, RunnerPoolRemoteProvided, false},
		{"a claimed runner's substrate name is not an executor id", RunnerPoolMember{ExecutorID: "runner:" + id.String()}, RunnerPoolRemoteProvided, false},
		{"a socket path is not an executor id", RunnerPoolMember{ExecutorID: "/var/run/docker.sock"}, RunnerPoolRemoteProvided, false},
	} {
		if err := tc.m.Validate(tc.hosting); (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}

func TestRunnerPoolDefaultsValidateAndWireShape(t *testing.T) {
	id := uuid.New()
	if err := (RunnerPoolDefaults{PreferredHosting: RunnerPoolSelfHosted, RemoteProvided: &id}).Validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []RunnerPoolDefaults{{PreferredHosting: "cloud"}, {SelfHosted: &uuid.UUID{}}} {
		if bad.Validate() == nil {
			t.Errorf("%+v should be refused", bad)
		}
	}
	raw, _ := json.Marshal(RunnerPoolDefaults{})
	if string(raw) != "{}" {
		t.Errorf("an empty document is %s: an absent field must inherit, so it is omitted", raw)
	}
	if (RunnerPoolDefaults{RemoteProvided: &id}).PoolFor(RunnerPoolSelfHosted) != nil {
		t.Error("PoolFor answers only for its own hosting type")
	}
}

func TestRunnerPoolUseSubjectsOnlyNarrow(t *testing.T) {
	user := RunnerPoolSubject{SubjectType: CapabilitySubjectUser, Subject: "alice"}
	for _, tc := range []struct {
		name string
		in   []RunnerPoolSubject
		ok   bool
	}{
		{"a person, a group and a role", []RunnerPoolSubject{user, {CapabilitySubjectGroup, "eng"}, {CapabilitySubjectUserType, "contractor"}}, true},
		{"empty reads as unrestricted, so it is refused", nil, false},
		{"all is no narrowing", []RunnerPoolSubject{{CapabilitySubjectAll, ""}}, false},
		{"a repeated subject", []RunnerPoolSubject{user, user}, false},
		{"an unpadded name", []RunnerPoolSubject{{CapabilitySubjectUser, " alice"}}, false},
		{"a blank name", []RunnerPoolSubject{{CapabilitySubjectGroup, ""}}, false},
		{"too many", make([]RunnerPoolSubject, RunnerPoolUseSubjectsMax+1), false},
	} {
		if err := ValidateRunnerPoolUseSubjects(tc.in); (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}

func TestRunnerPoolUsePolicyNarrowsRemoteProvidedPoolsOnly(t *testing.T) {
	policy := RunnerPoolUsePolicy{Subjects: []RunnerPoolSubject{{CapabilitySubjectGroup, "eng"}}}
	if err := policy.Validate(RunnerPoolRemoteProvided); err != nil {
		t.Fatal(err)
	}
	for _, h := range []RunnerPoolHosting{RunnerPoolSelfHosted, "", "cloud"} {
		if err := policy.Validate(h); err == nil {
			t.Errorf("a use policy on a %q pool was accepted", h)
		}
	}
	if err := (RunnerPoolUsePolicy{}).Validate(RunnerPoolRemoteProvided); err == nil {
		t.Error("an empty policy on a remote-provided pool was accepted")
	}
}

func TestValidateRunnerPoolName(t *testing.T) {
	for name, ok := range map[string]bool{"Build farm": true, "": false, " x": false, "x ": false, "a\nb": false, strings.Repeat("x", RunnerPoolNameMax): true, strings.Repeat("x", RunnerPoolNameMax+1): false} {
		if err := ValidateRunnerPoolName(name); (err == nil) != ok {
			t.Errorf("%q: err = %v, want ok=%v", name, err, ok)
		}
	}
}
