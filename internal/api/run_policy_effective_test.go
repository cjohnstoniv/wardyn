// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// artifactSiteCfgStore is dispatchTestStore with ONE operator artifact override,
// so the dispatch under test really widens its egress mid-flight
// (substituteArtifactEgress) instead of handing the proxy the policy it was given.
type artifactSiteCfgStore struct{ *dispatchTestStore }

func (artifactSiteCfgStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return types.SiteConfig{ArtifactOverrides: map[string]types.ArtifactOverride{
		"npm": {BaseURL: "https://artifactory.corp/npm"},
	}}, nil
}

// TestDispatch_AuditsEffectivePolicyEnvelope pins the authorization envelope: the
// run row cannot answer "what was this agent allowed to do?" (agent_runs.policy_id
// has no FK and no spec column, run_policies.spec is overwritten in place, and an
// inline/default policy has no row at all), so the append-only
// run.policy.effective event is the only durable record — and it is worth nothing
// if it snapshots the PRE-widening spec. Counterfactual: emit the event at the end
// of handleCreateRun (or anywhere above dispatch's widening phases) and the corp
// mirror is missing while the dropped public registry is still listed.
func TestDispatch_AuditsEffectivePolicyEnvelope(t *testing.T) {
	fr := &fakeRunner{}
	srv, st, audit, run := dispatchTeardownFixture(t, fr, types.RunPending)
	srv.cfg.Store = artifactSiteCfgStore{st}
	run.Task = "" // no agent exec / completion watcher: this test is about the envelope

	srv.dispatchRun(context.Background(), run, dispatchParams{
		RunToken: "run-token", Image: "wardyn/claude-code:latest",
		Policy: types.RunPolicySpec{
			AllowedDomains:      []string{"api.anthropic.com", "registry.npmjs.org"},
			MinConfinementClass: types.CC1,
		},
	})

	ev := findAudit(audit.events, run.ID, "run.policy.effective", "success")
	if ev == nil {
		t.Fatalf("dispatch recorded no run.policy.effective envelope; events=%s", auditDump(audit.events, run.ID))
	}
	var got types.RunPolicySpec
	if err := json.Unmarshal(ev.Data, &got); err != nil {
		t.Fatalf("envelope is not a RunPolicySpec: %v (%s)", err, ev.Data)
	}
	enforced := fr.lastSpec.ProxyConfig.Policy.AllowedDomains
	if !reflect.DeepEqual(got.AllowedDomains, enforced) {
		t.Errorf("audited envelope != the policy handed to the proxy:\n audited  = %v\n enforced = %v", got.AllowedDomains, enforced)
	}
	if slices.Contains(got.AllowedDomains, "registry.npmjs.org") || !slices.Contains(got.AllowedDomains, "artifactory.corp") {
		t.Errorf("envelope snapshots the PRE-widening policy; want the corp mirror substituted in: %v", got.AllowedDomains)
	}
}
