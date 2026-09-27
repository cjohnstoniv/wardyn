// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestAuthorSubscriptionInjection_GatewayHostAndMITMEntry pins the third
// failure mode #137 fixes: pointing ANTHROPIC_BASE_URL at a configured
// gateway is not enough by itself — the injection grant's Host must match
// what the sandbox actually dials (or the proxy's injector never resolves an
// entry for it), and that host must ALSO be added to this run's per-run MITM
// host list (or the CONNECT tunnel to the gateway stays opaque and the live
// token is never swapped in, silently defeating the feature).
//
// Unset must stay byte-identical to today: host == the vendor default, and
// NO extra MITM entry (isMITMHost already recognizes the built-in hosts
// without one).
func TestAuthorSubscriptionInjection_GatewayHostAndMITMEntry(t *testing.T) {
	newDispatchServer := func(gatewayBase string) *Server {
		s := &Server{cfg: Config{Store: vetoGrantStore{}, Now: time.Now}}
		if gatewayBase != "" {
			s.cfg.LLMGateways = map[string]string{"api.anthropic.com": gatewayBase}
		}
		return s
	}
	run := types.AgentRun{ID: uuid.New()}

	t.Run("unset: vendor host, no extra MITM entry", func(t *testing.T) {
		s := newDispatchServer("")
		policy := &types.RunPolicySpec{}
		injections, mitmHosts, ok := s.authorSubscriptionInjection(context.Background(), run,
			llmTransport{injectSub: true, subscription: true}, policy, nil)
		if !ok {
			t.Fatal("expected ok=true")
		}
		if mitmHosts != nil {
			t.Fatalf("unset gateway must add NO extra MITM entry (isMITMHost already covers the vendor host), got %v", mitmHosts)
		}
		if len(injections) != 1 || injections[0].Rule.Host != "api.anthropic.com" {
			t.Fatalf("expected one injection targeting api.anthropic.com, got %+v", injections)
		}
		if !domainAllowedExact(policy.AllowedDomains, "api.anthropic.com") {
			t.Fatalf("expected api.anthropic.com unioned into egress, got %v", policy.AllowedDomains)
		}
	})

	t.Run("configured gateway: injection host + MITM entry both move to it", func(t *testing.T) {
		s := newDispatchServer("https://llm-gateway.corp.internal:8443")
		policy := &types.RunPolicySpec{}
		injections, mitmHosts, ok := s.authorSubscriptionInjection(context.Background(), run,
			llmTransport{injectSub: true, subscription: true}, policy, nil)
		if !ok {
			t.Fatal("expected ok=true")
		}
		if len(injections) != 1 || injections[0].Rule.Host != "llm-gateway.corp.internal" {
			t.Fatalf("expected the injection to target the configured gateway host, got %+v", injections)
		}
		if want := []string{"llm-gateway.corp.internal:8443"}; len(mitmHosts) != 1 || mitmHosts[0] != want[0] {
			t.Fatalf("expected the gateway added as a port-scoped MITM entry %v, got %v — without it the CONNECT "+
				"to the gateway stays an opaque tunnel and the live token is never injected", want, mitmHosts)
		}
		if !domainAllowedExact(policy.AllowedDomains, "llm-gateway.corp.internal") {
			t.Fatalf("expected the gateway host unioned into egress, got %v", policy.AllowedDomains)
		}
		// The grant the sink will later validate must be a host this deployment's
		// subscriptionInjectionHostAllowed actually accepts (injection.go) — proving
		// the two sides of the fix agree on the same host.
		if !s.subscriptionInjectionHostAllowed(injections[0].Rule.Host) {
			t.Fatalf("the authored injection host %q is not accepted by the sink's own allowlist", injections[0].Rule.Host)
		}
	})
}
