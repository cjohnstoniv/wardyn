// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestRunnerPoolScopeMapsToOnePlacement(t *testing.T) {
	if RunnerPoolOrganization.Placement() != PlacementRemote || RunnerPoolUser.Placement() != PlacementLocal {
		t.Fatal("each scope places on its own kind of runner")
	}
	if RunnerPoolScope("cloud").Valid() || RunnerPoolScope("cloud").Placement() != "" {
		t.Fatal("an unknown scope is neither valid nor placed")
	}
}

func TestRunnerPoolMemberOrganizationPoolsHoldRemoteHostedOnly(t *testing.T) {
	id := uuid.New()
	nilID := uuid.Nil
	for _, tc := range []struct {
		name  string
		m     RunnerPoolMember
		scope RunnerPoolScope
		ok    bool
	}{
		{"self-hosted runner in a user pool", RunnerPoolMember{RunnerID: &id}, RunnerPoolUser, true},
		{"remote-hosted runner in an organization pool", RunnerPoolMember{RemoteRunnerID: "build-1"}, RunnerPoolOrganization, true},
		{"remote-hosted runner in a user pool", RunnerPoolMember{RemoteRunnerID: "build-1"}, RunnerPoolUser, true},
		{"self-hosted runner in an organization pool", RunnerPoolMember{RunnerID: &id}, RunnerPoolOrganization, false},
		{"both runners", RunnerPoolMember{RunnerID: &id, RemoteRunnerID: "build-1"}, RunnerPoolUser, false},
		{"no runner", RunnerPoolMember{}, RunnerPoolUser, false},
		{"nil runner id", RunnerPoolMember{RunnerID: &nilID}, RunnerPoolUser, false},
		{"an address is not a remote runner id", RunnerPoolMember{RemoteRunnerID: "tcp://10.0.0.1:2376"}, RunnerPoolOrganization, false},
		{"a claimed runner's substrate name is not a remote runner id", RunnerPoolMember{RemoteRunnerID: "runner:" + id.String()}, RunnerPoolOrganization, false},
		{"a UUID is not a remote runner id", RunnerPoolMember{RemoteRunnerID: id.String()}, RunnerPoolOrganization, false},
		{"a socket path is not a remote runner id", RunnerPoolMember{RemoteRunnerID: "/var/run/docker.sock"}, RunnerPoolOrganization, false},
	} {
		if err := tc.m.Validate(tc.scope); (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
	if err := (RunnerPoolMember{RunnerID: &id}).Validate(RunnerPoolOrganization); err == nil || err.Error() != OrgPoolSelfHostedMsg {
		t.Errorf("a self-hosted runner in an organization pool: %v, want the ruling's sentence", err)
	}
}

func TestRunnerPoolDefaultsValidateAndWireShape(t *testing.T) {
	id := uuid.New()
	if err := (RunnerPoolDefaults{PoolID: &id}).Validate(); err != nil {
		t.Fatal(err)
	}
	for _, ok := range []ConfinementClass{"", CC1, CC2, CC3} {
		if err := (RunnerPoolDefaults{Barrier: ok}).Validate(); err != nil {
			t.Errorf("barrier %q refused: %v", ok, err)
		}
	}
	for _, bad := range []RunnerPoolDefaults{{PoolID: &uuid.UUID{}}, {Barrier: "CC9"}, {Barrier: "cc2"}} {
		if bad.Validate() == nil {
			t.Errorf("%+v should be refused", bad)
		}
	}
	raw, _ := json.Marshal(RunnerPoolDefaults{})
	if string(raw) != "{}" {
		t.Errorf("an empty document is %s: an absent field must inherit, so it is omitted", raw)
	}
	raw, _ = json.Marshal(RunnerPoolDefaults{PoolID: &id})
	if string(raw) != `{"pool_id":"`+id.String()+`"}` {
		t.Errorf("a default pool is %s", raw)
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

func TestRunnerPoolUsePolicyNarrowsOrganizationPoolsOnly(t *testing.T) {
	policy := RunnerPoolUsePolicy{Subjects: []RunnerPoolSubject{{CapabilitySubjectGroup, "eng"}}}
	if err := policy.Validate(RunnerPoolOrganization); err != nil {
		t.Fatal(err)
	}
	for _, h := range []RunnerPoolScope{RunnerPoolUser, "", "cloud"} {
		if err := policy.Validate(h); err == nil {
			t.Errorf("a use policy on a %q pool was accepted", h)
		}
	}
	if err := (RunnerPoolUsePolicy{}).Validate(RunnerPoolOrganization); err == nil {
		t.Error("an empty policy on an organization pool was accepted")
	}
}

func TestValidateRunnerPoolName(t *testing.T) {
	for name, ok := range map[string]bool{"Build farm": true, "": false, " x": false, "x ": false, "a\nb": false, strings.Repeat("x", RunnerPoolNameMax): true, strings.Repeat("x", RunnerPoolNameMax+1): false} {
		if err := ValidateRunnerPoolName(name); (err == nil) != ok {
			t.Errorf("%q: err = %v, want ok=%v", name, err, ok)
		}
	}
}
