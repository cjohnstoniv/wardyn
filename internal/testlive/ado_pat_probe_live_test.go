// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build live

package testlive

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// TestLiveADOPATMintProbe (LL2c) re-measures the one fact the minted_pat token
// mode was dropped on: can an application that is NOT a Microsoft first-party
// client mint a personal access token through the token lifecycle API with a
// user-delegated Entra token? Wardyn's own app never requests vso.pats
// (adoscope.neverRequestedScopes — consenting to it would put it in every run's
// token), so this uses a separate, throwaway public-client app registration.
//
// It is a probe, not a regression test: it passes whichever way Azure DevOps
// answers and logs the verdict. It fails only when it cannot get an answer. A
// PAT that does get minted is revoked before the test returns.
func TestLiveADOPATMintProbe(t *testing.T) {
	Require(t, EnvADOPATProbe, EnvADOPATProbeTenant, EnvADOPATProbeClient, EnvADOOrg)
	tenant, clientID, org := os.Getenv(EnvADOPATProbeTenant), os.Getenv(EnvADOPATProbeClient), os.Getenv(EnvADOOrg)
	scope := cmp.Or(os.Getenv(EnvADOPATProbeScope), "499b84ac-1321-427f-aa17-267ca6975798/vso.pats")

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	token := probeSignIn(ctx, t, tenant, clientID, scope)

	base := "https://vssps.dev.azure.com/" + url.PathEscape(org) + "/_apis/tokens/pats?api-version=7.1-preview.1"
	status, _, _ := probeCall(ctx, t, token, http.MethodGet, base, "")
	Logf(t, "list PATs: HTTP %d", status)

	body := fmt.Sprintf(`{"displayName":"wardyn-pat-probe","scope":"vso.code","validTo":%q,"allOrgs":false}`,
		time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	status, hdr, raw := probeCall(ctx, t, token, http.MethodPost, base, body)
	var created struct {
		PatToken struct {
			AuthorizationID string `json:"authorizationId"`
			Token           string `json:"token"`
		} `json:"patToken"`
		PatTokenError string `json:"patTokenError"`
	}
	_ = json.Unmarshal(raw, &created)
	if status == http.StatusOK && created.PatToken.Token != "" {
		st, _, _ := probeCall(ctx, t, token, http.MethodDelete, base+"&authorizationId="+url.QueryEscape(created.PatToken.AuthorizationID), "")
		Logf(t, "VERDICT: MINT WORKS for this app with scope %q; the probe PAT was revoked (HTTP %d)", scope, st)
		return
	}
	Logf(t, "VERDICT: MINT REFUSED with scope %q: HTTP %d, X-TFS-ServiceError %q, patTokenError %q, body %s",
		scope, status, hdr.Get("X-TFS-ServiceError"), created.PatTokenError, Redact(strings.TrimPrefix(string(raw), "\ufeff")))
}

// probeSignIn runs an authorization-code + PKCE sign-in on a loopback redirect
// and returns the access token. The person opens the logged URL in a browser.
func probeSignIn(ctx context.Context, t *testing.T, tenant, clientID, scope string) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		Fatalf(t, "listen: %v", err)
	}
	redirect := fmt.Sprintf("http://localhost:%d/", ln.Addr().(*net.TCPAddr).Port)
	verifier := probeRandom()
	sum := sha256.Sum256([]byte(verifier))
	state := probeRandom()
	authority := "https://login.microsoftonline.com/" + url.PathEscape(tenant) + "/oauth2/v2.0/"
	authURL := authority + "authorize?" + url.Values{
		"client_id": {clientID}, "response_type": {"code"}, "redirect_uri": {redirect},
		"scope": {scope}, "state": {state}, "prompt": {"select_account"},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"},
	}.Encode()
	// Unredacted on purpose: the URL holds only public values (tenant, client
	// id, PKCE challenge, state), and Redact would mask them into an unusable link.
	t.Logf("open this URL in a browser and sign in as the member:\n%s", authURL)

	codes := make(chan string, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("state") != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			return
		}
		if e := q.Get("error"); e != "" {
			fmt.Fprintf(w, "sign-in refused: %s", e)
			Logf(t, "sign-in refused: %s: %s", e, q.Get("error_description"))
			codes <- ""
			return
		}
		fmt.Fprintln(w, "Signed in. You can close this tab.")
		codes <- q.Get("code")
	})}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	var code string
	select {
	case code = <-codes:
	case <-ctx.Done():
		Fatalf(t, "no sign-in within the time limit")
	}
	if code == "" {
		Fatalf(t, "sign-in did not return a code")
	}
	form := url.Values{
		"client_id": {clientID}, "grant_type": {"authorization_code"}, "code": {code},
		"redirect_uri": {redirect}, "code_verifier": {verifier}, "scope": {scope},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, authority+"token", strings.NewReader(form.Encode()))
	if err != nil {
		Fatalf(t, "token redeem: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		Fatalf(t, "token redeem: %v", err)
	}
	defer resp.Body.Close()
	var tok struct {
		AccessToken string `json:"access_token"`
		Scope       string `json:"scope"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil || tok.AccessToken == "" {
		Fatalf(t, "token redeem: HTTP %d %s %s", resp.StatusCode, tok.Error, tok.Description)
	}
	Logf(t, "granted scope: %s", tok.Scope)
	return tok.AccessToken
}

// probeCall makes one Azure DevOps call with the bearer and returns the status,
// headers and body. It never logs the token.
func probeCall(ctx context.Context, t *testing.T, token, method, u, body string) (int, http.Header, []byte) {
	req, err := http.NewRequestWithContext(ctx, method, u, strings.NewReader(body))
	if err != nil {
		Fatalf(t, "%s: %v", method, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		Fatalf(t, "%s: %v", method, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, resp.Header, raw
}

func probeRandom() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
