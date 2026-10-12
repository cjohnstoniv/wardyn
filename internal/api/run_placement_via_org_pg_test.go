// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The via_org half of local eligibility (H12) against a real Postgres: an org-held grant of a
// via_org-shaped class reaches the final unsupported-local refusal only once the stored delivery
// policy delivers its class via_org, and persists recorded via_org; a class outside the shape or the
// class's bounds never does, even from a policy row written around the validated writer.
//
// Guarded by WARDYN_TEST_PG (throwawayPGPool): skipped cleanly when unset, must PASS when set.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func (f *localOwnerFixture) setDeliveryPolicy(t *testing.T, raw string) {
	t.Helper()
	st := f.h.srv.cfg.Store.(store.RunnerStore)
	if _, err := st.PutCredentialDeliveryPolicy(context.Background(), json.RawMessage(raw), "seed", time.Now()); err != nil {
		t.Fatal(err)
	}
}

func (f *localOwnerFixture) localRunWith(t *testing.T, grant string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"agent":"claude-code","task":"t","placement":"local","inline_policy":{"min_confinement_class":"CC2","allowed_domains":["api.anthropic.com","corp.example"],` +
		`"eligible_grants":[` + grant + `]}}`
	return doSSO(t, f.h.srv, http.MethodPost, "/api/v1/runs", f.bob, body)
}

func TestLocalEligibility_ViaOrgOnlyWhenConfiguredAndShaped(t *testing.T) {
	f := newLocalOwnerFixture(t) // bob owns nothing: every grant below is the organisation's
	apiKey := `{"kind":"api_key","scope":{"host":"corp.example","secret_name":"` + localCorpSecret + `"}}`
	f.setDeliveryPolicy(t, `{"classes":{}}`)
	if w := f.localRunWith(t, apiKey); w.Code != 422 || errorReason(w) != string(placement.ReasonPlacementCredential) {
		t.Errorf("api_key with no policy = %d %s, want placement_credential", w.Code, w.Body)
	}
	f.setDeliveryPolicy(t, `{"classes":{"api_key":{"mode":"refuse"}}}`)
	if w := f.localRunWith(t, apiKey); w.Code != 422 || errorReason(w) != string(placement.ReasonPlacementCredential) {
		t.Errorf("api_key refused by policy = %d %s, want placement_credential", w.Code, w.Body)
	}
	f.setDeliveryPolicy(t, `{"classes":{"api_key":{"mode":"via_org"}}}`)
	// Past every credential check: only the final unsupported-local refusal remains.
	if w := f.localRunWith(t, apiKey); w.Code != 422 || errorReason(w) != string(placement.ReasonPlacementUnavailable) {
		t.Errorf("api_key delivered via_org = %d %s, want the final placement_unavailable", w.Code, w.Body)
	}
	if runs, grants := f.rowsWritten(t); runs != 0 || grants != 0 {
		t.Fatalf("refused local creates wrote %d runs and %d grants", runs, grants)
	}

	// Every kind at the persistence gate, which reads the same stored policy.
	admittedUnder := func(g types.GrantSpec, policy placement.DeliveryPolicy) bool {
		_, d, ref := f.h.srv.ownLocalGrantSpecs(localMemberRequest("bob"), types.RunPolicySpec{EligibleGrants: []types.GrantSpec{g}}, policy)
		return ref == nil && len(d) == 1 && d[0].mode == placement.ModeViaOrg
	}
	admitted := func(g types.GrantSpec) bool { return admittedUnder(g, storedPolicy(t, f.h.srv)) }
	for _, tc := range []struct {
		class string
		grant types.GrantSpec
	}{
		{placement.ClassAPIKey, apiKeyGrantSpec("corp.example", localCorpSecret)},
		{placement.ClassGitHubToken, types.GrantSpec{Kind: types.GrantGitHubToken, Scope: json.RawMessage(`{"repos":["acme/widgets"]}`)}},
	} {
		f.setDeliveryPolicy(t, `{"classes":{}}`)
		if admitted(tc.grant) {
			t.Errorf("%s admitted with no policy", tc.class)
		}
		f.setDeliveryPolicy(t, `{"classes":{"`+tc.class+`":{"mode":"via_org"}}}`)
		if !admitted(tc.grant) {
			t.Errorf("%s refused with the class delivered via_org", tc.class)
		}
	}
	// Never: the helper shape of git_pat, cloud_sts, and a resident-only class, even under a policy no
	// writer would accept (a stored one fails every door closed before persistence is reached).
	unshaped := placement.DeliveryPolicy{Classes: map[string]placement.ClassPolicy{}}
	for _, c := range []string{"git_pat_broker", "git_pat_helper", "cloud_sts", "env_secret", "oauth_subscription"} {
		unshaped.Classes[c] = placement.ClassPolicy{Mode: placement.ModeViaOrg}
	}
	for _, g := range []types.GrantSpec{
		{Kind: types.GrantGitPAT, Scope: json.RawMessage(`{"host":"git.corp.example","secret_name":"` + localCorpSecret + `"}`)},
		{Kind: types.GrantCloudSTS},
		{Kind: types.GrantEnvSecret, Scope: json.RawMessage(`{"name":"CORP_KEY","secret_name":"` + localCorpSecret + `"}`)},
	} {
		if admittedUnder(g, unshaped) {
			t.Errorf("%s admitted via_org", g.Kind)
		}
	}

	// Persistence: the grant is stored as authored (never OwnerOnly) and recorded via_org.
	ctx := context.Background()
	spec := types.RunPolicySpec{EligibleGrants: []types.GrantSpec{apiKeyGrantSpec("corp.example", localCorpSecret)}}
	f.setDeliveryPolicy(t, `{"classes":{}}`)
	refused := uuid.New()
	f.createRun(t, refused, "bob", types.PlacementLocal)
	if _, ok := f.h.srv.persistRunGrants(ctx, httptest.NewRecorder(), localMemberRequest("bob"), refused, time.Now().UTC(), spec, types.PlacementLocal, storedPolicy(t, f.h.srv)); ok {
		t.Fatal("an operator grant persisted for a local run with no via_org policy")
	}
	f.setDeliveryPolicy(t, `{"classes":{"api_key":{"mode":"via_org"}}}`)
	runID := uuid.New()
	f.createRun(t, runID, "bob", types.PlacementLocal)
	gw, ok := f.h.srv.persistRunGrants(ctx, httptest.NewRecorder(), localMemberRequest("bob"), runID, time.Now().UTC(), spec, types.PlacementLocal, storedPolicy(t, f.h.srv))
	if !ok {
		t.Fatal("a via_org grant was refused at persistence")
	}
	rows := mustListGrants(t, f.h.srv.cfg.Store, runID)
	if len(rows) != 1 || rows[0].Delivery != types.GrantDeliveryViaOrg || rows[0].Spec.OwnerOnly {
		t.Fatalf("stored %+v, want one via_org grant without OwnerOnly", rows)
	}
	if len(gw.delivery) != 1 || gw.delivery[0].GrantID != rows[0].ID || gw.delivery[0].Class != placement.ClassAPIKey || gw.delivery[0].Mode != placement.ModeViaOrg {
		t.Errorf("run.create delivery = %+v", gw.delivery)
	}
}
