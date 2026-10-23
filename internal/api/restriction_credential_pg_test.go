// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestRestrictionCredentialClass: a restriction's withheld_grants keeps a credential of that kind from
// a run that captured it, at dispatch (the seal layers) and at a live mint, and nothing else; a
// restriction that stops withholding the kind is not exempt from a second human.
func TestRestrictionCredentialClass(t *testing.T) {
	e := newGovEnvOn(t, throwawayPGPool(t))
	ctx := context.Background()
	ssh := types.GrantSpec{Kind: types.GrantSSHKey, Scope: json.RawMessage(`{"host":"git.corp.example","key_secret_ref":"deploy-key"}`)}
	pat := types.GrantSpec{Kind: types.GrantGitPAT, Scope: json.RawMessage(`{"host":"git.corp.example","secret_name":"corp-pat"}`)}
	r := seedRestriction(t, e, "rp-cred", types.RestrictionSpec{WithheldGrants: []types.GrantSpec{{Kind: types.GrantSSHKey}}},
		types.RestrictionLimits{}, userMember("sub-rp-cred"))
	run := types.AgentRun{ID: uuid.New(), CreatedBy: "sub-rp-cred", RestrictionIDs: []uuid.UUID{r.ID}}

	live, err := e.srv.liveWithholding(ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	dispatch := withholding{layers: e.srv.outerLayers(ctx, run)}
	for name, w := range map[string]withholding{"mint": live, "dispatch": dispatch} {
		if !w.credentialWithheld(ssh) {
			t.Errorf("%s: an ssh_key grant was not withheld", name)
		}
		if w.credentialWithheld(pat) {
			t.Errorf("%s: a git_pat grant was withheld by an ssh_key entry", name)
		}
	}
	if w, err := e.srv.liveWithholding(ctx, types.AgentRun{ID: uuid.New()}); err != nil || w.credentialWithheld(ssh) {
		t.Errorf("a run with no restriction: ssh_key withheld %v (%v)", w.credentialWithheld(ssh), err)
	}

	// A scoped entry names one pairing only.
	other := types.GrantSpec{Kind: types.GrantSSHKey, Scope: json.RawMessage(`{"host":"git.other.example","key_secret_ref":"other-key"}`)}
	if !grantWithheldBy(ssh, []types.GrantSpec{ssh}) || grantWithheldBy(other, []types.GrantSpec{ssh}) {
		t.Errorf("a scoped entry: own pairing %v other pairing %v, want true false",
			grantWithheldBy(ssh, []types.GrantSpec{ssh}), grantWithheldBy(other, []types.GrantSpec{ssh}))
	}

	// Dropping the withheld kind widens, so the write needs a second human; keeping it does not.
	keep := types.RestrictionWrite{Name: r.Name, Spec: r.Spec, Contact: r.Contact, Members: r.Members}
	drop := keep
	drop.Spec = types.RestrictionSpec{}
	if !restrictionAtLeastAsStrict(r, keep) || restrictionAtLeastAsStrict(r, drop) {
		t.Errorf("strictness: keep %v drop %v, want true false", restrictionAtLeastAsStrict(r, keep), restrictionAtLeastAsStrict(r, drop))
	}
	bad := types.RestrictionWrite{Name: "x", Spec: types.RestrictionSpec{WithheldGrants: []types.GrantSpec{{Kind: "nope"}}}}
	if err := validateRestriction(&bad); err == nil {
		t.Error("an unknown withheld grant kind was accepted")
	}
}
