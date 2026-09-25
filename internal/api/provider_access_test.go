// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// provider_access_test.go: MP-12's per-provider grading (SetupProviderAccess),
// generalising SetupModelAccess's single hardcoded AWS-SSO lane to every kind
// a person may be granted. The grading tests read through providerAccessFor/
// setupProviderAccess — the functions GET /setup/status calls — never the lower
// helpers (awsSSOCredentialState, ownSecret) directly;
// TestSetupStatusProviderAccess reads it off the endpoint itself.

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
	now := time.Now().UTC()
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

	// The pin matches, but an admin has since pointed the provider at another
	// access portal (rule 8 does not purge on that): dispatch refuses the
	// session (mpBRPortal), so this row must not read live beside it.
	_ = sec.For(paOwner).Put(context.Background(), providerSecretName(p.UID, providerSSOPart),
		brBlob("sso-tok-alice", "123456789012", "BedrockUser", now.Add(48*time.Hour)))
	moved, b := p, *p.Bedrock
	b.SSOStartURL = "https://other.awsapps.com/start"
	moved.Bedrock = &b
	got = h.srv.providerAccessFor(context.Background(), moved, paOwner)
	if got.State != modelAccessExpiredSignin || got.Deadline != "" || got.Action != providerAccessPortalAction {
		t.Fatalf("session from another access portal: got %+v, want expired_signin, no deadline, action %q", got, providerAccessPortalAction)
	}
	_, d, err := h.srv.providerBedrockRefusal(context.Background(), moved, "claude-code", paOwner, false)
	if err != nil || !strings.Contains(d.msg, mpBRPortal) || !d.credential {
		t.Fatalf("dispatch on the same credential: refusal=%+v err=%v, want the access-portal refusal, a credential one", d, err)
	}
	// Case and a trailing slash are the same portal, as dispatch reads it.
	b.SSOStartURL = "HTTPS://acme.awsapps.com/start/"
	if got = h.srv.providerAccessFor(context.Background(), moved, paOwner); got.State != modelAccessLive {
		t.Errorf("same portal spelled differently: state = %q, want live", got.State)
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

// TestSetupProviderAccess_DisabledProviderSkipped: dispatch refuses a
// disabled provider outright (mp.Disabled, provider_subscription.go/
// provider_bedrock.go), so grading one would tell the caller connecting (or
// reconnecting) a credential helps when it never does. A disabled provider
// holding a live key must not grade "ok", and one with no key must not grade
// a "connect it" warn — it gets no row at all.
func TestSetupProviderAccess_DisabledProviderSkipped(t *testing.T) {
	h, sec := newSecretsHarness(t)
	p1 := paKeyProvider("gw", types.ModelProviderAnthropicAPIKey)
	p2 := paKeyProvider("gw-off", types.ModelProviderAnthropicAPIKey)
	sc := types.SiteConfig{ModelProviders: providerBlock(p1, p2)}
	granted := []SetupModelProvider{{ID: p1.ID}, {ID: p2.ID, Disabled: true}}
	// The disabled provider even holds a stored key, to prove Disabled alone
	// suppresses the row rather than the credential being absent.
	_ = sec.For(paOwner).Put(context.Background(), providerSecretName(p2.UID, providerKeyPart), []byte("a-real-key-value-0123456789"))

	got := h.srv.setupProviderAccess(context.Background(), sc, granted, paOwner)
	if len(got) != 1 || got[0].Provider != p1.ID {
		t.Fatalf("got %+v, want exactly one row for gw (gw-off is disabled)", got)
	}
}

// TestProviderAccessCheck_RowPerState is the checklist half of MP-12: each
// provider_access state in the checklist's grammar, the row's fix being the
// provider_access action verbatim so the two never disagree.
func TestProviderAccessCheck_RowPerState(t *testing.T) {
	p := types.ModelProvider{ID: "corp-gateway", Name: "Corp gateway", Kind: types.ModelProviderCustomEndpoint}
	for _, tc := range []struct {
		state, action, wantStatus, wantDetail, wantFix string
	}{
		{modelAccessLive, "", "ok", "Your token for this provider is connected; runs on it use your own credential.", ""},
		{modelAccessExpiring, "Sign in again before x", "warn", "Your token for this provider may stop working soon; runs on it fail once it does.", "Sign in again before x"},
		{modelAccessExpiredSignin, "Sign in to AWS", "warn", "Your token for this provider can no longer be used, so runs on it are refused until you sign in again.", "Sign in to AWS"},
		{modelAccessNotConfigured, providerAccessAddTokenAction, "warn", "You have not connected your token for this provider yet, so runs on it are refused until you do.", providerAccessAddTokenAction},
		{modelAccessNotApplicable, "", "info", providerAccessMechanismDetail, bedrockMechanismFix},
	} {
		got := providerAccessCheck(p, SetupProviderAccess{Provider: p.ID, State: tc.state, Action: tc.action}, true)
		want := SetupCheck{ID: "llm_provider:corp-gateway", Label: "LLM access: Corp gateway",
			Status: tc.wantStatus, Detail: tc.wantDetail, Fix: tc.wantFix}
		if got != want {
			t.Errorf("%s:\n got  %+v\n want %+v", tc.state, got, want)
		}
		assertSetupCheckBlocking(t, got)
	}
}

// TestProviderAccessCheck_NotDefaultNotConfiguredIsInfo: 5.4's "no alarm,
// because it isn't the default" rule. A granted provider that is not any of
// the caller's harness defaults grades not_configured as info, not warn — the
// caller connected the provider they actually use, and nothing routes to this
// one unless they choose it themselves. isDefault=true (or any OTHER state)
// keeps the ordinary warn/ok grading unchanged.
func TestProviderAccessCheck_NotDefaultNotConfiguredIsInfo(t *testing.T) {
	p := types.ModelProvider{ID: "corp-gateway", Name: "Corp gateway", Kind: types.ModelProviderCustomEndpoint}
	a := SetupProviderAccess{Provider: p.ID, State: modelAccessNotConfigured, Action: providerAccessAddTokenAction}

	got := providerAccessCheck(p, a, false)
	if got.Status != "info" {
		t.Errorf("non-default, not configured: status = %q, want info", got.Status)
	}
	if got.Detail != fmt.Sprintf(providerAccessMissingDetail, "token") || got.Fix != providerAccessAddTokenAction {
		t.Errorf("non-default, not configured keeps its own detail/fix: got %+v", got)
	}
	assertSetupCheckBlocking(t, got)

	if got := providerAccessCheck(p, a, true); got.Status != "warn" {
		t.Errorf("default, not configured: status = %q, want warn (unchanged)", got.Status)
	}
	live := SetupProviderAccess{Provider: p.ID, State: modelAccessLive}
	if got := providerAccessCheck(p, live, false); got.Status != "ok" {
		t.Errorf("non-default but live: status = %q, want ok (isDefault only touches not_configured)", got.Status)
	}
}

// TestLLMProviderCheck_ProviderArm: with no legacy signal, a provider block
// granting this caller providers is not "no model/harness provider
// configured" — the LLM access row answers from provider_access instead.
func TestLLMProviderCheck_ProviderArm(t *testing.T) {
	row := func(id, state string) SetupProviderAccess { return SetupProviderAccess{Provider: id, State: state} }
	missing := llmProviderCheck("", SetupBedrock{}, []SetupProviderAccess{row("a", modelAccessNotConfigured), row("b", modelAccessExpiredSignin)})
	if missing.Status != "warn" || missing.Detail != providerAccessLLMMissingDetail || missing.Fix != providerAccessLLMMissingFix {
		t.Errorf("no usable provider: got %+v, want the warn arm", missing)
	}
	live := llmProviderCheck("", SetupBedrock{}, []SetupProviderAccess{row("a", modelAccessNotConfigured), row("b", modelAccessLive), row("c", modelAccessExpiring)})
	if live.Status != "ok" || live.Detail != "Model providers you can run on now: b, c." || live.Fix != "" {
		t.Errorf("b live, c expiring: got %+v, want ok naming b and c", live)
	}
	mech := llmProviderCheck("", SetupBedrock{}, []SetupProviderAccess{row("a", modelAccessNotApplicable)})
	if mech.Status != "info" || mech.Detail != providerAccessMechanismDetail {
		t.Errorf("mechanism principal: got %+v, want info", mech)
	}
	// The legacy winning signal still outranks the provider arm (until MP-4).
	if got := llmProviderCheck("legacy lane", SetupBedrock{}, []SetupProviderAccess{row("a", modelAccessNotConfigured)}); got.Status != "ok" || got.Detail != "legacy lane" {
		t.Errorf("legacy signal: got %+v, want it unchanged", got)
	}
	for _, chk := range []SetupCheck{missing, live, mech} {
		assertSetupCheckBlocking(t, chk)
	}
}

// TestSetupStatusProviderAccess reads provider_access off GET /setup/status
// itself: graded against the REQUESTING member's own namespace (the handler's
// owner derivation), on the wire, and through the member redaction — plus the
// per-provider checklist row an admin sees.
func TestSetupStatusProviderAccess(t *testing.T) {
	p := paKeyProvider("anthropic", types.ModelProviderAnthropicAPIKey)
	site := types.SiteConfig{ModelProviders: providerBlock(p),
		AgentProviders: agentBlock(types.AgentProvider{ID: "claude-code", Mechanism: types.AgentMechanismAnthropicAPIKey})}
	srv := modelProvidersStatusSrv(t, site, &capStore{})
	sec := srv.cfg.Secrets.(*memSecrets)
	status := func(t *testing.T, sub string) SetupStatus {
		t.Helper()
		w := doSSO(t, srv, http.MethodGet, "/api/v1/setup/status", ssoSession(t, sub, sub+"@corp.example", oidc.RoleUser), "")
		if w.Code != http.StatusOK {
			t.Fatalf("GET /setup/status = %d; body=%s", w.Code, w.Body.String())
		}
		var st SetupStatus
		if err := json.Unmarshal(w.Body.Bytes(), &st); err != nil {
			t.Fatal(err)
		}
		return st
	}
	_ = sec.For("sub-alice").Put(context.Background(), providerSecretName(p.UID, providerKeyPart), []byte("alices-own-key-0123456789"))

	if got := status(t, "sub-alice").ProviderAccess; len(got) != 1 || got[0] != (SetupProviderAccess{Provider: "anthropic", State: modelAccessLive}) {
		t.Errorf("alice, own key stored: provider_access = %+v, want [anthropic live]", got)
	}
	want := SetupProviderAccess{Provider: "anthropic", State: modelAccessNotConfigured, Action: providerAccessAddKeyAction}
	if got := status(t, "sub-bob").ProviderAccess; len(got) != 1 || got[0] != want {
		t.Errorf("bob, only alice's key stored: provider_access = %+v, want [%+v]", got, want)
	}

	w := do(t, srv, http.MethodGet, "/api/v1/setup/status", adminToken, "")
	var admin SetupStatus
	if err := json.Unmarshal(w.Body.Bytes(), &admin); err != nil {
		t.Fatal(err)
	}
	var row *SetupCheck
	for i := range admin.Checks {
		if admin.Checks[i].ID == "llm_provider:anthropic" {
			row = &admin.Checks[i]
		}
	}
	if row == nil || row.Status != "info" || row.Detail != providerAccessMechanismDetail {
		t.Errorf("admin token's checklist row for anthropic = %+v, want the not_applicable info row", row)
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
