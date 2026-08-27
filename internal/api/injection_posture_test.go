// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

// sentinelInjection is the shape authorSubscriptionInjection writes: an api_key
// grant whose secret_name is a sentinel resolving live at the sink, host-pinned
// to Anthropic so the host pin (H2) is satisfied and posture is what decides.
func sentinelInjection(name string) *egress.InjectionRule {
	return &egress.InjectionRule{
		Host: "api.anthropic.com", Header: "Authorization",
		SecretName: name, Format: "Bearer %s",
	}
}

// Off-posture, the sink must refuse BOTH sentinels — the resident subscription
// lane and the Wardyn-managed lane. Gating only one is not a fix: the managed
// lane is a default fallback for every claude-code run, needing no policy, no
// integration id and no flag.
func TestInternalInjection_RefusesSharedSubscriptionOffPosture(t *testing.T) {
	for _, name := range []string{types.SubscriptionOAuthSecret, types.ManagedOAuthSecret} {
		h, _ := newSecretsHarness(t)
		h.srv.cfg.SubscriptionPostureOK = false
		h.srv.cfg.SubscriptionPostureReason = "OIDC/SSO is configured"
		runID := uuid.New()
		token := h.mintRunToken(t, runID)
		h.broker.minted = broker.Minted{
			Kind: types.GrantAPIKey, JTI: "jti-posture", Injection: sentinelInjection(name),
		}
		rr := do(t, h.srv, http.MethodGet, "/api/v1/internal/injection/"+uuid.NewString(), token, "")
		if rr.Code != http.StatusForbidden {
			t.Fatalf("%s off-posture must be 403, got %d body=%s", name, rr.Code, rr.Body.String())
		}
		// This assertion also pins the ORDER. The posture check sits ahead of the
		// nil-provider check and ahead of provider.Current() — which for the resident
		// lane shells out to the operator's own `claude` and rotates their
		// ~/.claude/.credentials.json. If posture were checked later this body would
		// read "token provider is not configured" instead.
		if !strings.Contains(rr.Body.String(), "OIDC/SSO is configured") {
			t.Fatalf("refusal must say WHY, and must precede the provider checks: %s", rr.Body.String())
		}
		if strings.Contains(strings.ToLower(rr.Body.String()), "bearer") {
			t.Fatalf("refusal body must not carry credential material: %s", rr.Body.String())
		}
	}
}

// The drift case, which dispatch-time gating cannot see at all: the grant was
// authored while the daemon was single-user, the sandbox outlived a restart, and
// the still-running proxy re-resolves the token (resident tokens carry an expiry
// and the injector refreshes near the margin) against a now-multi-user daemon.
func TestInternalInjection_RefusesGrantAuthoredUnderAnotherPosture(t *testing.T) {
	h, _ := newSecretsHarness(t)
	h.srv.cfg.SubscriptionPostureOK = true // authored here
	runID := uuid.New()
	token := h.mintRunToken(t, runID)
	h.broker.minted = broker.Minted{
		Kind: types.GrantAPIKey, JTI: "jti-drift",
		Injection: sentinelInjection(types.SubscriptionOAuthSecret),
	}
	// ...daemon restarts into a multi-user posture; the grant row is unchanged.
	h.srv.cfg.SubscriptionPostureOK = false
	h.srv.cfg.SubscriptionPostureReason = "the Kubernetes runner is a multi-user deployment"
	rr := do(t, h.srv, http.MethodGet, "/api/v1/internal/injection/"+uuid.NewString(), token, "")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("a grant authored under a permissive posture must not resolve under a restrictive one; got %d body=%s", rr.Code, rr.Body.String())
	}
}
