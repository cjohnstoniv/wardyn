// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// provider_access_test.go: MP-12's per-provider grading (SetupProviderAccess),
// generalising SetupModelAccess's single hardcoded AWS-SSO lane to every kind
// a person may be granted. Every test reads through providerAccessFor/
// setupProviderAccess exactly as GET /setup/status does — never the lower
// helpers (awsSSOCredentialState, ownSecret) directly — so a passing test is an
// assertion about the wire shape, not about a helper this file merely calls.

const paOwner = "alice@example.com"

func paKeyProvider(id string, kind types.ModelProviderKind) types.ModelProvider {
	return types.ModelProvider{ID: id, UID: uuid.NewString(), Kind: kind,
		Harnesses: []types.ProviderHarness{{Harness: "claude-code", Model: "m"}}}
}

func paSSOProvider(id string) types.ModelProvider {
	return types.ModelProvider{ID: id, UID: uuid.NewString(), Kind: types.ModelProviderBedrockSSO,
		Bedrock: &types.BedrockSettings{Region: "us-west-2", SSOStartURL: "https://acme.awsapps.com/start",
			SSOAccountID: "123456789012", SSORoleName: "BedrockUser"},
		Harnesses: []types.ProviderHarness{{Harness: "claude-code", Model: "m"}}}
}

// TestProviderAccess_KeyKinds_LiveOrNotConfigured walks the "no key probe"
// rule the issue names verbatim: stored -> live, absent -> not_configured, for
// every typed-key/token kind, with custom_endpoint alone saying "token".
func TestProviderAccess_KeyKinds_LiveOrNotConfigured(t *testing.T) {
	for _, kind := range []types.ModelProviderKind{
		types.ModelProviderAnthropicAPIKey, types.ModelProviderOpenAIAPIKey,
		types.ModelProviderBedrockBearer, types.ModelProviderCustomEndpoint,
	} {
		t.Run(string(kind), func(t *testing.T) {
			h, sec := newSecretsHarness(t)
			p := paKeyProvider("prov", kind)

			got := h.srv.providerAccessFor(context.Background(), p, paOwner)
			if got.State != modelAccessNotConfigured {
				t.Fatalf("no key stored: state = %q, want not_configured", got.State)
			}
			wantAction := providerAccessAddKeyAction
			if kind == types.ModelProviderCustomEndpoint {
				wantAction = providerAccessAddTokenAction
			}
			if got.Action != wantAction {
				t.Errorf("no key stored: action = %q, want %q", got.Action, wantAction)
			}

			_ = sec.For(paOwner).Put(context.Background(), providerSecretName(p.UID, providerKeyPart), []byte("a-real-key-value-0123456789"))
			got = h.srv.providerAccessFor(context.Background(), p, paOwner)
			if got.State != modelAccessLive {
				t.Fatalf("key stored: state = %q, want live", got.State)
			}
			if got.Action != "" || got.Deadline != "" {
				t.Errorf("live carries no action/deadline, got action=%q deadline=%q", got.Action, got.Deadline)
			}
		})
	}
}

// TestProviderAccess_KeyKinds_OwnNamespaceOnly is the strict-read guarantee
// providerBedrockRefusal and providerSubscriptionRefusal already carry into
// dispatch: another person's — or the operator's — stored key must never
// read this person's row as live.
func TestProviderAccess_KeyKinds_OwnNamespaceOnly(t *testing.T) {
	h, sec := newSecretsHarness(t)
	p := paKeyProvider("prov", types.ModelProviderAnthropicAPIKey)
	sec.m[providerSecretName(p.UID, providerKeyPart)] = []byte("operator-key-must-not-leak")
	_ = sec.For("bob@example.com").Put(context.Background(), providerSecretName(p.UID, providerKeyPart), []byte("bobs-key-must-not-leak-000"))

	got := h.srv.providerAccessFor(context.Background(), p, paOwner)
	if got.State != modelAccessNotConfigured {
		t.Fatalf("alice's own row is empty despite the operator's and bob's being stored: state = %q, want not_configured", got.State)
	}
}

// TestProviderAccess_AnthropicSubscription walks not_configured -> live ->
// expiring (harnessTokenAging, the SAME conservative age heuristic
// SetupHarness.Aging already uses) for a per-person Claude sign-in.
func TestProviderAccess_AnthropicSubscription(t *testing.T) {
	h, sec := newSecretsHarness(t)
	now := time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)
	h.srv.cfg.Now = func() time.Time { return now }
	p := paKeyProvider("claude-sub", types.ModelProviderAnthropicSubscription)

	got := h.srv.providerAccessFor(context.Background(), p, paOwner)
	if got.State != modelAccessNotConfigured || got.Action != providerAccessSignInClaude {
		t.Fatalf("no sign-in captured: got %+v, want state=not_configured action=%q", got, providerAccessSignInClaude)
	}

	fresh, _ := marshalManagedCredBlob(t, "sk-ant-oat-alice", now.Add(-time.Hour))
	_ = sec.For(paOwner).Put(context.Background(), providerSecretName(p.UID, providerOAuthPart), fresh)
	got = h.srv.providerAccessFor(context.Background(), p, paOwner)
	if got.State != modelAccessLive {
		t.Fatalf("freshly captured: state = %q, want live", got.State)
	}

	aged, capturedAt := marshalManagedCredBlob(t, "sk-ant-oat-alice", now.Add(-harnessTokenAging-time.Hour))
	_ = sec.For(paOwner).Put(context.Background(), providerSecretName(p.UID, providerOAuthPart), aged)
	got = h.srv.providerAccessFor(context.Background(), p, paOwner)
	if got.State != modelAccessExpiring {
		t.Fatalf("captured at %s (past harnessTokenAging): state = %q, want expiring", capturedAt, got.State)
	}
	if got.Action != providerAccessClaudeAgingAction {
		t.Errorf("expiring action = %q, want the 5.4 aging copy verbatim", got.Action)
	}
}

// TestProviderAccess_BedrockSSO reuses awsSSOCredentialState's own vocabulary
// (not_configured / live / expired_signin) over a provider-scoped read, then
// the provider's own account/role pin turning a live, renewable session for
// the WRONG identity into expired_signin — the same substitution
// setupModelAccess's roster-pin arm makes, moved onto the provider record.
func TestProviderAccess_BedrockSSO(t *testing.T) {
	h, sec := newSecretsHarness(t)
	now := awsSSOTestFixedNow
	h.srv.cfg.Now = func() time.Time { return now }
	p := paSSOProvider("bedrock-prod")

	got := h.srv.providerAccessFor(context.Background(), p, paOwner)
	if got.State != modelAccessNotConfigured {
		t.Fatalf("no session captured: state = %q, want not_configured", got.State)
	}

	_ = sec.For(paOwner).Put(context.Background(), providerSecretName(p.UID, providerSSOPart),
		brBlob("sso-tok-alice", "123456789012", "BedrockUser", now.Add(48*time.Hour)))
	got = h.srv.providerAccessFor(context.Background(), p, paOwner)
	if got.State != modelAccessLive {
		t.Fatalf("session matches the provider's pin: state = %q, want live", got.State)
	}

	_ = sec.For(paOwner).Put(context.Background(), providerSecretName(p.UID, providerSSOPart),
		brBlob("sso-tok-alice", "999999999999", "Wrong", now.Add(48*time.Hour)))
	got = h.srv.providerAccessFor(context.Background(), p, paOwner)
	if got.State != modelAccessExpiredSignin {
		t.Fatalf("session names an account/role the provider does not pin: state = %q, want expired_signin", got.State)
	}
	if got.Deadline != "" {
		t.Errorf("a pin-contradicted session carries no deadline (nothing lapses; the identity is simply wrong), got %q", got.Deadline)
	}
	wantAction := "Your stored AWS session is for account 999999999999 / role Wrong; this row now allows 123456789012 / BedrockUser — sign in again."
	if got.Action != wantAction {
		t.Errorf("pin-mismatch action = %q, want %q", got.Action, wantAction)
	}
}

// TestProviderAccess_MechanismPrincipalIsNotApplicable: the shared admin
// bearer token under OIDC has no credential of its own for ANY provider kind
// — the same "not a person" refusal credentialOwner/perPersonSubscriptionPosture
// give the write doors, read here instead of refused.
func TestProviderAccess_MechanismPrincipalIsNotApplicable(t *testing.T) {
	h, _ := newSecretsHarness(t)
	h.srv.cfg.OIDC = &oidc.Authenticator{}
	for _, p := range []types.ModelProvider{
		paKeyProvider("gw", types.ModelProviderAnthropicAPIKey),
		paKeyProvider("claude-sub", types.ModelProviderAnthropicSubscription),
		paSSOProvider("bedrock-prod"),
	} {
		got := h.srv.providerAccessFor(context.Background(), p, adminTokenPrincipal)
		if got.State != modelAccessNotApplicable {
			t.Errorf("%s: mechanism principal state = %q, want not_applicable", p.Kind, got.State)
		}
		if got.Action != "" || got.Deadline != "" {
			t.Errorf("%s: not_applicable carries no action/deadline, got action=%q deadline=%q", p.Kind, got.Action, got.Deadline)
		}
	}
	// An empty owner (no store/subject at all) is the same "nobody" case,
	// OIDC or not.
	h.srv.cfg.OIDC = nil
	got := h.srv.providerAccessFor(context.Background(), paKeyProvider("gw", types.ModelProviderAnthropicAPIKey), "")
	if got.State != modelAccessNotApplicable {
		t.Errorf("empty owner state = %q, want not_applicable", got.State)
	}
}

// TestSetupProviderAccess_OneRowPerGrantedProvider is the wiring
// setupProviderAccess itself owns: nil with no providers, else exactly one row
// per SetupModelProvider passed in, graded against the matching full record.
func TestSetupProviderAccess_OneRowPerGrantedProvider(t *testing.T) {
	h, sec := newSecretsHarness(t)
	if got := h.srv.setupProviderAccess(context.Background(), types.SiteConfig{}, nil, paOwner); got != nil {
		t.Fatalf("no providers: got %+v, want nil", got)
	}

	p1 := paKeyProvider("gw", types.ModelProviderAnthropicAPIKey)
	p2 := paKeyProvider("claude-sub", types.ModelProviderAnthropicSubscription)
	sc := types.SiteConfig{ModelProviders: providerBlock(p1, p2)}
	granted := []SetupModelProvider{{ID: p1.ID}, {ID: p2.ID}}
	_ = sec.For(paOwner).Put(context.Background(), providerSecretName(p1.UID, providerKeyPart), []byte("a-real-key-value-0123456789"))

	got := h.srv.setupProviderAccess(context.Background(), sc, granted, paOwner)
	if len(got) != 2 {
		t.Fatalf("len(provider_access) = %d, want 2", len(got))
	}
	byID := map[string]SetupProviderAccess{}
	for _, row := range got {
		byID[row.Provider] = row
	}
	if byID[p1.ID].State != modelAccessLive {
		t.Errorf("gw: state = %q, want live", byID[p1.ID].State)
	}
	if byID[p2.ID].State != modelAccessNotConfigured {
		t.Errorf("claude-sub: state = %q, want not_configured", byID[p2.ID].State)
	}

	// A SetupModelProvider naming an id the block no longer carries (a stale
	// caller-held list against a store that changed under it) is skipped, not
	// a zero-value crash row.
	stale := []SetupModelProvider{{ID: "gone"}}
	if got := h.srv.setupProviderAccess(context.Background(), sc, stale, paOwner); len(got) != 0 {
		t.Errorf("a provider id no longer in the block: got %+v, want an empty slice", got)
	}
}

// marshalManagedCredBlob is subBlob (provider_subscription_test.go) plus an
// explicit CapturedAt, which the aging grade needs and subBlob leaves zero.
func marshalManagedCredBlob(t *testing.T, tok string, capturedAt time.Time) ([]byte, time.Time) {
	t.Helper()
	b, err := json.Marshal(managedCredBlob{Token: tok, CapturedAt: capturedAt})
	if err != nil {
		t.Fatalf("marshal managedCredBlob: %v", err)
	}
	return b, capturedAt
}
