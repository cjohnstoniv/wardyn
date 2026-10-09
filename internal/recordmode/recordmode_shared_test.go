// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package recordmode

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A grant an organisation's component added carries a mark no policy may
// author, so a profile that copied it could be neither saved nor launched.
// It is left out, and said to be; the grant beside it is kept.
func TestSynthesize_OmitsAnOrganisationComponentsSharedGrant(t *testing.T) {
	run := types.AgentRun{ID: uuid.New()}
	shared, own := uuid.New(), uuid.New()
	grants := []types.CredentialGrant{
		{ID: shared, RunID: run.ID, Spec: types.GrantSpec{Kind: types.GrantAPIKey,
			Scope: json.RawMessage(`{"host":"org-api.example","require_tls":true,"secret_name":"org-tool-token","shared":true}`)}},
		{ID: own, RunID: run.ID, Spec: types.GrantSpec{Kind: types.GrantAPIKey, OwnerOnly: true,
			Scope: json.RawMessage(`{"host":"person-api.example","secret_name":"person-secret"}`)}},
	}
	got, warns := Synthesize(Observations{MintedGrantIDs: []uuid.UUID{shared, own}}, grants, run)
	if len(got.EligibleGrants) != 1 || string(got.EligibleGrants[0].Scope) != string(grants[1].Spec.Scope) {
		t.Fatalf("eligible grants = %+v, want the person's own grant alone", got.EligibleGrants)
	}
	if !containsSubstr(warns, shared.String()) || !containsSubstr(warns, "organisation's component") {
		t.Errorf("the shared grant was left out silently: %v", warns)
	}
	if containsSubstr(warns, "org-tool-token") {
		t.Errorf("a warning names the organisation's secret: %v", warns)
	}
}
