// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package awsssofake

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

// TestDeviceCodeFlowPending exercises the fake with a plain HTTP client (no
// AWS CLI, no Docker) to validate the wire protocol in isolation: register,
// start device auth, poll pending, approve, poll success, then use the token
// against the sso portal endpoints and confirm the bearer-header contract is
// enforced.
func TestDeviceCodeFlowPending(t *testing.T) {
	s := New()
	defer s.Close()

	clientID, clientSecret := registerClient(t, s)

	devResp := startDeviceAuth(t, s, clientID, clientSecret, "https://fake.awsapps.com/start")
	if devResp["deviceCode"] == "" || devResp["userCode"] == "" {
		t.Fatalf("device authorization response missing codes: %v", devResp)
	}
	if got := s.StartURLSeen(); got != "https://fake.awsapps.com/start" {
		t.Errorf("StartURLSeen() = %q, want the configured start URL", got)
	}

	// Poll before approval: must get authorization_pending, not a hard error.
	status, body := createToken(t, s, clientID, clientSecret, devResp["deviceCode"].(string))
	if status != http.StatusBadRequest {
		t.Fatalf("pre-approval CreateToken status = %d, want 400", status)
	}
	if body["error"] != "authorization_pending" {
		t.Errorf("pre-approval error = %v, want authorization_pending", body["error"])
	}

	s.Approve()

	status, tokBody := createToken(t, s, clientID, clientSecret, devResp["deviceCode"].(string))
	if status != http.StatusOK {
		t.Fatalf("post-approval CreateToken status = %d, want 200, body=%v", status, tokBody)
	}
	accessToken, _ := tokBody["accessToken"].(string)
	if accessToken == "" || accessToken != s.AccessToken() {
		t.Fatalf("CreateToken accessToken = %q, want it to match s.AccessToken() = %q", accessToken, s.AccessToken())
	}

	// GetRoleCredentials without the bearer header must fail.
	req, _ := http.NewRequest(http.MethodGet, s.URL()+"/federation/credentials?role_name=AdministratorAccess&account_id=111111111111", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GetRoleCredentials (no header): %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("GetRoleCredentials without bearer header status = %d, want 401", resp.StatusCode)
	}

	// With the correct header, it must succeed.
	req.Header.Set(bearerHeader, accessToken)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GetRoleCredentials (with header): %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("GetRoleCredentials with bearer header status = %d, body=%s", resp.StatusCode, raw)
	}
	var credResp struct {
		RoleCredentials struct {
			AccessKeyID string `json:"accessKeyId"`
		} `json:"roleCredentials"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&credResp); err != nil {
		t.Fatalf("decode GetRoleCredentials response: %v", err)
	}
	if credResp.RoleCredentials.AccessKeyID == "" {
		t.Error("GetRoleCredentials returned an empty accessKeyId")
	}
}

func registerClient(t *testing.T, s *Server) (clientID, clientSecret string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"clientName": "test", "clientType": "public"})
	resp, err := http.Post(s.URL()+"/client/register", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("RegisterClient: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("RegisterClient status = %d", resp.StatusCode)
	}
	var out struct {
		ClientID     string `json:"clientId"`
		ClientSecret string `json:"clientSecret"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode RegisterClient response: %v", err)
	}
	if out.ClientID == "" || out.ClientSecret == "" {
		t.Fatal("RegisterClient returned empty clientId/clientSecret")
	}
	return out.ClientID, out.ClientSecret
}

func startDeviceAuth(t *testing.T, s *Server, clientID, clientSecret, startURL string) map[string]any {
	t.Helper()
	body, _ := json.Marshal(map[string]string{
		"clientId": clientID, "clientSecret": clientSecret, "startUrl": startURL,
	})
	resp, err := http.Post(s.URL()+"/device_authorization", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("StartDeviceAuthorization: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StartDeviceAuthorization status = %d", resp.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode StartDeviceAuthorization response: %v", err)
	}
	return out
}

func createToken(t *testing.T, s *Server, clientID, clientSecret, deviceCode string) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{
		"clientId": clientID, "clientSecret": clientSecret,
		"grantType": deviceGrantType, "deviceCode": deviceCode,
	})
	resp, err := http.Post(s.URL()+"/token", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode CreateToken response: %v", err)
	}
	return resp.StatusCode, out
}
