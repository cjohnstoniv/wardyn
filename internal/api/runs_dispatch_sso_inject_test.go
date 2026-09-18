// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ssoInjectAuth is the resolved captured-SSO posture a Phase-B dispatch carries.
func ssoInjectAuth() bedrockAuth {
	return bedrockAuth{
		ready: true, ssoInject: true, ssoProxyInject: true,
		ssoAccountID: "111122223333", ssoRoleName: "WardynAgent", ssoRegion: "eu-west-2",
		region: "us-east-1", model: "m",
	}
}

// The grant an ssoInject dispatch authors is the WHOLE security contract of
// Phase B: one api_key grant, bound to the run's own portal host, carrying the
// forced header and the IMMUTABLE dispatch-time scope snapshot the resolver
// compares against the live roster.
func TestAuthorBedrockSSOInjection_GrantScopeAndMITMEntry(t *testing.T) {
	s := fullyConfiguredBedrockServer()
	s.cfg.Store = vetoGrantStore{}
	run := types.AgentRun{ID: uuid.New()}
	llm := llmTransport{bedrock: ssoInjectAuth()}
	injections, mitmHosts, ok := s.authorBedrockSSOInjection(context.Background(), run, llm,
		awsSSOScope{perUser: true, owner: "member@corp.example"}, nil)
	if !ok {
		t.Fatal("authorBedrockSSOInjection failed; want ok")
	}
	if len(injections) != 1 {
		t.Fatalf("injections = %d, want exactly 1", len(injections))
	}
	wantHost := "portal.sso.eu-west-2.amazonaws.com"
	if got := injections[0].Rule.Host; got != wantHost {
		t.Errorf("injection scope host = %q, want the BARE portal host %q (buildInjector keys byHost verbatim)", got, wantHost)
	}
	if !injections[0].Rule.RequireTLS {
		t.Error("RequireTLS is false on a production deployment — the token would be injectable in cleartext")
	}
	if got, want := injections[0].Rule.Header, "x-amz-sso_bearer_token"; got != want {
		t.Errorf("header = %q, want %q", got, want)
	}
	// host:PORT for the MITM entry, never bare (F037).
	wantMITM := net.JoinHostPort(wantHost, "443")
	if len(mitmHosts) != 1 || mitmHosts[0] != wantMITM {
		t.Errorf("MITM hosts = %v, want exactly %q", mitmHosts, wantMITM)
	}
}

// The SNAPSHOT (I3): every field resolveAWSSSOInjection re-derives and compares.
// Authored HERE, at dispatch, so an admin flipping the roster mid-run cannot
// make a held run resolve a different principal's credential.
func TestAuthorBedrockSSOInjection_ScopeCarriesTheDispatchTimeSnapshot(t *testing.T) {
	s := fullyConfiguredBedrockServer()
	captured := &captureGrantStore{}
	s.cfg.Store = captured
	llm := llmTransport{bedrock: ssoInjectAuth()}
	if _, _, ok := s.authorBedrockSSOInjection(context.Background(), types.AgentRun{ID: uuid.New()}, llm,
		awsSSOScope{perUser: true, owner: "member@corp.example"}, nil); !ok {
		t.Fatal("authorBedrockSSOInjection failed; want ok")
	}
	if len(captured.grants) != 1 {
		t.Fatalf("grants = %d, want 1", len(captured.grants))
	}
	g := captured.grants[0]
	if g.Spec.Kind != types.GrantAPIKey {
		t.Errorf("grant kind = %q, want api_key", g.Spec.Kind)
	}
	if g.Spec.TTLSeconds != 3600 {
		t.Errorf("grant TTL = %d, want 3600 (the bearer precedent)", g.Spec.TTLSeconds)
	}
	var scope struct {
		Host       string            `json:"host"`
		Header     string            `json:"header"`
		Format     string            `json:"format"`
		SecretName string            `json:"secret_name"`
		Snapshot   map[string]string `json:"snapshot"`
	}
	if err := json.Unmarshal(g.Spec.Scope, &scope); err != nil {
		t.Fatalf("grant scope is not the expected JSON: %v", err)
	}
	if scope.SecretName != types.AWSSSOAccessTokenSecret || scope.Format != "%s" {
		t.Errorf("scope = %+v, want the AWS SSO sentinel with a bare %%s format", scope)
	}
	for k, want := range map[string]string{
		"owner_subject":     "member@corp.example",
		"credential_source": string(types.CredentialSourcePerUser),
		"mechanism":         string(types.AgentMechanismBedrockSSO),
		"sso_account_id":    "111122223333",
		"sso_role_name":     "WardynAgent",
		"region":            "eu-west-2",
	} {
		if got := scope.Snapshot[k]; got != want {
			t.Errorf("snapshot[%q] = %q, want %q — the resolver compares every one of these against the LIVE roster and refuses any drift", k, got, want)
		}
	}
}

// The plain-http test fake is the ONE deployment that may inject without TLS,
// and it is a deployment that already refused to boot without
// WARDYN_ALLOW_TEST_ENDPOINTS.
func TestAuthorBedrockSSOInjection_EndpointOverrideDropsRequireTLS(t *testing.T) {
	s := fullyConfiguredBedrockServer()
	s.cfg.Store = vetoGrantStore{}
	s.cfg.AWSSSOEndpointOverride = theOverride
	llm := llmTransport{bedrock: ssoInjectAuth()}
	injections, mitmHosts, ok := s.authorBedrockSSOInjection(context.Background(), types.AgentRun{ID: uuid.New()}, llm,
		awsSSOScope{perUser: true, owner: "m"}, nil)
	if !ok {
		t.Fatal("authorBedrockSSOInjection failed; want ok")
	}
	if injections[0].Rule.Host != theOverrideHost {
		t.Errorf("scope host = %q, want the override host %q", injections[0].Rule.Host, theOverrideHost)
	}
	if injections[0].Rule.RequireTLS {
		t.Error("RequireTLS stayed true under the endpoint override — the plain-http fake serves no TLS, so the run would be refused rather than credentialed")
	}
	// THE OVERRIDE'S OWN PORT (security NIT-2). Authored at 443 the tunnel is
	// never terminated and the header is never injected — the fake lane would
	// carry a placeholder to a 401 with nothing in the proxy to say why.
	if len(mitmHosts) != 1 || mitmHosts[0] != net.JoinHostPort(theOverrideHost, "8090") {
		t.Errorf("MITM hosts = %v, want the override host on ITS OWN port 8090", mitmHosts)
	}
	// …and the egress list authored the same port, or AuthoredPortFor would
	// withhold the credential on it.
	if hosts := ssoEgressHosts("eu-west-2", theOverride); len(hosts) != 2 || hosts[1] != theOverrideHost+":8090" {
		t.Errorf("egress hosts = %v, want the bare host and its authored port — the MITM entry and the allowlist must name the same port", hosts)
	}
}

// captureGrantStore records what CreateGrant was handed.
type captureGrantStore struct {
	vetoGrantStore
	grants []types.CredentialGrant
}

func (c *captureGrantStore) CreateGrant(_ context.Context, g types.CredentialGrant) (types.CredentialGrant, error) {
	c.grants = append(c.grants, g)
	return g, nil
}
