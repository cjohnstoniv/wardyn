// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

func TestLocalPlacementDoorsP3AndUpstreamRefuseBeforeIdentityOrStore(t *testing.T) {
	for _, tc := range []struct {
		name    string
		arrange func(*componentFixture)
		body    map[string]any
		reason  string
		status  int
	}{
		{name: "P3 unassigned default", body: componentBody(map[string]any{"name": "My Tool", "inline": map[string]any{"hosts": []string{"mine.example"}}}), reason: string(placement.ReasonPlacementComponentSelfDefine), status: 403},
		{name: "upstream secret", arrange: func(f *componentFixture) { f.st.siteConfig.UpstreamProxySecretRef = "corporate-parent" }, body: componentBody(), reason: string(placement.ReasonPlacementCredential), status: 422},
		{name: "own inputs still unsupported", body: componentBody(), reason: string(placement.ReasonPlacementUnavailable), status: 422},
	} {
		for _, door := range componentDoors {
			t.Run(tc.name+door, func(t *testing.T) {
				f := newComponentFixture(t)
				if tc.arrange != nil {
					tc.arrange(f)
				}
				spy := &mintCountingIdentity{Provider: f.srv.cfg.Identity}
				f.srv.cfg.Identity = spy
				body := map[string]any{}
				for k, v := range tc.body {
					body[k] = v
				}
				body["placement"] = "local"
				w := f.ask(t, door, body)
				got := decodeErrorBody(t, w)
				if w.Code != tc.status || got.Reason != tc.reason {
					t.Fatalf("%d %s, want %d %s", w.Code, w.Body.String(), tc.status, tc.reason)
				}
				if spy.mints != 0 || len(f.st.runs) != 0 || len(f.st.grants) != 0 || len(f.st.snapshots) != 0 {
					t.Fatalf("refused local run wrote/minted: %d/%d/%d/%d", spy.mints, len(f.st.runs), len(f.st.grants), len(f.st.snapshots))
				}
				if tc.reason == string(placement.ReasonPlacementComponentSelfDefine) && len(f.componentAudit(0, "authz.denied")) == 0 {
					t.Fatal("P3 refusal was not audited")
				}
			})
		}
	}
}

func TestLocalPlacementRefusalCannotRenewExpiredAWSOrWritePair(t *testing.T) {
	srv := createRenewalFixture(t)
	calls := renewedOIDC(t)
	for _, door := range contractDoors {
		body := strings.TrimSuffix(createRenewalBody, "}") + `,"placement":"local"}`
		w := do(t, srv, http.MethodPost, door.path, providerAdminToken(srv, createRenewalOwner), body)
		if w.Code != 422 || errorReason(w) != string(placement.ReasonPlacementUnavailable) {
			t.Fatalf("%s=%d %s", door.name, w.Code, w.Body.String())
		}
	}
	if calls.Load() != 0 || runRowCount(srv) != 0 {
		t.Fatalf("refused path renewed=%d created=%d", calls.Load(), runRowCount(srv))
	}
	if got, _ := createRenewalStored(t, srv); got.RefreshToken != "old-refresh-token-1234567890" {
		t.Fatal("refused local path changed the pair")
	}
}

func TestLocalStoredGrantPersistenceForcesOwnerOnlyAndRefusesFallback(t *testing.T) {
	scopes := []types.GrantSpec{
		{Kind: types.GrantAPIKey, Scope: json.RawMessage(`{"host":"api.example","secret_name":"person-secret"}`)},
		{Kind: types.GrantGitPAT, Scope: json.RawMessage(`{"host":"git.example","secret_name":"person-secret"}`)},
		{Kind: types.GrantSSHKey, Scope: json.RawMessage(`{"host":"ssh.github.com","key_secret_ref":"person-secret"}`)},
		{Kind: types.GrantEnvSecret, Scope: json.RawMessage(`{"name":"MY_KEY","secret_name":"person-secret"}`)},
		{Kind: types.GrantFileSecret, Scope: json.RawMessage(`{"file":"key","secret_name":"person-secret"}`)},
	}
	f := newComponentFixture(t)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(utCtx("user", "", nil))
	spec := types.RunPolicySpec{EligibleGrants: scopes}
	if _, ok := f.srv.persistRunGrants(context.Background(), httptest.NewRecorder(), r, uuid.New(), time.Now().UTC(), spec, types.PlacementLocal); !ok {
		t.Fatal("own local grants refused")
	}
	if len(f.st.grants) != len(scopes) {
		t.Fatalf("persisted=%d", len(f.st.grants))
	}
	for _, g := range f.st.grants {
		if !g.Spec.OwnerOnly {
			t.Fatalf("%s permits operator fallback", g.Spec.Kind)
		}
	}
	for _, g := range scopes {
		if g.OwnerOnly {
			t.Fatal("mutated caller policy")
		}
	}
	before := len(f.st.grants)
	spec.EligibleGrants = append(spec.EligibleGrants, types.GrantSpec{Kind: types.GrantAPIKey, Scope: json.RawMessage(`{"host":"corp.example","secret_name":"operator-only-secret"}`)})
	if _, ok := f.srv.persistRunGrants(context.Background(), httptest.NewRecorder(), r, uuid.New(), time.Now().UTC(), spec, types.PlacementLocal); ok || len(f.st.grants) != before {
		t.Fatal("missing own namespace wrote a partial grant set")
	}
}

func TestLocalPlacementOrdinaryScanTaskDoesNotBecomeTrustedOutput(t *testing.T) {
	f := newComponentFixture(t)
	for _, door := range componentDoors {
		body := componentBody()
		body["task"] = "source scan"
		body["placement"] = "local"
		w := f.ask(t, door, body)
		if w.Code != 422 || errorReason(w) != string(placement.ReasonPlacementUnavailable) {
			t.Fatalf("ordinary task became trusted output: %d %s", w.Code, w.Body.String())
		}
	}
}

func TestLocalP3ComponentNameIsResponseOnly(t *testing.T) {
	f := newComponentFixture(t)
	body := componentBody(map[string]any{"name": "Private Personal Tool", "inline": map[string]any{"hosts": []string{"mine.example"}}})
	body["placement"] = "local"
	w := f.ask(t, componentDoors[0], body)
	if w.Code != 403 || !strings.Contains(w.Body.String(), "Private Personal Tool") {
		t.Fatalf("P3 response=%d %s", w.Code, w.Body.String())
	}
	for _, ev := range f.rec.snapshot() {
		if strings.Contains(string(ev.Data), "Private Personal Tool") {
			t.Fatalf("personal component name reached append-only %s", ev.Action)
		}
	}
}
