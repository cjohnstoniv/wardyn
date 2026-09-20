// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package awsssofake

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// The four 0.7.6 additions the mid-run re-auth walk needs. Each is here because
// a walk that cannot reproduce a lapse in twelve minutes cannot prove the hold
// at all.

func redeemRefresh(t *testing.T, s *Server, clientID, clientSecret, refresh string) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{
		"clientId": clientID, "clientSecret": clientSecret,
		"grantType": refreshGrantType, "refreshToken": refresh,
	})
	resp, err := http.Post(s.URL()+"/token", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("CreateToken(refresh): %v", err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp.StatusCode, out
}

// signIn runs the device flow and returns the issued pair.
func signIn(t *testing.T, s *Server) (clientID, clientSecret, access, refresh string) {
	t.Helper()
	clientID, clientSecret = registerClient(t, s)
	dev := startDeviceAuth(t, s, clientID, clientSecret, "https://fake.awsapps.com/start")
	s.Approve()
	status, body := createToken(t, s, clientID, clientSecret, dev["deviceCode"].(string))
	if status != http.StatusOK {
		t.Fatalf("CreateToken = %d, want 200: %v", status, body)
	}
	return clientID, clientSecret, body["accessToken"].(string), body["refreshToken"].(string)
}

// (a) AWSSSOFAKE_TOKEN_TTL — the access token's advertised lifetime, which is
// what puts a run inside injectRefreshMargin in minutes rather than an hour.
func TestTokenTTL_IsAdvertisedAndDefaultsTo3600(t *testing.T) {
	s := New()
	defer s.Close()
	_, _, _, _ = signIn(t, s)
	// Default first: a fake nobody configured is byte-identical to before.
	s2 := New()
	defer s2.Close()
	cid, csec := registerClient(t, s2)
	dev := startDeviceAuth(t, s2, cid, csec, "https://fake.awsapps.com/start")
	s2.Approve()
	_, body := createToken(t, s2, cid, csec, dev["deviceCode"].(string))
	if body["expiresIn"] != float64(3600) {
		t.Errorf("default expiresIn = %v, want 3600", body["expiresIn"])
	}

	s3 := New()
	defer s3.Close()
	s3.SetTokenTTL(12 * time.Minute)
	cid, csec = registerClient(t, s3)
	dev = startDeviceAuth(t, s3, cid, csec, "https://fake.awsapps.com/start")
	s3.Approve()
	_, body = createToken(t, s3, cid, csec, dev["deviceCode"].(string))
	if body["expiresIn"] != float64(720) {
		t.Errorf("expiresIn with a 12m TTL = %v, want 720", body["expiresIn"])
	}
}

// (b) AWSSSOFAKE_ROLE_CRED_TTL — stamped PER CALL. This is the one that would
// silently not work: New() fixes ONE absolute Expiration at construction and
// every answer echoed it, so a constructor-only TTL would make every answer
// after the first already-expired — a refresh loop, not a T+3/6/9 cadence.
func TestRoleCredTTL_IsStampedPerCall(t *testing.T) {
	s := New()
	defer s.Close()
	_, _, access, _ := signIn(t, s)
	s.SetRoleCredTTL(3 * time.Minute)

	first := roleCredExpiry(t, s, access)
	time.Sleep(15 * time.Millisecond)
	second := roleCredExpiry(t, s, access)

	if !second.After(first) {
		t.Fatalf("two GetRoleCredentials answers carried the SAME expiry (%v) — a constructor-only TTL makes every answer after the first already-expired, which is a refresh loop rather than a cadence", first)
	}
	if d := time.Until(second); d < 2*time.Minute || d > 3*time.Minute+time.Second {
		t.Errorf("the second answer expires in %v, want ~3m", d)
	}
}

// …and with NO TTL set, the construction expiry is echoed exactly as before.
func TestRoleCredTTL_UnsetEchoesTheConstructionExpiry(t *testing.T) {
	s := New()
	defer s.Close()
	_, _, access, _ := signIn(t, s)
	first := roleCredExpiry(t, s, access)
	time.Sleep(15 * time.Millisecond)
	if second := roleCredExpiry(t, s, access); !second.Equal(first) {
		t.Errorf("the expiry moved (%v -> %v) with no TTL set", first, second)
	}
}

func roleCredExpiry(t *testing.T, s *Server, access string) time.Time {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, s.URL()+"/federation/credentials?account_id=111111111111&role_name=AdministratorAccess", nil)
	req.Header.Set(bearerHeader, access)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GetRoleCredentials: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GetRoleCredentials = %d, want 200", resp.StatusCode)
	}
	var out struct {
		RoleCredentials struct {
			Expiration int64 `json:"expiration"`
		} `json:"roleCredentials"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return time.UnixMilli(out.RoleCredentials.Expiration)
}

// (c) the invalid_grant control — the shape a session really dies in, and the
// one Wardyn classifies as "spent". Counted on REFRESH redemptions so the
// walk's own sign-in still succeeds.
func TestReauthAfter_RetiresTheSessionOnTheNthRefresh(t *testing.T) {
	s := New()
	defer s.Close()
	cid, csec, _, refresh := signIn(t, s)
	s.SetReauthAfter(2)

	status, body := redeemRefresh(t, s, cid, csec, refresh)
	if status != http.StatusOK {
		t.Fatalf("the FIRST refresh = %d, want 200: %v", status, body)
	}
	next := body["refreshToken"].(string)

	status, body = redeemRefresh(t, s, cid, csec, next)
	if status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("the second refresh = %d %v, want 400 invalid_grant — the shape awsSSOErrorIsSpent reads as a retired session", status, body["error"])
	}
	// …and it STAYS retired.
	if status, body = redeemRefresh(t, s, cid, csec, next); status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Errorf("a later refresh = %d %v, want it still retired", status, body["error"])
	}
}

// The on-cluster form of the same control: a test driving a POD cannot call the
// setter in-process.
func TestReauthControlEndpoint(t *testing.T) {
	s := New()
	defer s.Close()
	cid, csec, _, refresh := signIn(t, s)

	resp, err := http.Post(s.URL()+"/_control/reauth?after=1", "", nil)
	if err != nil {
		t.Fatalf("control: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("control = %d, want 200", resp.StatusCode)
	}
	if status, body := redeemRefresh(t, s, cid, csec, refresh); status != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("after the control, refresh = %d %v, want 400 invalid_grant", status, body["error"])
	}
	// after=0 puts it back, so one walk can kill AND restore a session.
	resp, err = http.Post(s.URL()+"/_control/reauth?after=0", "", nil)
	if err != nil {
		t.Fatalf("control: %v", err)
	}
	_ = resp.Body.Close()
	if status, _ := redeemRefresh(t, s, cid, csec, refresh); status != http.StatusOK {
		t.Errorf("after=0 did not restore the session: refresh = %d", status)
	}
}
