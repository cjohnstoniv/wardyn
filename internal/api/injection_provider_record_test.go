// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The injection sink trusts the provider record, never the grant (#531): a
// provider key is re-checked against the record, by UID, on every resolve.

// TestProviderKeySink_ReadsTheRecordByUID: dispatch authors the owner's grant
// on the provider as it stood at launch; the record then changes. Only an
// unchanged record resolves, and its answer expires after 15 minutes, so the
// proxy (5 minutes early) asks again about every 10 minutes: one mint and one
// secret.read row each time, the cadence docs/AUDIT-ACTIONS.md names.
func TestProviderKeySink_ReadsTheRecordByUID(t *testing.T) {
	endpoint := func() types.ModelProvider {
		p := mpKeyProvider("corp", "uid-e", types.ModelProviderCustomEndpoint,
			types.ProviderHarness{Harness: "claude-code", Path: "/anthropic"})
		p.BaseURL = "https://llm.corp.example"
		p.Auth = &types.ProviderAuth{Header: "api-key", Format: "%s"}
		return p
	}
	for _, tc := range []struct {
		name   string
		change func(st *bearerGuardStore, p *types.ModelProvider)
		want   int
		reason string
	}{
		{"unchanged", func(*bearerGuardStore, *types.ModelProvider) {}, http.StatusOK, ""},
		{"re-pointed to another host", func(_ *bearerGuardStore, p *types.ModelProvider) {
			p.BaseURL = "https://llm-new.corp.example"
		}, http.StatusForbidden, "provider_changed"},
		{"its auth header changed", func(_ *bearerGuardStore, p *types.ModelProvider) {
			p.Auth.Header = "Authorization"
		}, http.StatusForbidden, "provider_changed"},
		{"its auth format changed", func(_ *bearerGuardStore, p *types.ModelProvider) {
			p.Auth.Format = "Bearer %s"
		}, http.StatusForbidden, "provider_changed"},
		{"turned off", func(_ *bearerGuardStore, p *types.ModelProvider) { p.Disabled = true }, http.StatusForbidden, "provider_changed"},
		{"no longer serves the agent", func(_ *bearerGuardStore, p *types.ModelProvider) {
			p.Harnesses = []types.ProviderHarness{{Harness: "codex-cli"}}
		}, http.StatusForbidden, "provider_changed"},
		{"deleted and re-added under the same id", func(_ *bearerGuardStore, p *types.ModelProvider) {
			p.UID = "uid-other"
		}, http.StatusForbidden, "provider_changed"},
		{"removed", func(st *bearerGuardStore, _ *types.ModelProvider) {
			st.site.ModelProviders = providerBlock()
		}, http.StatusForbidden, "provider_changed"},
		{"the block cannot be read", func(st *bearerGuardStore, _ *types.ModelProvider) {
			st.siteErr = errors.New("db down")
		}, http.StatusServiceUnavailable, "providers_unreadable"},
		{"the run cannot be read", func(st *bearerGuardStore, _ *types.ModelProvider) {
			st.failRunFromCall = st.runCalls + 2 // the /internal liveness gate's read passes; the sink's fails
			st.runErr = errors.New("db down")
		}, http.StatusServiceUnavailable, "run_unreadable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := endpoint()
			st := &bearerGuardStore{
				run:  types.AgentRun{ID: uuid.New(), Agent: "claude-code", CreatedBy: mpOwner, ModelProviderID: p.ID},
				site: types.SiteConfig{ModelProviders: providerBlock(p)},
			}
			h := mpHarness(t, st, p)
			policy := types.RunPolicySpec{}
			_, _, grants, ok := mpDispatch(t, h, st, &policy, nil)
			if !ok || len(grants) != 1 {
				t.Fatalf("dispatch ok=%v grants=%d, want one authored grant", ok, len(grants))
			}
			rule, err := injectionRuleFromScope(grants[0].Spec.Scope)
			if err != nil {
				t.Fatal(err)
			}
			h.broker.minted = broker.Minted{Kind: types.GrantAPIKey, JTI: "j1", Injection: &rule}

			changed := endpoint()
			tc.change(st, &changed)
			if st.site.ModelProviders != nil && len(st.site.ModelProviders.Providers) == 1 {
				st.site.ModelProviders = providerBlock(changed)
			}
			before := time.Now()
			rr := do(t, h.srv, http.MethodGet, "/api/v1/internal/injection/"+grants[0].ID.String(), h.mintRunToken(t, st.run.ID), "")
			if rr.Code != tc.want {
				t.Fatalf("sink = %d %s, want %d", rr.Code, rr.Body.String(), tc.want)
			}
			if tc.want != http.StatusOK {
				if strings.Contains(rr.Body.String(), mpOwnerKey) {
					t.Fatalf("a refusal carried the key: %s", rr.Body.String())
				}
				if ev := lastAuditEvent(t, h.audit.events, "secret.read"); ev.Outcome != "failure" || !strings.Contains(string(ev.Data), tc.reason) {
					t.Errorf("audit = %s %s, want a failure naming %s", ev.Outcome, ev.Data, tc.reason)
				}
				return
			}
			var got injectionResponse
			if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.Host != "llm.corp.example" || got.Header != "api-key" || got.Value != mpOwnerKey {
				t.Errorf("resolved = %+v, want the owner's key on the record's host and header", got)
			}
			const lease = 15 * time.Minute
			exp := time.UnixMilli(got.ExpiresAt)
			if exp.Before(before.Add(lease).Truncate(time.Millisecond)) || exp.After(time.Now().Add(lease)) {
				t.Errorf("expires_at = %v, want now+%v so the proxy re-checks the record every 10 minutes", exp, lease)
			}
		})
	}
}
