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

// TestListAccountRoles_ScopedToRequestedAccount is a protocol pin: the real
// portal's ListAccountRoles answers for the account_id it was ASKED about,
// and returns an error for one the session is not entitled to. A fake that
// ignored the parameter and answered with its single fixture's role whatever
// was asked would let a helper that pins an account "verify" its pin against
// a list never scoped to it — right here, and meaningless against AWS.
//
// If handleListAccountRoles never read account_id, asking for account B would
// return account A's roles and the unknown-account arm would answer 200.
func TestListAccountRoles_ScopedToRequestedAccount(t *testing.T) {
	s := New()
	defer s.Close()
	s.SetAccounts([]Account{
		{AccountID: "222222222222", Roles: []string{"ReadOnly", "DevPower"}},
		{AccountID: "111111111111", Roles: []string{"BedrockRunner", "AdministratorAccess"}},
	})
	token := s.AccessToken()

	for _, tc := range []struct {
		accountID string
		want      []string
	}{
		{"222222222222", []string{"ReadOnly", "DevPower"}},
		{"111111111111", []string{"BedrockRunner", "AdministratorAccess"}},
	} {
		got := listAccountRoles(t, s, token, tc.accountID)
		if len(got) != len(tc.want) {
			t.Fatalf("roles for %s = %v, want %v", tc.accountID, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("roles for %s = %v, want %v", tc.accountID, got, tc.want)
			}
		}
	}

	// An account this session is not entitled to is an ERROR, not an empty
	// list and never another account's roles: a helper verifying a pin has to
	// be able to tell "not entitled" from "no roles".
	req, _ := http.NewRequest(http.MethodGet, s.URL()+"/assignment/roles?account_id=999999999999", nil)
	req.Header.Set(bearerHeader, token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("ListAccountRoles (unknown account): %v", err)
	}
	defer resp.Body.Close()
	// 403 specifically: the helper distinguishes a 4xx ("not entitled — the
	// admin's pin is wrong") from a 0/5xx ("the portal could not be reached —
	// retry"), so a fake that answered 5xx here would send a person chasing
	// the wrong problem.
	if resp.StatusCode != http.StatusForbidden {
		raw, _ := io.ReadAll(resp.Body)
		t.Errorf("ListAccountRoles for an unentitled account = %d %s, want 403 (the real portal's ForbiddenException)", resp.StatusCode, raw)
	}

	// ListAccounts must carry EVERY entitlement, in the order set — index 0 is
	// deliberately the wrong answer in this fixture (finding 1's shape).
	ids := listAccounts(t, s, token)
	if len(ids) != 2 || ids[0] != "222222222222" || ids[1] != "111111111111" {
		t.Errorf("ListAccounts = %v, want both entitlements in the order set", ids)
	}
}

// TestNewSeedsOneAccount pins the default fixture every existing caller reads:
// New() still seeds exactly one account with one role, so a test that never
// calls SetAccounts sees byte-identical behaviour.
func TestNewSeedsOneAccount(t *testing.T) {
	s := New()
	defer s.Close()
	acct := s.Account()
	if acct.AccountID != "111111111111" || len(acct.Roles) != 1 || acct.Roles[0] != "AdministratorAccess" {
		t.Errorf("New() default fixture = %+v, want one account 111111111111 with one role AdministratorAccess", acct)
	}
	if got := listAccounts(t, s, s.AccessToken()); len(got) != 1 {
		t.Errorf("ListAccounts on the default fixture = %v, want exactly one", got)
	}
}

func listAccountRoles(t *testing.T, s *Server, token, accountID string) []string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, s.URL()+"/assignment/roles?account_id="+accountID, nil)
	req.Header.Set(bearerHeader, token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("ListAccountRoles(%s): %v", accountID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("ListAccountRoles(%s) status = %d, body=%s", accountID, resp.StatusCode, raw)
	}
	var out struct {
		RoleList []struct {
			RoleName string `json:"roleName"`
		} `json:"roleList"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode ListAccountRoles(%s): %v", accountID, err)
	}
	names := make([]string, 0, len(out.RoleList))
	for _, r := range out.RoleList {
		names = append(names, r.RoleName)
	}
	return names
}

func listAccounts(t *testing.T, s *Server, token string) []string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, s.URL()+"/assignment/accounts", nil)
	req.Header.Set(bearerHeader, token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("ListAccounts status = %d, body=%s", resp.StatusCode, raw)
	}
	var out struct {
		AccountList []struct {
			AccountID string `json:"accountId"`
		} `json:"accountList"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode ListAccounts: %v", err)
	}
	ids := make([]string, 0, len(out.AccountList))
	for _, a := range out.AccountList {
		ids = append(ids, a.AccountID)
	}
	return ids
}
