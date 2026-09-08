// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// vetoGrantStore is a minimal store.Store whose CreateGrant always succeeds —
// enough for planArtifactRedirect's grant-authoring path without a real DB.
type vetoGrantStore struct{ store.Store }

func (vetoGrantStore) CreateGrant(_ context.Context, g types.CredentialGrant) (types.CredentialGrant, error) {
	return g, nil
}

// newVetoRedirectServer builds a Server wired for planArtifactRedirect: the
// named secret exists, grant creation succeeds, and an audit recorder
// captures every run.artifact.redirect event.
func newVetoRedirectServer(secretName string) (*Server, *recRecorder) {
	rec := &recRecorder{}
	s := &Server{cfg: Config{
		Store:   vetoGrantStore{},
		Secrets: &memSecrets{m: map[string][]byte{secretName: []byte("tok")}},
		Audit:   rec,
		Now:     time.Now,
	}}
	return s, rec
}

// TestPlanArtifactRedirect_ToGatewayRefused: a redirect whose To names the
// configured internal model gateway is refused — a colliding row would swap
// an artifact token onto model traffic via buildInjector's last-write-wins
// byHost map.
func TestPlanArtifactRedirect_ToGatewayRefused(t *testing.T) {
	s, rec := newVetoRedirectServer("corp-token")
	s.cfg.LLMGateways = map[string]string{"api.anthropic.com": "https://llm-gateway.corp.internal"}
	sc := types.SiteConfig{EgressRedirects: []types.EgressRedirect{
		{From: "registry.example.com", To: "llm-gateway.corp.internal", TokenSecretRef: "corp-token"},
	}}
	run := types.AgentRun{ID: uuid.New()}
	plan := s.planArtifactRedirect(context.Background(), run, sc, []string{"registry.example.com"})
	if len(plan.injections) != 0 {
		t.Fatalf("a redirect To the gateway must not author an injection, got %+v", plan.injections)
	}
	if len(plan.mitmHosts) != 0 {
		t.Fatalf("a redirect To the gateway must not become MITM-eligible, got %v", plan.mitmHosts)
	}
	assertRefusalAudited(t, rec, "llm-gateway.corp.internal")
}

// TestPlanArtifactRedirect_ToPublicProviderRefused: a redirect whose To names
// the public provider host directly (the latent collision this veto's other
// half closes) is refused the same way.
func TestPlanArtifactRedirect_ToPublicProviderRefused(t *testing.T) {
	s, rec := newVetoRedirectServer("corp-token")
	sc := types.SiteConfig{EgressRedirects: []types.EgressRedirect{
		{From: "registry.example.com", To: "api.anthropic.com", TokenSecretRef: "corp-token"},
	}}
	run := types.AgentRun{ID: uuid.New()}
	plan := s.planArtifactRedirect(context.Background(), run, sc, []string{"registry.example.com"})
	if len(plan.injections) != 0 {
		t.Fatalf("a redirect To api.anthropic.com must not author an injection, got %+v", plan.injections)
	}
	assertRefusalAudited(t, rec, "api.anthropic.com")
}

// TestPlanArtifactRedirect_ToBedrockHostRefused pins B1, the reject-direction
// sibling of F019: this veto is a REJECT test, but it consulted the
// anthropic/openai-only isModelProviderHost, so a redirect whose To named the
// Bedrock lane's data/control host (or a WARDYN_BEDROCK_BASE_URL endpoint) was
// still allowed to author an artifact-token injection — and buildInjector's
// byHost map is last-write-wins, so that row could swap the artifact token onto
// the run's Bedrock traffic, which resolveBedrockAuth's preferred bearer mode
// injects proxy-side.
//
// RED on the base tree: the two bedrock rows plan an injection. The
// Bedrock-disabled row is here so a fix that simply refused every amazonaws.com
// host could not pass.
func TestPlanArtifactRedirect_ToBedrockHostRefused(t *testing.T) {
	for _, tc := range []struct {
		name          string
		bedrockRegion string
		bedrockBase   string
		to            string
		refused       bool
	}{
		{"bedrock data plane", "us-east-1", "", "bedrock-runtime.us-east-1.amazonaws.com", true},
		{"bedrock control plane", "us-east-1", "", "bedrock.us-east-1.amazonaws.com", true},
		{
			"WARDYN_BEDROCK_BASE_URL override host", "us-east-1",
			"https://vpce-abc.bedrock-runtime.us-east-1.vpce.amazonaws.com",
			"vpce-abc.bedrock-runtime.us-east-1.vpce.amazonaws.com", true,
		},
		{"bedrock host with Bedrock disabled still plans", "", "", "bedrock-runtime.us-east-1.amazonaws.com", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, rec := newVetoRedirectServer("corp-token")
			s.cfg.BedrockRegion = tc.bedrockRegion
			s.cfg.BedrockBaseURL = tc.bedrockBase
			sc := types.SiteConfig{EgressRedirects: []types.EgressRedirect{
				{From: "registry.example.com", To: tc.to, TokenSecretRef: "corp-token"},
			}}
			run := types.AgentRun{ID: uuid.New()}
			plan := s.planArtifactRedirect(context.Background(), run, sc, []string{"registry.example.com"})
			if tc.refused {
				if len(plan.injections) != 0 {
					t.Fatalf("a redirect To %q must not author an injection, got %+v", tc.to, plan.injections)
				}
				if len(plan.mitmHosts) != 0 {
					t.Fatalf("a redirect To %q must not become MITM-eligible, got %v", tc.to, plan.mitmHosts)
				}
				assertRefusalAudited(t, rec, tc.to)
				return
			}
			if len(plan.injections) != 1 {
				t.Fatalf("with Bedrock disabled the redirect must still plan an injection, got %+v", plan.injections)
			}
		})
	}
}

// TestPlanArtifactRedirect_UnrelatedToStillPlans: the veto is narrow — an
// ordinary corp-mirror redirect unrelated to any model-provider host still
// plans normally.
func TestPlanArtifactRedirect_UnrelatedToStillPlans(t *testing.T) {
	s, _ := newVetoRedirectServer("corp-token")
	sc := types.SiteConfig{EgressRedirects: []types.EgressRedirect{
		{From: "registry.npmjs.org", To: "artifactory.corp", TokenSecretRef: "corp-token", Ecosystem: "npm"},
	}}
	run := types.AgentRun{ID: uuid.New()}
	plan := s.planArtifactRedirect(context.Background(), run, sc, []string{"registry.npmjs.org"})
	if len(plan.injections) != 1 {
		t.Fatalf("an unrelated redirect must still plan an injection, got %+v", plan.injections)
	}
	if len(plan.mitmHosts) != 1 || plan.mitmHosts[0] != "artifactory.corp:443" {
		t.Fatalf("an unrelated redirect must still become MITM-eligible, got %v", plan.mitmHosts)
	}
}

// assertRefusalAudited checks that exactly one run.artifact.redirect audit
// event fired, naming host, and that no injection-shaped success event rode
// along with it.
func assertRefusalAudited(t *testing.T, rec *recRecorder, host string) {
	t.Helper()
	var found bool
	for _, ev := range rec.events {
		if ev.Action != "run.artifact.redirect" {
			continue
		}
		if ev.Outcome == "success" {
			t.Fatalf("a vetoed redirect must never reach the success/injection audit, got %+v", ev)
		}
		found = true
	}
	if !found {
		t.Fatalf("expected a run.artifact.redirect audit event for the refused host %q, got %+v", host, rec.events)
	}
}
