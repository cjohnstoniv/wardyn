// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The resident captured-SSO Bedrock lane (WARDYN_AWS_SSO_PROXY_INJECT off) meets the boundary's
// eligible grants as the proxy-injected session's grant (#3238): a boundary that lists grants and
// covers none of it keeps the session out of the sandbox, one that lists it does not.

// dispatchResidentSSOUnder is dispatchSigV4BedrockUnder with the session resident in the sandbox: the
// sandbox spec and the run.ceiling.reassert row (nil when none was written).
func dispatchResidentSSOUnder(t *testing.T, b types.Boundary) (runner.SandboxSpec, *types.AuditEvent) {
	t.Helper()
	srv := providerRunFixture(t, types.SiteConfig{ModelProviders: providerBlock(awsSSOTestProvider())}, &capStore{}, nil)
	srv.cfg.Secrets = &memSecrets{m: map[string][]byte{}, owned: map[string]map[string][]byte{}}
	blob := createRenewalBlob()
	blob.ExpiresAt = time.Now().Add(2 * time.Hour)
	storeSSOBlobFor(t, srv, createRenewalOwner, blob)
	tok := providerAdminToken(srv, createRenewalOwner)
	srv.cfg.Store = boundaryIntegStore{integStore: srv.cfg.Store.(*integStore), b: b}
	if w := do(t, srv, http.MethodPost, "/api/v1/runs", tok, createRenewalBody); w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s, want 201", w.Code, w.Body.String())
	}
	spec := lastDispatched(t, srv.cfg.Runner.(*fakeRunner))
	var row *types.AuditEvent
	for _, ev := range srv.cfg.Audit.(*recRecorder).snapshot() {
		if ev.Action == "run.ceiling.reassert" {
			row = &ev
		}
	}
	return spec, row
}

// proxySSOGrant is the grant the proxy-injected lane is given for createRenewalBlob's session.
func proxySSOGrant(t *testing.T) types.GrantSpec {
	t.Helper()
	s := fullyConfiguredBedrockServer()
	captured := &captureGrantStore{}
	s.cfg.Store = captured
	auth := ssoInjectAuth()
	auth.ssoRegion = createRenewalBlob().Region
	if _, _, _, ok := s.authorBedrockSSOInjection(context.Background(), types.AgentRun{ID: uuid.New()}, llmTransport{bedrock: auth},
		awsSSOScope{perUser: true, owner: createRenewalOwner}, nil); !ok || len(captured.grants) != 1 {
		t.Fatalf("authored ok=%v grants=%d, want the proxy-injected session's grant", ok, len(captured.grants))
	}
	return captured.grants[0].Spec
}

func TestBoundaryResidentSSOGrant(t *testing.T) {
	listing := func(grants ...types.GrantSpec) types.Boundary {
		return types.Boundary{Mode: types.BoundaryHardLimits, Spec: types.BoundarySpec{EligibleGrants: &grants}}
	}
	for _, tc := range []struct {
		name     string
		b        types.Boundary
		withheld bool
	}{
		{"no_limits", types.Boundary{Mode: types.BoundaryNoLimits}, false},
		{"lists the proxy-injected session's grant", listing(proxySSOGrant(t)), false},
		{"lists no grant", listing(), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec, row := dispatchResidentSSOUnder(t, tc.b)
			if staged := spec.SecretEnv[awsSSOConfigEnvVar] != "" || spec.Env[awsSSOConfigEnvVar] != ""; staged == tc.withheld {
				t.Fatalf("captured session staged = %v, want %v", staged, !tc.withheld)
			}
			if !tc.withheld {
				return
			}
			if row == nil || !strings.Contains(string(row.Data), `"boundary":true`) || !strings.Contains(string(row.Data), `"dropped_broker_lanes":["`+bedrockCeilingLane+`"]`) {
				t.Errorf("run.ceiling.reassert = %v, want the Bedrock lane dropped and the boundary named", row)
			}
		})
	}
}
