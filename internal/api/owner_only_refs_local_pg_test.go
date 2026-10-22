// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_OwnerOnlyRefsNeverDeliveredFromTheOrgNamespace: an ssh_key grant
// binding the member's own key (owner_only_refs) beside an org known_hosts, on
// a local run whose delivery policy makes ssh_key runner_resident and with an
// operator key of the same name. Create refuses it, though the same grant
// without the binding is admitted resident; and the resident reader never
// serves a bound ref, the key or the known_hosts, from the operator's row.
func TestPG_OwnerOnlyRefsNeverDeliveredFromTheOrgNamespace(t *testing.T) {
	f := newResidentFix(t, nil, residentAllClassesPolicy)
	f.srv.cfg.DisableGitPATBroker = true
	ms := &memSecrets{m: map[string][]byte{}, owned: map[string]map[string][]byte{}}
	f.srv.cfg.Secrets = ms
	ctx := t.Context()
	owner := f.rn.Owner
	if err := ms.For(owner).Put(ctx, "dev-ssh", []byte("alice-key")); err != nil {
		t.Fatal(err)
	}
	for name, v := range map[string]string{"dev-ssh": "operator-dev-ssh", "corp-kh": "operator-corp-kh"} {
		if err := ms.Put(ctx, name, []byte(v)); err != nil {
			t.Fatal(err)
		}
	}
	grant := func(refs ...string) types.GrantSpec {
		return types.GrantSpec{Kind: types.GrantSSHKey, OwnerOnlyRefs: refs,
			Scope: json.RawMessage(`{"host":"github.com","key_secret_ref":"dev-ssh","known_hosts_secret_ref":"corp-kh"}`)}
	}
	judged := personalJudgement(storedPolicy(t, f.srv))
	persist := func(g types.GrantSpec) ([]localGrantDelivery, *runRefusal) {
		_, d, ref := f.srv.ownLocalGrantSpecs(localMemberRequest(owner), types.RunPolicySpec{EligibleGrants: []types.GrantSpec{g}}, judged)
		return d, ref
	}
	if d, ref := persist(grant()); ref != nil || len(d) != 1 || d[0].mode != placement.ModeRunnerResident {
		t.Fatalf("unbound org grant: %+v %v, want it admitted runner_resident", d, ref)
	}
	if d, ref := persist(grant("dev-ssh")); ref == nil || ref.body.Reason != string(placement.ReasonPlacementCredential) {
		t.Fatalf("own key bound: %+v %v, want the placement_credential refusal", d, ref)
	}

	for _, bound := range []string{"dev-ssh", "corp-kh"} {
		run := f.startingRun(t, nil)
		if _, err := f.st.CreateGrant(ctx, types.CredentialGrant{ID: uuid.New(), RunID: run.ID, Delivery: types.GrantDeliveryResident, Spec: grant(bound)}); err != nil {
			t.Fatal(err)
		}
		spec, values, _ := f.authorClass(t, run, residentClassCases[2], false)
		for _, v := range values {
			if bound == "dev-ssh" && string(v.value) == "operator-dev-ssh" {
				t.Errorf("%s bound: the operator's key was served on the resident path", bound)
			}
		}
		for _, m := range spec.ProxyConfig.ResidentMints {
			if bound == "corp-kh" && m.KnownHosts != "" {
				t.Errorf("known_hosts bound: the operator's known_hosts was served on the resident path")
			}
		}
	}
}
