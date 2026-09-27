// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package entrafake_test

import (
	"encoding/base64"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/cjohnstoniv/wardyn/test/entrafake"
)

// TestPicker_SignsInThePersonPicked: with identities set, /authorize answers
// the picker; following one person's link binds the code, the id_token, the
// refresh grant and every token the hook hears about to that person.
func TestPicker_SignsInThePersonPicked(t *testing.T) {
	s := entrafake.New()
	t.Cleanup(s.Close)
	admin := entrafake.Identity{Username: "admin@wardyn.test", Subject: "sub-admin"}
	member := entrafake.Identity{Username: "member@wardyn.test", Subject: "sub-member"}
	s.SetIdentities(admin, member)
	var mu sync.Mutex
	var issued []entrafake.IssuedToken
	s.OnIssue(func(it entrafake.IssuedToken) { mu.Lock(); issued = append(issued, it); mu.Unlock() })

	scope := "openid offline_access " + s.ConsentedScopes()[1]
	resp, err := noRedirectClient().Get(s.AuthorizeURL("st", entrafake.Challenge("v-1"), "n", scope))
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	page, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(page), admin.Username) {
		t.Fatalf("picker: status %d body %s", resp.StatusCode, page)
	}
	var link string
	for _, m := range regexp.MustCompile(`href="([^"]+)"`).FindAllStringSubmatch(string(page), -1) {
		if strings.Contains(html.UnescapeString(m[1]), url.QueryEscape(member.Username)) {
			link = html.UnescapeString(m[1])
		}
	}
	if link == "" {
		t.Fatalf("no picker link for %s in %s", member.Username, page)
	}
	resp, err = noRedirectClient().Get(s.URL() + link)
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	_ = resp.Body.Close()
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != http.StatusFound || loc.Query().Get("code") == "" {
		t.Fatalf("pick: status %d Location %s", resp.StatusCode, loc)
	}
	status, body := postToken(t, s, url.Values{
		"grant_type": {"authorization_code"}, "code": {loc.Query().Get("code")},
		"code_verifier": {"v-1"}, "redirect_uri": {s.RedirectURI()},
	})
	if status != http.StatusOK {
		t.Fatalf("code redeem: %d %v", status, body)
	}
	claims := idTokenClaims(t, body["id_token"].(string))
	if claims["sub"] != member.Subject || claims["email"] != member.Username || claims["preferred_username"] != member.Username {
		t.Errorf("id_token claims = %v, want the member's", claims)
	}
	status, _ = postToken(t, s, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {body["refresh_token"].(string)}})
	if status != http.StatusOK {
		t.Fatalf("refresh: %d", status)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(issued) != 2 || issued[0].Subject != member.Subject || issued[1].Subject != member.Subject {
		t.Errorf("hook heard %+v, want two tokens, both the member's", issued)
	}
	if issued[0].AccessToken != body["access_token"] {
		t.Errorf("hook token %q is not the token the client got", issued[0].AccessToken)
	}

	// A login_hint naming nobody is refused in band, never redirected.
	resp, err = noRedirectClient().Get(s.AuthorizeURL("st", entrafake.Challenge("v-2"), "n", scope) + "&login_hint=nobody%40x")
	if err != nil {
		t.Fatalf("unknown hint: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown login_hint: status %d, want 400", resp.StatusCode)
	}
}

// TestSetBaseURL_MovesIssuerAndEndpoints: served behind a front that owns
// another name, discovery and the id_token's iss use that name.
func TestSetBaseURL_MovesIssuerAndEndpoints(t *testing.T) {
	s := entrafake.New()
	t.Cleanup(s.Close)
	real := s.URL()
	s.SetBaseURL("https://login.microsoftonline.com/")
	want := "https://login.microsoftonline.com/" + s.TenantID() + "/v2.0"
	if s.Issuer() != want {
		t.Fatalf("Issuer() = %q, want %q", s.Issuer(), want)
	}
	resp, err := http.Get(real + "/" + s.TenantID() + "/v2.0/.well-known/openid-configuration")
	if err != nil {
		t.Fatalf("discovery: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var doc map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if doc["issuer"] != want || !strings.HasPrefix(doc["token_endpoint"].(string), "https://login.microsoftonline.com/") {
		t.Errorf("discovery = %v, want everything under the base URL", doc)
	}
}

func idTokenClaims(t *testing.T, tok string) map[string]any {
	t.Helper()
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("id_token is not a JWS: %q", tok)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	out := map[string]any{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	return out
}
