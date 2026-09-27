// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package awsssofake

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The three arms this file pins exist for the ON-CLUSTER fake (cmd/main.go,
// deploy/kind/sso/awsssofake.yaml): the refresh_token grant a dispatch-time
// renewal exercises (G4), the /_seen observation endpoint that carries
// RoleCredentialsSeen() across the process boundary (an in-process test reads
// the accessor; a test driving a POD cannot), and the Bedrock-runtime stub that
// gives the minted role credentials something to be spent on (G6).

// TestCreateToken_RefreshGrant is the G4 arm: a real dispatch-time renewal
// (internal/api/awssso_refresh.go createAWSSSOToken) POSTs grantType
// "refresh_token" with the stored refreshToken, and the fake must answer it
// rather than reject every such call with UnsupportedGrantTypeException —
// otherwise the renewal path could only be exercised against an httptest stub
// written for one test.
func TestCreateToken_RefreshGrant(t *testing.T) {
	s := New()
	defer s.Close()

	clientID, clientSecret := registerClient(t, s)
	devResp := startDeviceAuth(t, s, clientID, clientSecret, "https://fake.awsapps.com/start")
	s.Approve()
	status, tok := createToken(t, s, clientID, clientSecret, devResp["deviceCode"].(string))
	if status != http.StatusOK {
		t.Fatalf("device-grant CreateToken status = %d, want 200 (body=%v)", status, tok)
	}
	firstAccess, _ := tok["accessToken"].(string)
	firstRefresh, _ := tok["refreshToken"].(string)
	if firstRefresh == "" {
		t.Fatalf("device-grant CreateToken returned no refreshToken: %v", tok)
	}

	// The renewal itself.
	status, refreshed := postToken(t, s, map[string]string{
		"clientId":     clientID,
		"clientSecret": clientSecret,
		"grantType":    "refresh_token",
		"refreshToken": firstRefresh,
	})
	if status != http.StatusOK {
		t.Fatalf("refresh_token CreateToken status = %d, want 200 (body=%v)", status, refreshed)
	}
	nextAccess, _ := refreshed["accessToken"].(string)
	nextRefresh, _ := refreshed["refreshToken"].(string)
	if nextAccess == "" || nextAccess == firstAccess {
		t.Errorf("refresh returned accessToken %q, want a NEW one (was %q)", nextAccess, firstAccess)
	}
	if nextRefresh == "" || nextRefresh == firstRefresh {
		t.Errorf("refresh returned refreshToken %q, want a ROTATED one (was %q) — refreshAWSSSOBlob's `rotated` arm is only exercised when the token actually moves", nextRefresh, firstRefresh)
	}
	if got := s.AccessToken(); got != nextAccess {
		t.Errorf("s.AccessToken() = %q after refresh, want the newly issued %q", got, nextAccess)
	}
	if _, ok := refreshed["expiresIn"]; !ok {
		t.Errorf("refresh response has no expiresIn: %v", refreshed)
	}

	// The SPENT arm: the old refresh token must not work twice. Wardyn
	// classifies invalid_grant as spent (awsSSOErrorIsSpent), which is the
	// signal that stops a fleet re-trying a dead grant.
	status, spent := postToken(t, s, map[string]string{
		"clientId":     clientID,
		"clientSecret": clientSecret,
		"grantType":    "refresh_token",
		"refreshToken": firstRefresh,
	})
	if status != http.StatusBadRequest || spent["error"] != "invalid_grant" {
		t.Errorf("replaying the consumed refresh token = %d %v, want 400 invalid_grant", status, spent)
	}
}

// TestSeenEndpoint is the observation arm: on a cluster the fake is a POD, so
// RoleCredentialsSeen() — the only place a test can read WHICH identity real
// botocore asked AWS to mint — has to cross the process boundary as JSON.
func TestSeenEndpoint(t *testing.T) {
	s := New()
	defer s.Close()

	// Nothing seen yet: the endpoint answers, with empty fields.
	seen := getSeen(t, s.URL())
	if seen.AccountID != "" || seen.RoleName != "" {
		t.Errorf("/_seen before any call = %+v, want empty account/role", seen)
	}

	clientID, clientSecret := registerClient(t, s)
	devResp := startDeviceAuth(t, s, clientID, clientSecret, "https://fake.awsapps.com/start")
	s.Approve()
	_, tok := createToken(t, s, clientID, clientSecret, devResp["deviceCode"].(string))
	token, _ := tok["accessToken"].(string)

	req, _ := http.NewRequest(http.MethodGet, s.URL()+"/federation/credentials?role_name=BedrockRunner&account_id=222222222222", nil)
	req.Header.Set(bearerHeader, token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GetRoleCredentials: %v", err)
	}
	resp.Body.Close()

	seen = getSeen(t, s.URL())
	if seen.AccountID != "222222222222" || seen.RoleName != "BedrockRunner" {
		t.Errorf("/_seen = %+v, want account 222222222222 / role BedrockRunner", seen)
	}
	if seen.StartURL != "https://fake.awsapps.com/start" {
		t.Errorf("/_seen start_url = %q, want the configured start URL", seen.StartURL)
	}
}

// TestBedrockStub is the G6 arm: something has to CONSUME the minted role
// credentials, or a green walk proves only that a credential was minted, not
// that a model call was made with it. Both shapes claude-code can emit
// (Converse, InvokeModel) answer a canned body and bump one counter that
// /_seen reports.
func TestBedrockStub(t *testing.T) {
	s := New()
	defer s.Close()

	const modelID = "arn:aws:bedrock:us-east-1:222222222222:inference-profile/us.anthropic.claude-sonnet-4-5"
	for _, op := range []string{"converse", "invoke"} {
		body, status := postBedrock(t, s.URL(), modelID, op)
		if status != http.StatusOK {
			t.Fatalf("bedrock %s status = %d, body=%s", op, status, body)
		}
		if !bytes.Contains(body, []byte("wardyn-bedrock-stub")) {
			t.Errorf("bedrock %s body = %s, want the canned stub answer", op, body)
		}
	}

	seen := getSeen(t, s.URL())
	if seen.BedrockCalls != 2 {
		t.Errorf("/_seen bedrock_calls = %d, want 2", seen.BedrockCalls)
	}
	if seen.BedrockModel != modelID {
		t.Errorf("/_seen bedrock_model = %q, want the model id the caller asked for", seen.BedrockModel)
	}
}

// TestNewHandler serves the SAME mux without httptest, which is what
// test/awsssofake/cmd binds to a fixed port inside the cluster. A second
// constructor rather than exporting the field: New() must keep starting its own
// server for every in-process caller that already exists.
func TestNewHandler(t *testing.T) {
	s, h := NewHandler()
	if s == nil || h == nil {
		t.Fatalf("NewHandler() = (%v, %v), want both non-nil", s, h)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	seen := getSeen(t, srv.URL)
	if seen.AccountID != "" {
		t.Errorf("/_seen on a fresh handler = %+v, want empty", seen)
	}
	// URL() on an unstarted fake must not panic — cmd/main.go never calls it,
	// but a future caller reaching for it deserves an answer, not a nil deref.
	if got := s.URL(); got != "" {
		t.Errorf("URL() on an unstarted fake = %q, want \"\"", got)
	}
}

// helpers

type seenBody struct {
	AccountID    string `json:"account_id"`
	RoleName     string `json:"role_name"`
	StartURL     string `json:"start_url"`
	BedrockCalls int    `json:"bedrock_calls"`
	BedrockModel string `json:"bedrock_model"`
}

func getSeen(t *testing.T, base string) seenBody {
	t.Helper()
	resp, err := http.Get(base + "/_seen")
	if err != nil {
		t.Fatalf("GET /_seen: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /_seen status = %d, want 200", resp.StatusCode)
	}
	var out seenBody
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode /_seen: %v", err)
	}
	return out
}

// postToken POSTs an arbitrary CreateToken body (createToken above is the
// device-grant convenience and is deliberately left alone).
func postToken(t *testing.T, s *Server, body map[string]string) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	resp, err := http.Post(s.URL()+"/token", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("POST /token: %v", err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode /token response: %v", err)
	}
	return resp.StatusCode, out
}

// postBedrock calls the bedrock-runtime stub the way the real data plane is
// addressed: /model/<url-escaped model id>/{converse,invoke}.
func postBedrock(t *testing.T, base, modelID, op string) ([]byte, int) {
	t.Helper()
	url := base + "/model/" + modelID + "/" + op
	resp, err := http.Post(url, "application/json", bytes.NewReader([]byte(`{"messages":[]}`)))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return raw, resp.StatusCode
}

// R-10: handleCreateToken validates clientId/clientSecret for BOTH grants, and
// nothing exercised that on the refresh arm — a fake that answered 200 to an
// unregistered client would let a broken credential store look healthy.
func TestCreateToken_RefreshGrantChecksClientCredentials(t *testing.T) {
	s := New()
	defer s.Close()

	clientID, clientSecret := registerClient(t, s)
	devResp := startDeviceAuth(t, s, clientID, clientSecret, "https://fake.awsapps.com/start")
	s.Approve()
	_, tok := createToken(t, s, clientID, clientSecret, devResp["deviceCode"].(string))
	refresh, _ := tok["refreshToken"].(string)

	for _, tc := range []struct{ name, id, secret string }{
		{"wrong secret", clientID, "not-the-registered-secret"},
		{"wrong client id", "not-the-registered-client", clientSecret},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body := postToken(t, s, map[string]string{
				"clientId":     tc.id,
				"clientSecret": tc.secret,
				"grantType":    "refresh_token",
				"refreshToken": refresh,
			})
			if status != http.StatusBadRequest || body["error"] != "invalid_client" {
				t.Errorf("refresh with a %s = %d %v, want 400 invalid_client", tc.name, status, body)
			}
		})
	}

	// …and the valid pair still works afterwards: a refused attempt must not
	// consume or rotate anything.
	status, ok := postToken(t, s, map[string]string{
		"clientId": clientID, "clientSecret": clientSecret,
		"grantType": "refresh_token", "refreshToken": refresh,
	})
	if status != http.StatusOK {
		t.Errorf("the valid refresh after two refusals = %d %v, want 200", status, ok)
	}
}
