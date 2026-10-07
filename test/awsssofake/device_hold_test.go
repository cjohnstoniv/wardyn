// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package awsssofake

import (
	"net/http"
	"strings"
	"testing"
)

func postDeviceControl(t *testing.T, s *Server, query string) int {
	t.Helper()
	resp, err := http.Post(s.URL()+"/_control/device?"+query, "", nil)
	if err != nil {
		t.Fatalf("control: %v", err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// Hold, start, approve, and the next sign-in is pre-approved again: the hold
// covers exactly one device authorization and clears itself when approved.
func TestDeviceHold_HoldsTheNextSignInUntilApproved(t *testing.T) {
	s := New()
	defer s.Close()
	s.Approve() // the on-cluster default: every device code approved at once
	cid, csec := registerClient(t, s)

	if code := postDeviceControl(t, s, "hold=1"); code != http.StatusOK {
		t.Fatalf("hold = %d, want 200", code)
	}
	held := startDeviceAuth(t, s, cid, csec, "https://fake.awsapps.com/start")
	if got := held["verificationUriComplete"]; got != "https://device.sso.us-east-1.amazonaws.com/?user_code="+held["userCode"].(string) {
		t.Errorf("held verificationUriComplete = %v, want the device endpoint's address with the code", got)
	}
	// A second sign-in started while the first is held is not held.
	other := startDeviceAuth(t, s, cid, csec, "https://fake.awsapps.com/start")
	if status, _ := createToken(t, s, cid, csec, other["deviceCode"].(string)); status != http.StatusOK {
		t.Fatalf("an unheld sign-in = %d, want 200", status)
	}
	if strings.HasPrefix(other["verificationUri"].(string), "https://device.sso.") {
		t.Errorf("an unheld sign-in names the held address: %v", other["verificationUri"])
	}
	for range 2 {
		if status, body := createToken(t, s, cid, csec, held["deviceCode"].(string)); status != http.StatusBadRequest || body["error"] != "authorization_pending" {
			t.Fatalf("held CreateToken = %d %v, want 400 authorization_pending", status, body["error"])
		}
	}

	if code := postDeviceControl(t, s, "approve=1"); code != http.StatusOK {
		t.Fatalf("approve = %d, want 200", code)
	}
	if status, body := createToken(t, s, cid, csec, held["deviceCode"].(string)); status != http.StatusOK {
		t.Fatalf("approved CreateToken = %d %v, want 200", status, body)
	}
	next := startDeviceAuth(t, s, cid, csec, "https://fake.awsapps.com/start")
	if status, _ := createToken(t, s, cid, csec, next["deviceCode"].(string)); status != http.StatusOK {
		t.Fatalf("the next sign-in = %d, want 200 (pre-approved again)", status)
	}
}

// A hold no sign-in used is dropped by approve, so a failed case that held and
// never started cannot stall the next one.
func TestDeviceHold_ApproveDropsAnUnusedHold(t *testing.T) {
	s := New()
	defer s.Close()
	s.Approve()
	cid, csec := registerClient(t, s)
	postDeviceControl(t, s, "hold=1")
	postDeviceControl(t, s, "approve=1")
	dev := startDeviceAuth(t, s, cid, csec, "https://fake.awsapps.com/start")
	if status, _ := createToken(t, s, cid, csec, dev["deviceCode"].(string)); status != http.StatusOK {
		t.Fatalf("CreateToken = %d, want 200", status)
	}
	if code := postDeviceControl(t, s, "hold=1&approve=1"); code != http.StatusBadRequest {
		t.Errorf("both = %d, want 400", code)
	}
}
