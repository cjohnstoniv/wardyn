// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package apie2e

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// TestAuth_NoToken_401 drives the admin surface (GET /api/v1/runs via the SDK's
// ListRuns) with NO bearer token and asserts the server fails closed with 401.
// This is the black-box analogue of internal/api's TestAdminAuthRequired, but
// over a real httptest server through the public SDK.
func TestAuth_NoToken_401(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	// A client with an empty token sends "Authorization: Bearer " — the server
	// must reject it.
	noTok := client.New(h.srv.URL, "")
	_, err := noTok.ListRuns(context.Background())
	assertAPIStatus(t, err, http.StatusUnauthorized)
}

// TestAuth_WrongToken_401 asserts a non-matching bearer is rejected (constant-
// time compare on the server). The SDK returns *client.APIError with Status 401.
func TestAuth_WrongToken_401(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	wrong := client.New(h.srv.URL, "not-the-admin-token")
	_, err := wrong.ListRuns(context.Background())
	assertAPIStatus(t, err, http.StatusUnauthorized)
}

// TestAuth_AdminToken_200 asserts the correctly-tokened SDK is admitted to the
// admin surface (200 + a decodable body). ListRuns returns the global run list;
// it may be non-empty (the DB is shared) — we assert only that the call
// succeeded and decoded.
func TestAuth_AdminToken_200(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	runs, err := h.sdk.ListRuns(context.Background())
	if err != nil {
		t.Fatalf("admin ListRuns: %v", err)
	}
	// A successful decode yields a non-nil (possibly empty) slice; the assertion
	// is simply that no error occurred and the SDK decoded the 200 body.
	_ = runs
}

// TestAuth_Healthz_Open asserts /healthz is ungated: reachable with NO token and
// returns the identity provider name. The SDK has no healthz method (it is not
// part of the governed surface), so we hit it with a tokenless raw GET — a true
// black-box check of the open liveness endpoint.
func TestAuth_Healthz_Open(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, h.srv.URL+"/healthz", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode healthz: %v", err)
	}
	if body["identity_provider"] != "embedded" {
		t.Errorf("identity_provider = %v, want embedded", body["identity_provider"])
	}
	if body["trust_domain"] != trustDomain {
		t.Errorf("trust_domain = %v, want %s", body["trust_domain"], trustDomain)
	}
}

// assertAPIStatus asserts err is a *client.APIError carrying the wanted HTTP
// status. The SDK maps every non-2xx response to *APIError, so this is the
// black-box way to assert a status code through the public client.
func assertAPIStatus(t *testing.T, err error, want int) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected APIError with status %d, got nil error", want)
	}
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *client.APIError, got %T: %v", err, err)
	}
	if apiErr.Status != want {
		t.Fatalf("status = %d, want %d (body=%s)", apiErr.Status, want, apiErr.Body)
	}
}
