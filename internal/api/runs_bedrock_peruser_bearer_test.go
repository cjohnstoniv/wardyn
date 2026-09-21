// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The per-user Bedrock BEARER lane (#153): a member stores a bedrock-api-key
// under their own principal and their runs authenticate with it.
//
// Every test here guards one of the two ways the widening goes wrong. The first
// is CROSS-SOURCE SUBSTITUTION — serving the operator's key to a member, or a
// member's to the operator — which per_user exists to refuse and which the
// obvious `For(owner).Get` would do silently, because that Get falls back to the
// operator row by contract. The second is an EMPTY row reading as "Bedrock is
// configured": a blank value would win the precedence chain, author a grant and
// surface as an upstream 403 naming neither the lane nor the empty secret.
package api

import (
	"context"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	perUserBearerMember   = "member@corp.example"
	perUserMemberBearer   = "member-own-bedrock-bearer-0001"
	perUserOperatorBearer = "operator-wide-bedrock-bearer-9"
)

// bearerScopedServer builds a Bedrock-ready server whose operator namespace
// holds operatorBearer (empty = none) beside the static SigV4 keys
// fullyConfiguredBedrockServer always stores, and whose member namespace holds
// memberBearer (empty = none).
//
// The static keys are deliberately left in place on every case: they are the
// lane resolveBedrockAuth falls through to, so a test that asserts "not ready"
// is asserting the per_user barrier held rather than that the server had
// nothing to offer.
func bearerScopedServer(t *testing.T, operatorBearer, memberBearer string) *Server {
	t.Helper()
	s := fullyConfiguredBedrockServer()
	sec := s.cfg.Secrets.(*memSecrets)
	if operatorBearer != "" {
		sec.m[bedrockAPIKeySecret] = []byte(operatorBearer)
	}
	if memberBearer != "" {
		if err := sec.For(perUserBearerMember).Put(context.Background(), bedrockAPIKeySecret, []byte(memberBearer)); err != nil {
			t.Fatalf("seed member bearer: %v", err)
		}
	}
	return s
}

func resolveFor(s *Server, sso awsSSOScope) bedrockAuth {
	return s.resolveBedrockAuth(context.Background(), "claude-code", false,
		true /* modelRun */, false /* refresh */, nil, sso)
}

func memberScope() awsSSOScope {
	return awsSSOScope{perUser: true, owner: perUserBearerMember}
}

// TestResolveBedrockAuth_PerUser_MemberOwnBearerIsSelected is the lane #153
// opened: a member who stored their own bedrock-api-key dispatches on it.
func TestResolveBedrockAuth_PerUser_MemberOwnBearerIsSelected(t *testing.T) {
	ba := resolveFor(bearerScopedServer(t, "", perUserMemberBearer), memberScope())
	if !ba.ready || !ba.bearer {
		t.Fatalf("member's OWN bearer: ready=%v bearer=%v, want both true", ba.ready, ba.bearer)
	}
	// The real token never enters the sandbox env — the proxy substitutes it on
	// the wire, which is why this lane is member-writable at all.
	if got := ba.env["AWS_BEARER_TOKEN_BEDROCK"]; got != "wardyn-proxy-injected" {
		t.Fatalf("sandbox env carries %q, want the placeholder sentinel", got)
	}
	for k, v := range ba.env {
		if v == perUserMemberBearer {
			t.Fatalf("the real bearer leaked into the sandbox env at %q", k)
		}
	}
}

// TestResolveBedrockAuth_PerUser_OperatorBearerIsNotServedToAMember is the
// no-cross-source-fallback boundary in the direction that costs money: the
// member has no key of their own, the operator has one, and the run must be
// refused rather than billed to the org on a credential nobody named.
//
// The control leg is what makes this an assertion about the SCOPE rather than
// about an empty store: the same server under the shared scope selects that
// very bearer.
func TestResolveBedrockAuth_PerUser_OperatorBearerIsNotServedToAMember(t *testing.T) {
	s := bearerScopedServer(t, perUserOperatorBearer, "")

	if ba := resolveFor(s, memberScope()); ba.ready || ba.bearer {
		t.Fatalf("a member with no bearer of their own got ready=%v bearer=%v — the operator's key stood in",
			ba.ready, ba.bearer)
	}
	// Control: the key IS there, and a shared run still picks it up.
	if ba := resolveFor(s, awsSSOScope{}); !ba.ready || !ba.bearer {
		t.Fatalf("shared scope: ready=%v bearer=%v, want the operator's bearer selected — "+
			"without this leg the case above proves nothing", ba.ready, ba.bearer)
	}
}

// TestResolveBedrockAuth_MemberBearerIsNotServedToTheOperator is the SAME
// boundary in reverse. A shared-scope resolve reads the operator namespace and
// only it, so one member's stored key can never become the deployment-wide
// credential.
func TestResolveBedrockAuth_MemberBearerIsNotServedToTheOperator(t *testing.T) {
	s := bearerScopedServer(t, "", perUserMemberBearer)
	sec := s.cfg.Secrets.(*memSecrets)
	delete(sec.m, bedrockAccessKeyIDSecret)
	delete(sec.m, bedrockSecretAccessKeySecret)

	if ba := resolveFor(s, awsSSOScope{}); ba.ready || ba.bearer {
		t.Fatalf("shared scope: ready=%v bearer=%v — a member's own key became the deployment-wide credential",
			ba.ready, ba.bearer)
	}
}

// TestResolveBedrockAuth_EmptyBearerReadsAsAbsent is the second trap. An empty
// or whitespace-only row must fall THROUGH as "no credential", never win the
// precedence chain as a configured one that fails later at dial time.
//
// Both namespaces are covered because the emptiness check sits on the shared
// read path too, and the per-user and shared legs assert DIFFERENT outcomes:
// per_user stops at its barrier (not ready at all), while a shared run falls
// through to the operator's resident SigV4 keys — ready, but pointedly not on
// the bearer lane.
func TestResolveBedrockAuth_EmptyBearerReadsAsAbsent(t *testing.T) {
	for _, blank := range []string{"", "   ", "\n\t "} {
		t.Run("per_user member row "+quoteBlank(blank), func(t *testing.T) {
			s := fullyConfiguredBedrockServer()
			sec := s.cfg.Secrets.(*memSecrets)
			if err := sec.For(perUserBearerMember).Put(context.Background(), bedrockAPIKeySecret, []byte(blank)); err != nil {
				t.Fatalf("seed blank member bearer: %v", err)
			}
			ba := resolveFor(s, memberScope())
			if ba.bearer {
				t.Fatalf("a blank member bearer selected the bearer lane — it would 403 upstream naming neither the lane nor the empty secret")
			}
			if ba.ready {
				t.Fatalf("a blank member bearer read as ready; per_user must stop at its barrier")
			}
		})
		t.Run("operator row "+quoteBlank(blank), func(t *testing.T) {
			s := fullyConfiguredBedrockServer()
			s.cfg.Secrets.(*memSecrets).m[bedrockAPIKeySecret] = []byte(blank)
			ba := resolveFor(s, awsSSOScope{})
			if ba.bearer {
				t.Fatalf("a blank operator bearer selected the bearer lane instead of falling through")
			}
			if !ba.ready {
				t.Fatalf("a blank operator bearer blocked the fall-through to the resident SigV4 keys")
			}
		})
	}
}

// quoteBlank names a whitespace-only fixture in a subtest name.
func quoteBlank(s string) string {
	switch s {
	case "":
		return "empty"
	case "   ":
		return "spaces"
	default:
		return "whitespace"
	}
}

// TestBedrockBearerFor_PerUserWithNoOwnerIsAbsent is the fail-closed direction
// of awsSSOScope.namespaced(): a per_user scope that names nobody must read as
// "no credential", never as the operator's. It is the scope a failed roster or
// identity read produces, so the alternative is a store blip credentialing a
// member's run with the deployment-wide key.
func TestBedrockBearerFor_PerUserWithNoOwnerIsAbsent(t *testing.T) {
	s := bearerScopedServer(t, perUserOperatorBearer, "")
	if got := s.bedrockBearerFor(context.Background(), awsSSOScope{perUser: true}); len(got) > 0 {
		t.Fatalf("an owner-less per_user scope read the operator's bearer (%d bytes)", len(got))
	}
}

// TestMechanismSatisfied_PerUserComparesTheExactSubLane pins the dispatch gate's
// half of the promise. Under per_user the four Bedrock arms must NOT fold
// together: a bedrock_sso row is not satisfied by a bearer that fired
// underneath it, nor the reverse. The coarse fold stays for `shared`, where an
// admin's "Bedrock" legitimately means any of its lanes.
func TestMechanismSatisfied_PerUserComparesTheExactSubLane(t *testing.T) {
	perUser := func(m types.AgentMechanism) types.AgentProvider {
		return types.AgentProvider{ID: "claude-code", Mechanism: m,
			CredentialSource: types.CredentialSourcePerUser}
	}
	for _, tc := range []struct {
		name     string
		row      types.AgentProvider
		selected types.AgentMechanism
		want     bool
	}{
		{"bearer row, bearer selected", perUser(types.AgentMechanismBedrockBearer), types.AgentMechanismBedrockBearer, true},
		{"bearer row, sso selected", perUser(types.AgentMechanismBedrockBearer), types.AgentMechanismBedrockSSO, false},
		{"sso row, bearer selected", perUser(types.AgentMechanismBedrockSSO), types.AgentMechanismBedrockBearer, false},
		{"sso row, sso selected", perUser(types.AgentMechanismBedrockSSO), types.AgentMechanismBedrockSSO, true},
		// The shared fold is deliberately unchanged.
		{"shared bearer row, sso selected", types.AgentProvider{ID: "claude-code",
			Mechanism: types.AgentMechanismBedrockBearer}, types.AgentMechanismBedrockSSO, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := mechanismSatisfied(tc.row, tc.selected, true); got != tc.want {
				t.Fatalf("mechanismSatisfied(%s row, %s selected) = %v, want %v",
					tc.row.Mechanism, tc.selected, got, tc.want)
			}
		})
	}
}

// TestSetupBedrock_PerUser_BearerPresentIsTheCallersOwn keeps the PROBE honest
// with the RESOLVE. `present` is the operator's name map, so the pre-#153 code
// reported the operator's bearer to every caller; under per_user the answer has
// to be the caller's own row, or the setup page grades ready off a credential
// the resolve refuses to serve them.
func TestSetupBedrock_PerUser_BearerPresentIsTheCallersOwn(t *testing.T) {
	operatorPresent := map[string]bool{bedrockAPIKeySecret: true}

	t.Run("member without their own bearer", func(t *testing.T) {
		s := bearerScopedServer(t, perUserOperatorBearer, "")
		if b := s.setupBedrock(context.Background(), operatorPresent, memberScope()); b.BearerPresent {
			t.Fatalf("BearerPresent=true off the OPERATOR's key — setup would grade ready for a member who has none")
		}
	})
	t.Run("member with their own bearer", func(t *testing.T) {
		s := bearerScopedServer(t, "", perUserMemberBearer)
		b := s.setupBedrock(context.Background(), map[string]bool{}, memberScope())
		if !b.BearerPresent {
			t.Fatalf("BearerPresent=false over a bearer this member's runs really authenticate with")
		}
		if !b.Ready {
			t.Fatalf("Ready=false with region, model and the member's own bearer all present")
		}
	})
	t.Run("the operator's own shared read is unchanged", func(t *testing.T) {
		s := bearerScopedServer(t, perUserOperatorBearer, "")
		if b := s.setupBedrock(context.Background(), operatorPresent, awsSSOScope{}); !b.BearerPresent {
			t.Fatalf("BearerPresent=false for the operator under a shared scope — pre-#153 behaviour changed")
		}
	})
}
