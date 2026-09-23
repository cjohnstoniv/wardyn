// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"reflect"
	"strings"
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
	// host:PORT for the MITM entry, never bare.
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
	// …AND IN THE SCHEME THE OVERRIDE NAMES (walk-3). The proxy TERMINATES this
	// tunnel — it has to, because the sandbox holds a placeholder and the real
	// token can only be substituted into a request the proxy can see — and then
	// re-originates upstream. Without the prefix that upstream leg dialled TLS
	// at a server serving plain HTTP: builtin:dial-failed and a 502 on every one
	// of the SDK's 36-69 CONNECTs, so no role credentials, no model call, and
	// every case waiting on one timed out.
	if len(mitmHosts) != 1 || mitmHosts[0] != "http://"+net.JoinHostPort(theOverrideHost, "8090") {
		t.Errorf("MITM hosts = %v, want the override host on ITS OWN port 8090, prefixed http:// so the "+
			"MITM's upstream leg does not dial TLS at a plain-HTTP origin", mitmHosts)
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

// PRODUCTION IS BYTE-FOR-BYTE UNCHANGED, and this is the pin for it. A real
// portal serves TLS, so its entry must carry NO scheme prefix — the exact
// spelling every deployment has today, parsed by a sidecar that predates the
// prefix to exactly the same host and port.
func TestSSOPortalMITMEntry_ProductionIsUnprefixedAndPlainHTTPIsExplicit(t *testing.T) {
	for _, tc := range []struct {
		name     string
		override string
		want     string
	}{
		{"no override — the real portal", "", "portal.sso.eu-west-2.amazonaws.com:443"},
		{"an https override", "https://portal.corp.example", "portal.corp.example:443"},
		{"an https override on its own port", "https://portal.corp.example:9443", "portal.corp.example:9443"},
		{"the plain-http fake", theOverride, "http://" + theOverrideHost + ":8090"},
		{"a plain-http override on the default port", "http://fake.internal", "http://fake.internal:80"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ssoPortalMITMEntry("eu-west-2", tc.override)
			if got != tc.want {
				t.Errorf("ssoPortalMITMEntry(%q) = %q, want %q", tc.override, got, tc.want)
			}
			// Said the other way round, because this is the half that matters: the
			// prefix appears for an http:// override and NOWHERE else. A prefix on
			// a real portal would have the proxy dial cleartext at AWS.
			if strings.HasPrefix(got, "http://") != strings.HasPrefix(strings.ToLower(tc.override), "http://") {
				t.Errorf("ssoPortalMITMEntry(%q) = %q — the cleartext prefix does not follow the override's scheme",
					tc.override, got)
			}
		})
	}
}

// THE PIN IS AUTHORED, AND IT IS AUTHORED FROM THE SNAPSHOT (docs REVIEW-3
// coverage note). The proxy enforces it (internal/egress/proxy), and the proxy's
// tests supply the rule by hand — so nothing checked that a real dispatch
// actually writes one. A grant authored without it is a rule the sidecar reads
// as unpinned, and the injected session goes back to riding every request to the
// portal host, including the `POST /logout` that ends its owner's sign-in
// session for every run they have.
func TestAuthorBedrockSSOInjection_ScopeCarriesThePathPin(t *testing.T) {
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
	var scope struct {
		PinPath  string              `json:"pin_path"`
		PinQuery map[string]string   `json:"pin_query"`
		Snapshot awsSSOScopeSnapshot `json:"snapshot"`
	}
	if err := json.Unmarshal(captured.grants[0].Spec.Scope, &scope); err != nil {
		t.Fatalf("grant scope is not JSON: %v", err)
	}
	if scope.PinPath != "/federation/credentials" {
		t.Errorf("pin_path = %q, want /federation/credentials — GetRoleCredentials is the ONE call this "+
			"session may ride", scope.PinPath)
	}
	// FROM THE SNAPSHOT, not from anywhere else: the snapshot is what the
	// resolver re-compares against the roster, so pinning from the same value
	// keeps the wire and the resolve talking about one pair.
	want := map[string]string{"account_id": scope.Snapshot.SSOAccountID, "role_name": scope.Snapshot.SSORoleName}
	if !reflect.DeepEqual(scope.PinQuery, want) {
		t.Errorf("pin_query = %v, want the snapshot's pair %v", scope.PinQuery, want)
	}
	if scope.Snapshot.SSOAccountID == "" || scope.Snapshot.SSORoleName == "" {
		t.Fatal("the snapshot carried no account/role, so this test proved nothing")
	}
	// …and it survives the decode the sidecar's config is built from.
	rule, err := injectionRuleFromScope(captured.grants[0].Spec.Scope)
	if err != nil {
		t.Fatalf("injectionRuleFromScope: %v", err)
	}
	if !rule.Pinned() {
		t.Error("the decoded rule is UNPINNED — the pin never reaches the sidecar")
	}
	if !rule.AllowsInjection(http.MethodGet, "/federation/credentials",
		"account_id="+want["account_id"]+"&role_name="+want["role_name"]) {
		t.Error("the dispatched GetRoleCredentials is refused by its own pin")
	}
	if rule.AllowsInjection(http.MethodPost, "/logout", "") {
		t.Error("POST /logout is allowed the session — it ends the owner's sign-in for every run they have")
	}
}
