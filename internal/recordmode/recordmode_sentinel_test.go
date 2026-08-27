// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package recordmode

import (
	"encoding/json"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

// A recorded profile is durable, shareable and addressable by id. Copying a
// shared-subscription sentinel into one would turn a single recording into a
// permanent grant on ONE operator's live Anthropic OAuth token, usable by anyone
// who can launch a run against that profile long after the recording. The
// injection sink refuses off-posture anyway; this stops the grant being written
// down at all, which is the difference between a refusal and an artifact nobody
// notices for a year.
func TestSynthesize_DropsSharedSubscriptionSentinelGrants(t *testing.T) {
	for _, sentinel := range []string{types.SubscriptionOAuthSecret, types.ManagedOAuthSecret} {
		run := types.AgentRun{ID: uuid.New()}
		id := uuid.New()
		spec := types.GrantSpec{
			Kind: types.GrantAPIKey,
			Scope: json.RawMessage(`{"host":"api.anthropic.com","header":"Authorization",` +
				`"format":"Bearer %s","secret_name":"` + sentinel + `"}`),
		}
		grants := []types.CredentialGrant{{ID: id, RunID: run.ID, Spec: spec}}
		obs := Observations{MintedGrantIDs: []uuid.UUID{id}}

		got, warns := Synthesize(obs, grants, run)
		if len(got.EligibleGrants) != 0 {
			t.Fatalf("%s: sentinel grant was copied into the profile: %+v", sentinel, got.EligibleGrants)
		}
		if !containsSubstr(warns, "shared subscription OAuth sentinel") {
			t.Fatalf("%s: dropped silently; a synthesize that quietly removes a grant is worse than one that says so. warns=%v", sentinel, warns)
		}
	}
}

// Unparseable scope reads as "sentinel" — a grant we cannot inspect is not one to
// write into a durable least-privilege profile.
func TestSynthesize_DropsUninspectableAPIKeyGrant(t *testing.T) {
	run := types.AgentRun{ID: uuid.New()}
	id := uuid.New()
	grants := []types.CredentialGrant{{ID: id, RunID: run.ID, Spec: types.GrantSpec{
		Kind: types.GrantAPIKey, Scope: json.RawMessage(`not json`),
	}}}
	got, _ := Synthesize(Observations{MintedGrantIDs: []uuid.UUID{id}}, grants, run)
	if len(got.EligibleGrants) != 0 {
		t.Fatalf("api_key grant with unreadable scope must not be recorded: %+v", got.EligibleGrants)
	}
}

// An ordinary api_key grant naming a real stored secret is unaffected — the rule
// targets the two sentinels, not the grant kind.
func TestSynthesize_KeepsOrdinaryAPIKeyGrant(t *testing.T) {
	run := types.AgentRun{ID: uuid.New()}
	id := uuid.New()
	grants := []types.CredentialGrant{{ID: id, RunID: run.ID, Spec: types.GrantSpec{
		Kind:  types.GrantAPIKey,
		Scope: json.RawMessage(`{"host":"api.example","secret_name":"anthropic-api-key"}`),
	}}}
	got, _ := Synthesize(Observations{MintedGrantIDs: []uuid.UUID{id}}, grants, run)
	if len(got.EligibleGrants) != 1 {
		t.Fatalf("ordinary api_key grant must survive, got %+v", got.EligibleGrants)
	}
}
