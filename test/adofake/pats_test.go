// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package adofake

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// TestPatRoutesTakeOnlyAnEntraBearerWithBothPATScopes pins the shape the live
// gate measured: an Entra bearer holding vso.pats and vso.pats_manage creates;
// the retired vso.tokens alone, either PAT scope alone, and a PAT over Basic
// carrying both are all refused.
func TestPatRoutesTakeOnlyAnEntraBearerWithBothPATScopes(t *testing.T) {
	s := New()
	defer s.Close()
	s.RegisterToken("tokens-only", ScopeTokens)
	s.RegisterToken("pats-only", ScopePats)
	s.RegisterToken("manage-only", ScopePatsManage)
	s.RegisterToken("both", ScopePats, ScopePatsManage)
	base := s.URL() + "/fakeorg/_apis/tokens/pats"
	body := []byte(`{"displayName":"x","scope":"vso.code","validTo":"2099-01-01T00:00:00Z","allOrgs":false}`)

	for _, tok := range []string{"tokens-only", "pats-only", "manage-only"} {
		if status, _ := postJSON(t, base, tok, body); status != http.StatusUnauthorized {
			t.Errorf("create with a %s bearer: status %d, want 401", tok, status)
		}
	}
	req, _ := http.NewRequest(http.MethodPost, base, nil)
	req.SetBasicAuth("", "both")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST over Basic: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("create over Basic: status %d, want 401 — the lifecycle API takes no PAT", resp.StatusCode)
	}
	if status, raw := postJSON(t, base, "both", body); status != http.StatusOK {
		t.Errorf("create with both scopes: status %d body %s, want 200", status, raw)
	}
}

// TestPatLifespanLimit: a validTo past the limit answers the configured
// patTokenError with no token; one inside it is created.
func TestPatLifespanLimit(t *testing.T) {
	s := New()
	defer s.Close()
	s.RegisterToken("both", ScopePats, ScopePatsManage)
	s.SetPatLifespanLimit(7*24*time.Hour, PatTokenErrorLifespanPolicyViolation)
	base := s.URL() + "/fakeorg/_apis/tokens/pats"
	create := func(d time.Duration) (string, string) {
		t.Helper()
		body := fmt.Sprintf(`{"displayName":"x","scope":"vso.profile","validTo":%q,"allOrgs":false}`,
			time.Now().Add(d).UTC().Format(time.RFC3339))
		_, raw := postJSON(t, base, "both", []byte(body))
		var out struct {
			PatToken *struct {
				Token string `json:"token"`
			} `json:"patToken"`
			PatTokenError string `json:"patTokenError"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("decode: %v (%s)", err, raw)
		}
		tok := ""
		if out.PatToken != nil {
			tok = out.PatToken.Token
		}
		return tok, out.PatTokenError
	}
	if tok, perr := create(8 * time.Hour); tok == "" || perr != string(PatTokenErrorNone) {
		t.Errorf("8h inside a 7-day limit: token %q error %q, want a token", tok, perr)
	}
	if tok, perr := create(364 * 24 * time.Hour); tok != "" || perr != string(PatTokenErrorLifespanPolicyViolation) {
		t.Errorf("364 days past a 7-day limit: token %q error %q, want the lifespan violation", tok, perr)
	}
	s.SetPatLifespanLimit(0, "")
	if tok, _ := create(364 * 24 * time.Hour); tok == "" {
		t.Error("with no limit a 364-day token was refused")
	}
}
