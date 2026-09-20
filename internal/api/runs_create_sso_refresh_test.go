// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// 0.7.7: the click IS the check. A captured AWS SSO session whose access token
// has lapsed but whose refresh token still lives used to be ADMITTED at create
// (refresh=false, "dispatch renews it") — and when AWS had already retired that
// refresh token, the run failed at dispatch, on its own page, after the person
// had been told it launched. The 0.7.6 field report's shape. The real launch
// now redeems the token at create: a renewal AWS refuses (spent) is refused
// there with the class the console's launch door acts on; one AWS did not
// answer is refused with "launch again in a moment" and NO class, because a
// sign-in repairs nothing about an outage. Preflight (Review) stays a dry run.

// expiredRenewableMemberBlob stores, for pinTestMember, a session whose access
// token lapsed a minute ago and whose refresh token and client registration are
// live — matching the pinned pair, so the stored-identity arm stays out of it.
func expiredRenewableMemberBlob(t *testing.T, sec *scopedSecrets, account, role string) {
	t.Helper()
	raw, err := json.Marshal(awsSSOBlob{
		AccessToken: "access-" + pinTestMember, RefreshToken: "refresh-" + pinTestMember,
		ClientID: "client-id", ClientSecret: "client-secret",
		StartURL: "https://acme.awsapps.com/start", Region: "us-east-1",
		AccountID: account, RoleName: role,
		ExpiresAt:             awsSSOTestFixedNow.Add(-time.Minute),
		CapturedAt:            awsSSOTestFixedNow.Add(-time.Hour),
		RegistrationExpiresAt: awsSSOTestFixedNow.Add(90 * 24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("marshal blob: %v", err)
	}
	if perr := sec.For(pinTestMember).Put(context.Background(), harnessCredSecretName(awsSSOProvider), raw); perr != nil {
		t.Fatalf("put blob: %v", perr)
	}
}

func createGate(t *testing.T, srv *Server, refresh bool) (bool, *httptest.ResponseRecorder) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := createRunRequest{Agent: modelAccessAgent, Task: "ship it"}
	ok := srv.enforceCreateLLMMechanism(context.Background(), rec, req, types.RunPolicySpec{}, nil, pinTestMember, nil, refresh)
	return ok, rec
}

func TestCreateRun_RedeemsAnExpiredSessionAtTheClick(t *testing.T) {
	srv, sec, st, _ := pinDispatchSrv(t)
	st.sc = pinnedPerUserRoster("111111111111", "BedrockRunner")
	expiredRenewableMemberBlob(t, sec, "111111111111", "BedrockRunner")
	calls := fakeOIDC(t, func(w http.ResponseWriter, _ map[string]string, _ int) {
		_ = json.NewEncoder(w).Encode(map[string]any{"accessToken": "fresh", "refreshToken": "rotated", "expiresIn": 3600})
	})
	ok, rec := createGate(t, srv, true)
	if !ok {
		t.Fatalf("create refused a renewable session: %d %s", rec.Code, rec.Body.String())
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("CreateToken calls = %d; want 1 — the real launch redeems at the click", got)
	}
	stored, found, err := srv.readAWSSSOBlob(context.Background(), awsSSOScope{owner: pinTestMember, perUser: true})
	if err != nil || !found || stored.AccessToken != "fresh" {
		t.Errorf("stored blob after the click: found=%v err=%v access=%q; want the renewed token persisted", found, err, stored.AccessToken)
	}
}

func TestCreateRun_ASpentSessionIsRefusedAtTheClickWithTheClass(t *testing.T) {
	srv, sec, st, _ := pinDispatchSrv(t)
	st.sc = pinnedPerUserRoster("111111111111", "BedrockRunner")
	expiredRenewableMemberBlob(t, sec, "111111111111", "BedrockRunner")
	fakeOIDC(t, func(w http.ResponseWriter, _ map[string]string, _ int) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid_grant", "error_description": "refresh token is invalid"})
	})
	ok, rec := createGate(t, srv, true)
	if ok || rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("admitted=%v code=%d; want a 422 at create for a spent session (body %s)", ok, rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "can no longer be renewed") {
		t.Errorf("body = %q; want the spent sentence", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"reason":"model_credential"`) {
		t.Errorf("body = %q; want the class — a sign-in repairs this one", rec.Body.String())
	}
}

func TestCreateRun_AnUnansweredRenewalIsRefusedWithoutTheClass(t *testing.T) {
	srv, sec, st, _ := pinDispatchSrv(t)
	st.sc = pinnedPerUserRoster("111111111111", "BedrockRunner")
	expiredRenewableMemberBlob(t, sec, "111111111111", "BedrockRunner")
	fakeOIDC(t, func(w http.ResponseWriter, _ map[string]string, _ int) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	ok, rec := createGate(t, srv, true)
	if ok || rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("admitted=%v code=%d; want a 422 — the token in hand has lapsed and AWS did not answer (body %s)", ok, rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "launch again in a moment") {
		t.Errorf("body = %q; want the unavailable sentence", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"reason"`) {
		t.Errorf("body = %q; an outage carries NO class — the sign-in dialog must not open over it", rec.Body.String())
	}
}

// The GATE with refresh=false — the value preflight.go's handler and the
// create-path advisory pass by construction (a literal at each call site).
func TestCreateRun_GateWithRefreshFalseNeverRedeems(t *testing.T) {
	srv, sec, st, _ := pinDispatchSrv(t)
	st.sc = pinnedPerUserRoster("111111111111", "BedrockRunner")
	expiredRenewableMemberBlob(t, sec, "111111111111", "BedrockRunner")
	calls := fakeOIDC(t, func(w http.ResponseWriter, _ map[string]string, _ int) {
		t.Error("preflight redeemed the refresh token — Review is a dry run over a one-use rotating credential")
		w.WriteHeader(http.StatusInternalServerError)
	})
	ok, rec := createGate(t, srv, false)
	if !ok {
		t.Fatalf("preflight refused a renewable session: %d %s", rec.Code, rec.Body.String())
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("CreateToken calls = %d; want 0", got)
	}
}
