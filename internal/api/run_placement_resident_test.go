// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// residentFactsStore adds the stored delivery policy and the bound runner's
// reported posture to the local-plan test store.
type residentFactsStore struct {
	localPlanTestStore
	policy  json.RawMessage
	posture types.RunnerPosture
	reads   *int // delivery-policy reads, when set
}

func (s residentFactsStore) GetCredentialDeliveryPolicy(context.Context) (types.CredentialDeliveryPolicyDoc, error) {
	if s.reads != nil {
		*s.reads++
	}
	return types.CredentialDeliveryPolicyDoc{Policy: s.policy}, nil
}

func (s residentFactsStore) GetRunner(_ context.Context, id uuid.UUID) (types.Runner, error) {
	reported := time.Now()
	return types.Runner{ID: id, State: types.RunnerClaimed, Posture: s.posture, PostureReportedAt: &reported}, nil
}

// N1/Q7: the dispatch and revive doors judge a runner_resident credential on the
// same facts as create: the stored policy, the bound runner's posture and the
// run's blast-radius floor. An api_key to a host outside the baseline puts the
// run at a CC3 floor, which a runner-asserted class never meets for a resident
// credential; a runner whose posture misses the requirement is refused too.
func TestLocalResident_DispatchAndReviveDoorsJudgeTheFloorAndPosture(t *testing.T) {
	f := newComponentFixture(t)
	runnerID := uuid.New()
	run := types.AgentRun{ID: uuid.New(), CreatedBy: capSub, Placement: types.PlacementLocal, RunnerID: &runnerID}
	env := types.CredentialGrant{ID: uuid.New(), RunID: run.ID, Spec: types.GrantSpec{Kind: types.GrantEnvSecret, Scope: json.RawMessage(`{"name":"TOKEN","secret_name":"ci-token"}`)}}
	apiKey := types.CredentialGrant{ID: uuid.New(), RunID: run.ID, Spec: types.GrantSpec{Kind: types.GrantAPIKey, Scope: json.RawMessage(`{"host":"api.vendor.example","secret_name":"vendor-key"}`)}}
	st := residentFactsStore{localPlanTestStore: localPlanTestStore{f.st},
		policy:  json.RawMessage(`{"classes":{"env_secret":{"mode":"runner_resident","require_posture":{"disk_encrypted":true}},"api_key":{"mode":"runner_resident"}}}`),
		posture: types.RunnerPosture{DiskEncrypted: true, OS: "linux", Evidence: types.PostureEvidenceOSProbe}}
	f.srv.cfg.Store = st

	// dispatch carries the env value as its placeholder and the given grants as
	// eligibility records; revive carries only the stored proxy config's policy.
	dispatch := func(grants ...types.CredentialGrant) *placement.Refusal {
		t.Helper()
		f.st.grants = append([]types.CredentialGrant{env}, grants...)
		spec := runner.SandboxSpec{RunID: run.ID, SecretEnv: map[string]string{"TOKEN": placement.ResidentPlaceholder(env.ID)}}
		for _, g := range grants {
			spec.ProxyConfig.Policy.EligibleGrants = append(spec.ProxyConfig.Policy.EligibleGrants, g.Spec)
		}
		p, err := f.srv.localResolvedPlan(context.Background(), run, false, types.SiteConfig{}, spec, nil, llmTransport{})
		if err != nil {
			t.Fatal(err)
		}
		_, r := placement.LocalEligibility(p)
		return r
	}
	revive := func(grants ...types.CredentialGrant) *placement.Refusal {
		t.Helper()
		f.st.grants = grants
		cfg := proxy.Config{RunID: run.ID}
		for _, g := range grants {
			cfg.Policy.EligibleGrants = append(cfg.Policy.EligibleGrants, g.Spec)
		}
		r, _ := f.srv.refreshLocalDeploymentConfig(context.Background(), run, types.SiteConfig{}, &cfg).(*placement.Refusal)
		return r
	}
	want := func(door string, r *placement.Refusal, reason placement.Reason) {
		t.Helper()
		if r == nil && reason == "" {
			return
		}
		if r == nil || r.Reason != reason {
			t.Errorf("%s: %+v, want %q", door, r, reason)
		}
	}

	want("dispatch, no floor, posture met", dispatch(), "")
	want("revive, no floor", revive(), "")
	want("dispatch, CC3 floor", dispatch(apiKey), placement.ReasonRunnerPostureUnmet)
	want("revive, CC3 floor", revive(apiKey), placement.ReasonRunnerPostureUnmet)
	st.posture.DiskEncrypted = false
	f.srv.cfg.Store = st
	want("dispatch, posture unmet", dispatch(), placement.ReasonRunnerPostureUnmet)
	want("revive, posture unmet", revive(env), placement.ReasonRunnerPostureUnmet)
}

// Every LocalEligibility door reads the delivery policy once, through
// localPlanFacts: the plan builders behind create and dispatch read none.
func TestLocalPlanFacts_OneDeliveryPolicyReadPerDoor(t *testing.T) {
	f := newComponentFixture(t)
	reads := 0
	f.srv.cfg.Store = residentFactsStore{localPlanTestStore: localPlanTestStore{f.st}, policy: json.RawMessage(`{"classes":{}}`), reads: &reads}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(utCtx("user", "", nil))
	_, _ = f.srv.localPlacementRefusal(r, createRunRequest{Placement: placement.Local}, false, types.RunPolicySpec{}, governanceCeiling{}, runComponents{}, nil, nil, composer.Baseline{})
	if reads != 1 {
		t.Errorf("create read the delivery policy %d times, want 1", reads)
	}
	reads = 0
	run := types.AgentRun{ID: uuid.New(), CreatedBy: capSub, Placement: types.PlacementLocal}
	if _, err := f.srv.localResolvedPlan(context.Background(), run, false, types.SiteConfig{}, runner.SandboxSpec{RunID: run.ID}, nil, llmTransport{}); err != nil {
		t.Fatal(err)
	}
	if reads != 1 {
		t.Errorf("dispatch read the delivery policy %d times, want 1", reads)
	}
}

// A stored delivery policy that fails validation (written out of band or by another version) is
// never judged on: the read errors, so every LocalEligibility door fails closed.
func TestStoredDeliveryPolicy_InvalidDocumentFailsClosed(t *testing.T) {
	f := newComponentFixture(t)
	reads := 0
	f.srv.cfg.Store = residentFactsStore{localPlanTestStore: localPlanTestStore{f.st}, policy: json.RawMessage(`{"classes":{"cloud_sts":{"mode":"via_org"}}}`), reads: &reads}
	if _, err := f.srv.storedDeliveryPolicy(context.Background()); err == nil {
		t.Fatal("an invalid stored delivery policy was accepted")
	}
}
