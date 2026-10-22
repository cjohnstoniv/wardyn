// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The AWS re-signer (WARDYN_AWS_SIGV4_AT_PROXY) goes with the Bedrock lane it signs for: a boundary
// deny of the runtime host leaves the proxy no signing door at dispatch or at revive, and the live
// redeem of the session grant is refused.

const sigv4RuntimeHost = "bedrock-runtime.us-east-1.amazonaws.com"

var sigv4LaneHosts = []string{sigv4RuntimeHost, "bedrock.us-east-1.amazonaws.com"}

// boundaryIntegStore is integStore with a Wardyn boundary.
type boundaryIntegStore struct {
	*integStore
	b types.Boundary
}

func (s boundaryIntegStore) GetBoundary(context.Context) (types.Boundary, error) { return s.b, nil }

func (s boundaryIntegStore) PutBoundary(context.Context, types.BoundaryWrite, string) (types.Boundary, error) {
	return s.b, nil
}

func denyBoundary(deny ...string) types.Boundary {
	return types.Boundary{Mode: types.BoundaryHardLimits, Spec: types.BoundarySpec{DeniedDomains: &deny}}
}

// dispatchSigV4BedrockUnder is dispatchBedrockSSORun, re-signing on, under the boundary b.
func dispatchSigV4BedrockUnder(t *testing.T, b types.Boundary) *proxy.AWSSigV4Config {
	t.Helper()
	srv := providerRunFixture(t, types.SiteConfig{ModelProviders: providerBlock(awsSSOTestProvider())}, &capStore{}, nil)
	srv.cfg.Secrets = &memSecrets{m: map[string][]byte{}, owned: map[string]map[string][]byte{}}
	srv.cfg.AWSSSOProxyInject, srv.cfg.AWSSigV4AtProxy = true, true
	blob := createRenewalBlob()
	blob.ExpiresAt = time.Now().Add(2 * time.Hour)
	storeSSOBlobFor(t, srv, createRenewalOwner, blob)
	tok := providerAdminToken(srv, createRenewalOwner)
	srv.cfg.Store = boundaryIntegStore{integStore: srv.cfg.Store.(*integStore), b: b}
	if w := do(t, srv, http.MethodPost, "/api/v1/runs", tok, createRenewalBody); w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s, want 201", w.Code, w.Body.String())
	}
	return lastDispatched(t, srv.cfg.Runner.(*fakeRunner)).ProxyConfig.AWSSigV4
}

func TestBoundarySigV4WithheldAtDispatch(t *testing.T) {
	if g := dispatchSigV4BedrockUnder(t, denyBoundary("pypi.org")); g == nil || !slices.Equal(g.Hosts, sigv4LaneHosts) {
		t.Fatalf("control: aws_sigv4 = %+v, want the lane's re-signer on %v", g, sigv4LaneHosts)
	}
	if g := dispatchSigV4BedrockUnder(t, denyBoundary(sigv4RuntimeHost)); g != nil {
		t.Fatalf("aws_sigv4 = %+v under a boundary that denies %s: the withheld lane left a signing door", *g, sigv4RuntimeHost)
	}
}

func TestBoundarySigV4WithheldAtRevive(t *testing.T) {
	run := types.AgentRun{ID: uuid.New()}
	for _, tc := range []struct {
		deny string
		kept bool
	}{{"pypi.org", true}, {sigv4RuntimeHost, false}} {
		deny := []string{tc.deny}
		layers := []sealLayer{{rec: boundaryProvenance, egress: boundarySealEgress(types.BoundarySpec{DeniedDomains: &deny})}}
		cfg := &proxy.Config{RunID: run.ID, AWSSigV4: &proxy.AWSSigV4Config{GrantID: uuid.New(),
			PortalHost: "portal.sso.us-east-1.amazonaws.com", Hosts: sigv4LaneHosts}}
		re, rerr := reassertProxyCeiling(run, cfg, ownerCeiling{}, layers)
		if rerr != nil {
			t.Fatalf("deny %s: refused: %+v", tc.deny, rerr)
		}
		if kept := cfg.AWSSigV4 != nil; kept != tc.kept || slices.Contains(re.droppedLane, "aws_sigv4") == tc.kept {
			t.Errorf("deny %s: aws_sigv4 kept = %v (dropped %v), want kept = %v", tc.deny, kept, re.droppedLane, tc.kept)
		}
	}
}

func TestBoundarySigV4RedeemWithheld(t *testing.T) {
	s := fullyConfiguredBedrockServer()
	captured := &captureGrantStore{}
	s.cfg.Store = captured
	run := types.AgentRun{ID: uuid.New()}
	auth := ssoInjectAuth()
	auth.sigv4Hosts = sigv4LaneHosts
	_, _, sig, ok := s.authorBedrockSSOInjection(context.Background(), run, llmTransport{bedrock: auth},
		awsSSOScope{perUser: true, owner: "member@corp.example"}, nil)
	if !ok || sig == nil || len(captured.grants) != 1 {
		t.Fatalf("authored ok=%v sigv4=%+v grants=%d, want one re-signed session grant", ok, sig, len(captured.grants))
	}
	grant := captured.grants[0]
	for _, tc := range []struct {
		deny     string
		withheld bool
	}{{"pypi.org", false}, {sigv4RuntimeHost, true}} {
		t.Run(tc.deny, func(t *testing.T) {
			h, _ := newSecretsHarness(t)
			ast := newAuthzStore()
			ast.runs[run.ID] = run
			h.srv.cfg.Store = mintBoundaryStore{grantStore: grantStore{authzStore: ast, grants: []types.CredentialGrant{grant}}, b: denyBoundary(tc.deny)}
			h.broker.minted = broker.Minted{Kind: types.GrantAPIKey, JTI: "jti-1",
				Injection: &egress.InjectionRule{Host: sig.PortalHost, Header: awsSSOInjectHeader, SecretName: types.AWSSSOAccessTokenSecret, Format: "%s"}}
			rr := do(t, h.srv, http.MethodGet, "/api/v1/internal/injection/"+grant.ID.String(), h.mintRunToken(t, run.ID), "")
			refused := rr.Code == http.StatusForbidden && strings.Contains(rr.Body.String(), reasonGrantWithheld)
			if refused != tc.withheld {
				t.Fatalf("deny %s: redeem = %d %s, want withheld = %v", tc.deny, rr.Code, rr.Body.String(), tc.withheld)
			}
		})
	}
}
