// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// refreshRowsOf is every harness.credential.refresh row srv recorded, with its outcome.
func refreshRowsOf(t *testing.T, srv *Server) []map[string]any {
	t.Helper()
	var rows []map[string]any
	for _, ev := range srv.cfg.Audit.(*recRecorder).snapshot() {
		if ev.Action != "harness.credential.refresh" {
			continue
		}
		d := map[string]any{}
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			t.Fatal(err)
		}
		d["outcome"] = ev.Outcome
		rows = append(rows, d)
	}
	return rows
}

// Review's preflight renews an expired AWS sign-in as create does, so a renewal AWS refuses gives
// both doors the same refusal. Preflight's renewal writes the one refresh row carrying
// trigger: preflight and marks the token refused; the policy preview renews nothing.
func TestPreflightRenewal_RefusedRenewalAnswersAsCreate(t *testing.T) {
	srv := createRenewalFixture(t)
	calls := fakeOIDC(t, func(w http.ResponseWriter, _ map[string]string, _ int) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid_request", "error_description": "Invalid request"})
	})
	token := providerAdminToken(srv, createRenewalOwner)

	do(t, srv, http.MethodPost, "/api/v1/runs/policy-preview", token, createRenewalBody)
	if n, rows := calls.Load(), refreshRowsOf(t, srv); n != 0 || len(rows) != 0 {
		t.Fatalf("the policy preview renewed: %d CreateToken calls, refresh rows %v", n, rows)
	}

	pre := do(t, srv, http.MethodPost, "/api/v1/runs/preflight", token, createRenewalBody)
	var pb errorBody
	if err := json.Unmarshal(pre.Body.Bytes(), &pb); err != nil || pre.Code != http.StatusUnprocessableEntity || pb.Reason != llmRefusalAuditReason {
		t.Fatalf("preflight = %d %s, want the 422 model_credential door", pre.Code, pre.Body.String())
	}
	rows := refreshRowsOf(t, srv)
	if calls.Load() != 1 || len(rows) != 1 || rows[0]["outcome"] != "failure" || rows[0]["trigger"] != "preflight" {
		t.Fatalf("after preflight: %d CreateToken calls, refresh rows %v, want one failure with trigger preflight", calls.Load(), rows)
	}
	stored, _ := createRenewalStored(t, srv)
	if _, marked := srv.awsSSORefusedFor(stored); !marked {
		t.Error("preflight's refused renewal left no refused mark")
	}

	res := postCreate(t, srv)
	var cb errorBody
	if err := json.Unmarshal([]byte(res.body), &cb); err != nil || res.code != pre.Code ||
		cb.Error != pb.Error || cb.Reason != pb.Reason || cb.Provider != pb.Provider || cb.Kind != pb.Kind {
		t.Fatalf("create = %d %s\nwant preflight's answer %d %+v", res.code, res.body, pre.Code, pb)
	}
	if n := runRowCount(srv); n != 0 {
		t.Errorf("run rows = %d, want none", n)
	}
	preflightRows := 0
	for _, row := range refreshRowsOf(t, srv) {
		if row["trigger"] == "preflight" {
			preflightRows++
		}
	}
	if preflightRows != 1 {
		t.Errorf("refresh rows with trigger preflight = %d, want only preflight's own", preflightRows)
	}
}

// A renewable session is renewed at Review, with its success row naming the trigger, and the
// launch that follows has nothing left to renew.
func TestPreflightRenewal_RenewsOnceThenCreateRenewsNothing(t *testing.T) {
	srv := createRenewalFixture(t)
	calls := renewedOIDC(t)
	token := providerAdminToken(srv, createRenewalOwner)

	if w := do(t, srv, http.MethodPost, "/api/v1/runs/preflight", token, createRenewalBody); w.Code != http.StatusOK {
		t.Fatalf("preflight = %d %s, want 200", w.Code, w.Body.String())
	}
	rows := refreshRowsOf(t, srv)
	if calls.Load() != 1 || len(rows) != 1 || rows[0]["outcome"] != "success" || rows[0]["trigger"] != "preflight" {
		t.Fatalf("after preflight: %d CreateToken calls, refresh rows %v, want one success with trigger preflight", calls.Load(), rows)
	}
	if got, _ := createRenewalStored(t, srv); got.RefreshToken != "rotated-refresh-token-abcdefghij" {
		t.Fatalf("stored pair = %+v, want the one preflight renewed", got)
	}
	if res := postCreate(t, srv); res.code != http.StatusCreated {
		t.Fatalf("create = %d %s, want 201", res.code, res.body)
	}
	if calls.Load() != 1 || len(refreshRowsOf(t, srv)) != 1 {
		t.Errorf("create renewed again: %d CreateToken calls, refresh rows %v", calls.Load(), refreshRowsOf(t, srv))
	}
}

// A session AWS refused to renew reads expiring until its access token expires, inside the refresh
// skew or not, then expired_signin; its deadline is that expiry.
func TestAWSSSOCredentialState_RefusedOutsideTheSkewIsExpiring(t *testing.T) {
	now := awsSSOTestFixedNow
	b := awsSSOBlob{RefreshToken: "rt", ClientID: "cid", ClientSecret: "csec",
		ExpiresAt: now.Add(time.Hour), RegistrationExpiresAt: now.Add(90 * 24 * time.Hour)}
	if b.needsRefresh(now, awsSSORefreshSkew) {
		t.Fatal("the blob must be outside the refresh skew")
	}
	if got := awsSSOCredentialState(b, true, false, true, now, awsSSORefreshSkew); got != modelAccessExpiring {
		t.Errorf("refused outside the skew = %q, want expiring", got)
	}
	if got := modelAccessDeadline(b, true, false, true, now, awsSSORefreshSkew); got != b.ExpiresAt.UTC().Format(time.RFC3339) {
		t.Errorf("refused deadline = %q, want the access token's own expiry", got)
	}
	b.ExpiresAt = now.Add(-time.Minute)
	if got := awsSSOCredentialState(b, true, false, true, now, awsSSORefreshSkew); got != modelAccessExpiredSignin {
		t.Errorf("refused and expired = %q, want expired_signin", got)
	}
}
