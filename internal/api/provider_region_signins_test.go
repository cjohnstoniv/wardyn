// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// endRecorder is the approvals fake, keeping the actor and reason each ExpireOne call passed.
type endRecorder struct {
	*fakeApprovals
	ends map[uuid.UUID][2]string
}

func (a endRecorder) ExpireOne(ctx context.Context, id uuid.UUID, actor, reason string) error {
	a.ends[id] = [2]string{actor, reason}
	return a.fakeApprovals.ExpireOne(ctx, id, actor, reason)
}

// raiseHook runs after once a request is raised: a provider write that landed between a
// resolve's read of the provider and its raise, whose own list ran before the request existed.
type raiseHook struct {
	endRecorder
	after func()
}

func (a raiseHook) Request(ctx context.Context, req types.ApprovalRequest) (types.ApprovalRequest, error) {
	ap, err := a.endRecorder.Request(ctx, req)
	a.after()
	return ap, err
}

// grantSiteStore is the site-config fake that also answers each run's grants.
type grantSiteStore struct {
	*fakeSiteConfigStore
	grants map[uuid.UUID][]types.CredentialGrant
}

func (s grantSiteStore) ListGrantsByRun(_ context.Context, runID uuid.UUID) ([]types.CredentialGrant, error) {
	return s.grants[runID], nil
}

// providerWriteDoors are the two doors that save model providers, each with its body's shape.
var providerWriteDoors = []struct {
	path string
	wrap func(string) string
}{
	{"/api/v1/model-providers", func(b string) string { return b }},
	{"/api/v1/site-config", func(b string) string { return `{"model_providers":` + b + `}` }},
}

// TestProviderWriteEndsPendingAWSSignIns: changing a bedrock_sso provider's region, pinned
// account or pinned role, deleting it or changing its kind ends, in the save, every pending AWS
// sign-in request raised for it, at both write doors, as the system's and with the reason. A
// request for another provider, one naming no provider, one already answered, and every request
// at a save that does none of these (or only removes the pin, after which any account serves)
// are left as they are.
func TestProviderWriteEndsPendingAWSSignIns(t *testing.T) {
	for _, d := range providerWriteDoors {
		for _, tc := range []struct {
			name   string
			edit   func(*types.ModelProviders) // Providers[1] is bedrock-prod
			reason string                      // "" = nothing is ended
		}{
			{"a region edit", func(b *types.ModelProviders) { b.Providers[1].Bedrock.Region = "us-west-2" }, "provider_sign_in_changed"},
			{"an edit of the pinned account", func(b *types.ModelProviders) { b.Providers[1].Bedrock.SSOAccountID = "210987654321" }, "provider_sign_in_changed"},
			{"an edit of the pinned role", func(b *types.ModelProviders) { b.Providers[1].Bedrock.SSORoleName = "OtherRole" }, "provider_sign_in_changed"},
			{"the provider deleted", func(b *types.ModelProviders) { b.Providers = b.Providers[:1] }, "provider_removed"},
			{"a kind change", func(b *types.ModelProviders) { b.Providers[1] = keyProvider("bedrock-prod", "claude-code") }, "provider_removed"},
			{"an identical save", func(*types.ModelProviders) {}, ""},
			{"the pin removed", func(b *types.ModelProviders) {
				b.Providers[1].Bedrock.SSOAccountID, b.Providers[1].Bedrock.SSORoleName = "", ""
			}, ""},
		} {
			t.Run(d.path+"/"+tc.name, func(t *testing.T) {
				site := credentialSite(endpointProvider(), ssoProvider())
				sso, _ := modelProviderByID(site.ModelProviders, "bedrock-prod")
				srv := New(baseTestConfig(newHarness(t), grantSiteStore{&fakeSiteConfigStore{cfg: site}, nil}))
				approvals := endRecorder{newFakeApprovals(), map[uuid.UUID][2]string{}}
				srv.cfg.Approvals = approvals
				ctx := context.Background()
				raise := func(scope awsSSOReauthScopeBody) uuid.UUID {
					t.Helper()
					scope.Mechanism, scope.Owner = "sso", "alice@example.com"
					ap, err := approvals.Request(ctx, types.ApprovalRequest{RunID: uuid.New(), Kind: types.ApprovalCredentialReauth,
						RequestedScope: mustJSON(scope)})
					if err != nil {
						t.Fatal(err)
					}
					return ap.ID
				}
				own := awsSSOReauthScopeBody{Provider: "bedrock-prod", ProviderUID: sso.UID}
				open, answered := raise(own), raise(own)
				other := raise(awsSSOReauthScopeBody{Provider: "another", ProviderUID: "uid-another-provider"})
				bare := raise(awsSSOReauthScopeBody{}) // a CLI or roster request names no provider
				if _, err := approvals.Decide(ctx, answered, types.ActorHuman, types.ApprovalDecision{
					State: types.ApprovalApproved, DecidedBy: "alice@example.com"}); err != nil {
					t.Fatal(err)
				}

				body := normalizeModelProviders(providerBlock(endpointProvider(), ssoProvider()))
				tc.edit(body)
				raw, _ := json.Marshal(body)
				if w := do(t, srv, http.MethodPut, d.path, adminToken, d.wrap(string(raw))); w.Code != http.StatusOK {
					t.Fatalf("PUT = %d; body=%s", w.Code, w.Body.String())
				}

				want, ends := types.ApprovalPending, map[uuid.UUID][2]string{}
				if tc.reason != "" {
					want, ends[open] = types.ApprovalExpired, [2]string{"wardynd", tc.reason}
				}
				for id, state := range map[uuid.UUID]types.ApprovalState{
					open: want, other: types.ApprovalPending, bare: types.ApprovalPending, answered: types.ApprovalApproved,
				} {
					if got, _ := approvals.Get(ctx, id); got.State != state {
						t.Errorf("request %s is %s after the save, want %s", id, got.State, state)
					}
				}
				if len(approvals.ends) != len(ends) || approvals.ends[open] != ends[open] {
					t.Errorf("ended (actor, reason by request) = %v, want %v", approvals.ends, ends)
				}
			})
		}
	}
}

// TestProviderWriteKeepsTheRequestOfARunOnTheNewPin: pinning an unpinned provider, or pinning it
// to another account or role, ends a pending sign-in request only when its run's grant holds
// another account or role than the new pin, at both write doors. A run dispatched for the pair
// pinned now is served once its owner signs in, so its request stays pending and a sign-in for
// that provider still answers it.
func TestProviderWriteKeepsTheRequestOfARunOnTheNewPin(t *testing.T) {
	for _, d := range providerWriteDoors {
		for _, tc := range []struct {
			name   string
			before func(*types.BedrockSettings) // the provider as stored before the save, which pins 123456789012/BedrockUser
		}{
			{"a first pin", func(b *types.BedrockSettings) { b.SSOAccountID, b.SSORoleName = "", "" }},
			{"a pin edit", func(b *types.BedrockSettings) { b.SSOAccountID = "210987654321" }},
		} {
			t.Run(d.path+"/"+tc.name, func(t *testing.T) {
				old := ssoProvider()
				tc.before(old.Bedrock)
				site := credentialSite(endpointProvider(), old)
				sso, _ := modelProviderByID(site.ModelProviders, "bedrock-prod")
				st := grantSiteStore{&fakeSiteConfigStore{cfg: site}, map[uuid.UUID][]types.CredentialGrant{}}
				srv := New(baseTestConfig(newHarness(t), st))
				approvals := endRecorder{newFakeApprovals(), map[uuid.UUID][2]string{}}
				srv.cfg.Approvals = approvals
				ctx := context.Background()
				scope := chosenProvider{provider: sso, owner: "alice@example.com"}.awsScope()
				raise := func(account, role string) uuid.UUID {
					t.Helper()
					runID := uuid.New()
					st.grants[runID] = []types.CredentialGrant{{RunID: runID, Spec: types.GrantSpec{Kind: types.GrantAPIKey,
						Scope: mustJSON(map[string]any{"snapshot": awsSSOScopeSnapshot{
							OwnerSubject: "alice@example.com", CredentialSource: awsSSOCredentialSourceLabel(scope),
							Mechanism: string(types.ModelProviderBedrockSSO), SSOAccountID: account, SSORoleName: role,
							Region: "us-east-1", ProviderUID: sso.UID,
						}})}}}
					ap, err := approvals.Request(ctx, types.ApprovalRequest{RunID: runID, Kind: types.ApprovalCredentialReauth,
						RequestedScope: mustJSON(awsSSOReauthScopeBody{Mechanism: "sso", CredentialSource: awsSSOCredentialSourceLabel(scope),
							Owner: "alice@example.com", Provider: "bedrock-prod", ProviderUID: sso.UID})})
					if err != nil {
						t.Fatal(err)
					}
					return ap.ID
				}
				onPin, elsewhere := raise("123456789012", "BedrockUser"), raise("210987654321", "BedrockUser")

				raw, _ := json.Marshal(normalizeModelProviders(providerBlock(endpointProvider(), ssoProvider())))
				if w := do(t, srv, http.MethodPut, d.path, adminToken, d.wrap(string(raw))); w.Code != http.StatusOK {
					t.Fatalf("PUT = %d; body=%s", w.Code, w.Body.String())
				}

				kept, _ := approvals.Get(ctx, onPin)
				if kept.State != types.ApprovalPending {
					t.Errorf("the request of the run on the new pin is %s after the save, want PENDING", kept.State)
				}
				if !reauthResolvableBy(kept, scope, types.AgentRun{CreatedAt: time.Now().Add(time.Minute)}) {
					t.Errorf("a later sign-in for bedrock-prod does not answer the request of the run on the new pin")
				}
				if got, _ := approvals.Get(ctx, elsewhere); got.State != types.ApprovalExpired {
					t.Errorf("the request of the run on another account is %s after the save, want EXPIRED", got.State)
				}
				if want := map[uuid.UUID][2]string{elsewhere: {"wardynd", "provider_sign_in_changed"}}; len(approvals.ends) != 1 || approvals.ends[elsewhere] != want[elsewhere] {
					t.Errorf("ended (actor, reason by request) = %v, want %v", approvals.ends, want)
				}
			})
		}
	}
}

// TestResolveAWSSSOInjection_APinOnTheRunsOwnPairLeavesTheRaisedRequestOpen: a write that pins
// the provider, while a resolve raises its request, to the account and role the run's grant holds
// leaves the request pending (423), and the run is served once a session for that pair is stored.
func TestResolveAWSSSOInjection_APinOnTheRunsOwnPairLeavesTheRaisedRequestOpen(t *testing.T) {
	f := newReauthFixture(t, nil) // no session stored yet
	unpinned := reauthProvider()
	unpinned.Bedrock.SSOAccountID, unpinned.Bedrock.SSORoleName = "", ""
	f.st.site = reauthProviderSite(unpinned)
	approvals := raiseHook{endRecorder{f.st.approvals, map[uuid.UUID][2]string{}}, func() { f.st.site = reauthProviderSite(reauthProvider()) }}
	f.srv.cfg.Approvals = approvals

	if w := f.resolve(t); w.Code != http.StatusLocked {
		t.Fatalf("resolve = %d %s, want 423 (the request stays open)", w.Code, w.Body.String())
	}
	if len(approvals.ends) != 0 {
		t.Errorf("ended (actor, reason by request) = %v, want none", approvals.ends)
	}
	f.putBlob(t, "alice@example.com", liveSSOBlob())
	if w := f.resolve(t); w.Code != http.StatusOK {
		t.Errorf("resolve after the sign-in = %d %s, want 200", w.Code, w.Body.String())
	}
}

// TestResolveAWSSSOInjection_AProviderWriteRacingTheRaiseEndsTheRequest: a resolve that read
// its provider before a write moved it, changed its pin or removed it raises its sign-in request
// after that write listed the pending ones. The resolve re-reads the provider once the request is raised, ends
// the request and refuses, so no later sign-in can answer a request its run cannot use.
func TestResolveAWSSSOInjection_AProviderWriteRacingTheRaiseEndsTheRequest(t *testing.T) {
	for _, tc := range []struct {
		name   string
		site   func() types.SiteConfig // what the write saved
		reason string
	}{
		{"moved to another region", func() types.SiteConfig {
			p := reauthProvider()
			p.Bedrock.Region = "us-west-2"
			return reauthProviderSite(p)
		}, "provider_sign_in_changed"},
		{"its pinned account changed", func() types.SiteConfig {
			p := reauthProvider()
			p.Bedrock.SSOAccountID = "444455556666"
			return reauthProviderSite(p)
		}, "provider_sign_in_changed"},
		{"deleted", func() types.SiteConfig { return types.SiteConfig{ModelProviders: &types.ModelProviders{}} }, "provider_removed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReauthFixture(t, nil) // no session stored: the write's purge removed it
			approvals := raiseHook{endRecorder{f.st.approvals, map[uuid.UUID][2]string{}}, func() { f.st.site = tc.site() }}
			f.srv.cfg.Approvals = approvals

			w := f.resolve(t)
			var body errorBody
			_ = json.Unmarshal(w.Body.Bytes(), &body)
			if w.Code != http.StatusForbidden || body.Reason != reasonScopeChanged || body.Error != credentialReauthScopeChangedRefusal {
				t.Fatalf("resolve = %d %s, want 403 %s", w.Code, w.Body.String(), reasonScopeChanged)
			}
			if len(approvals.ends) != 1 {
				t.Fatalf("ended (actor, reason by request) = %v, want the one request raised", approvals.ends)
			}
			for id, got := range approvals.ends {
				if got != [2]string{"wardynd", tc.reason} {
					t.Errorf("ended as %v, want wardynd and %s", got, tc.reason)
				}
				if ap, _ := approvals.Get(context.Background(), id); ap.Kind != types.ApprovalCredentialReauth || ap.State != types.ApprovalExpired {
					t.Errorf("the raised request is %s %s, want an expired sign-in request", ap.Kind, ap.State)
				}
			}
		})
	}
}

// TestResolveAWSSSOInjection_ASaveAndTheResolveEndingOneRequestCountItOnce: a save that lands
// while a resolve raises its request ends it, and the resolve's own re-read ends it again. Only
// the end that moved the row counts, so wardyn_credential_reauth_total{outcome="expired"} is 1.
func TestResolveAWSSSOInjection_ASaveAndTheResolveEndingOneRequestCountItOnce(t *testing.T) {
	f := newReauthFixture(t, nil) // no session stored: the write's purge removed it
	f.srv.cfg.Approvals = raiseHook{endRecorder{f.st.approvals, map[uuid.UUID][2]string{}}, func() {
		before := f.st.site.ModelProviders
		p := reauthProvider()
		p.Bedrock.Region = "us-west-2"
		f.st.site = reauthProviderSite(p)
		f.srv.endUnanswerableSignIns(context.Background(), before, f.st.site.ModelProviders)
	}}

	w := f.resolve(t)
	var body errorBody
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if w.Code != http.StatusForbidden || body.Reason != reasonScopeChanged {
		t.Fatalf("resolve = %d %s, want 403 %s (the resolve's own end ran too)", w.Code, w.Body.String(), reasonScopeChanged)
	}
	if got := reauthCount(t, f.srv, "expired"); got != "1" {
		t.Errorf(`wardyn_credential_reauth_total{outcome="expired"} = %s, want 1`, got)
	}
}
