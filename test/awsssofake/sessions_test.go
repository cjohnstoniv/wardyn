// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package awsssofake

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// getRoleCreds calls GetRoleCredentials with token and returns the status and
// the answer's access key id.
func getRoleCreds(t *testing.T, s *Server, token string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, s.URL()+"/federation/credentials?role_name=WardynDev&account_id=222222222222", nil)
	req.Header.Set(bearerHeader, token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GetRoleCredentials: %v", err)
	}
	defer resp.Body.Close()
	var body struct {
		RoleCredentials struct {
			AccessKeyID string `json:"accessKeyId"`
		} `json:"roleCredentials"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body.RoleCredentials.AccessKeyID
}

type seenSession struct {
	Session         int            `json:"session"`
	RoleCredCallers map[string]int `json:"role_cred_callers"`
	BedrockCallers  map[string]int `json:"bedrock_callers"`
}

func seenSessions(t *testing.T, s *Server) map[int]seenSession {
	t.Helper()
	resp, err := http.Get(s.URL() + "/_seen")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Sessions []seenSession `json:"sessions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	out := map[int]seenSession{}
	for _, ss := range body.Sessions {
		out[ss.Session] = ss
	}
	return out
}

// TestConcurrentSignInsKeepTheirOwnSessions: two people signing in at once each
// get a session of their own. A second sign-in, or the first one's refresh,
// never invalidates the other's token; each session's role credentials carry
// their own access key id; and /_seen attributes every GetRoleCredentials call
// and every bedrock call signed with a session's key to that session and the
// peer that made it. The kind concurrency walk reads exactly this to tell which
// run spent which person's session.
func TestConcurrentSignInsKeepTheirOwnSessions(t *testing.T) {
	s := New()
	defer s.Close()
	s.Approve()
	clientID, clientSecret := registerClient(t, s)
	signIn := func() (access, refresh string) {
		dev := startDeviceAuth(t, s, clientID, clientSecret, "https://fake.awsapps.com/start")
		status, tok := createToken(t, s, clientID, clientSecret, dev["deviceCode"].(string))
		if status != http.StatusOK {
			t.Fatalf("CreateToken = %d %v", status, tok)
		}
		return tok["accessToken"].(string), tok["refreshToken"].(string)
	}
	accessA, refreshA := signIn()
	accessB, _ := signIn()

	statusA, keyA := getRoleCreds(t, s, accessA)
	statusB, keyB := getRoleCreds(t, s, accessB)
	if statusA != http.StatusOK || statusB != http.StatusOK {
		t.Fatalf("GetRoleCredentials: A %d, B %d; want both 200 — B's sign-in must not end A's session", statusA, statusB)
	}
	if keyA == "" || keyA == keyB {
		t.Fatalf("access key ids A %q, B %q; want one per session", keyA, keyB)
	}

	status, refreshed := postToken(t, s, map[string]string{
		"clientId": clientID, "clientSecret": clientSecret, "grantType": "refresh_token", "refreshToken": refreshA,
	})
	if status != http.StatusOK {
		t.Fatalf("refresh A = %d %v", status, refreshed)
	}
	if st, _ := getRoleCreds(t, s, accessA); st != http.StatusUnauthorized {
		t.Errorf("A's pre-refresh token = %d, want 401", st)
	}
	if st, _ := getRoleCreds(t, s, accessB); st != http.StatusOK {
		t.Errorf("B's token after A's refresh = %d, want 200", st)
	}
	if st, key := getRoleCreds(t, s, refreshed["accessToken"].(string)); st != http.StatusOK || key != keyA {
		t.Errorf("A's refreshed token = %d with key %q, want 200 with A's key %q", st, key, keyA)
	}

	req, _ := http.NewRequest(http.MethodPost, s.URL()+"/model/m/converse", strings.NewReader("{}"))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+keyB+"/20260928/us-east-1/bedrock/aws4_request, SignedHeaders=host, Signature=00")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	var a, b seenSession
	for _, ss := range seenSessions(t, s) {
		switch sessionAccessKeyID(ss.Session) {
		case keyA:
			a = ss
		case keyB:
			b = ss
		}
	}
	if a.RoleCredCallers["127.0.0.1"] != 2 || b.RoleCredCallers["127.0.0.1"] != 2 {
		t.Errorf("role-credential callers: A %v, B %v; want 2 calls each from 127.0.0.1", a.RoleCredCallers, b.RoleCredCallers)
	}
	if len(a.BedrockCallers) != 0 || b.BedrockCallers["127.0.0.1"] != 1 {
		t.Errorf("bedrock callers: A %v, B %v; want the one call signed with B's key under B only", a.BedrockCallers, b.BedrockCallers)
	}
}
