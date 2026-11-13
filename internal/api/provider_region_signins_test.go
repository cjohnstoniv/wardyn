// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

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

// TestProviderWriteEndsPendingAWSSignIns: changing a bedrock_sso provider's region, pinned
// account or pinned role, deleting it or changing its kind ends, in the save, every pending AWS
// sign-in request raised for it, at both write doors, as the system's and with the reason. A
// request for another provider, one naming no provider, one already answered, and every request
// at a save that does none of these (or only removes the pin, after which any account serves)
// are left as they are.
func TestProviderWriteEndsPendingAWSSignIns(t *testing.T) {
	doors := []struct {
		path string
		wrap func(string) string
	}{
		{"/api/v1/model-providers", func(b string) string { return b }},
		{"/api/v1/site-config", func(b string) string { return `{"model_providers":` + b + `}` }},
	}
	for _, d := range doors {
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
				srv, _ := newSiteConfigHarness(t, &fakeSiteConfigStore{cfg: site})
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
