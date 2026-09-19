// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// putFailingSecrets is the store whose Put fails AFTER a redeem has already
// succeeded — the one case where a refresh cannot be undone.
type putFailingSecrets struct{ secretstore.Store }

func (p *putFailingSecrets) Put(context.Context, string, []byte) error {
	return errors.New("secret store is wedged")
}

// fakeOIDC stands up an httptest SSO-OIDC /token endpoint and points
// awsSSOTokenURL at it for the duration of the test. handler decides the answer;
// calls counts the attempts, so the retry rules are assertable rather than
// asserted. Returns the call counter.
func fakeOIDC(t *testing.T, handler func(w http.ResponseWriter, body map[string]string, call int)) *atomic.Int32 {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(calls.Add(1))
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q; want application/json", ct)
		}
		if r.Header.Get("Authorization") != "" {
			t.Error("CreateToken must be sent UNSIGNED — an Authorization header means something signed it")
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		handler(w, body, n)
	}))
	t.Cleanup(srv.Close)
	prev := awsSSOTokenURL
	awsSSOTokenURL = func(string) string { return srv.URL + "/token" }
	t.Cleanup(func() { awsSSOTokenURL = prev })
	return &calls
}

// memAudit collects the audit rows a test's server emits.
type memAudit struct {
	mu   sync.Mutex
	rows []types.AuditEvent
}

func (a *memAudit) Record(_ context.Context, ev types.AuditEvent) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.rows = append(a.rows, ev)
	return nil
}

func (a *memAudit) find(action string) []types.AuditEvent {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []types.AuditEvent
	for _, ev := range a.rows {
		if ev.Action == action {
			out = append(out, ev)
		}
	}
	return out
}

// ssoRefreshServer is the shared fixture: Bedrock configured, NO other Bedrock
// credential (so the SSO lane is the only one that can fire and a fall-through is
// visible as not-ready), a captured blob whose access token expired a minute ago,
// a fixed clock and an audit sink.
func ssoRefreshServer(t *testing.T) (*Server, *memAudit, awsSSOBlob) {
	t.Helper()
	audit := &memAudit{}
	s := &Server{cfg: Config{
		BedrockRegion: "us-east-1",
		BedrockModel:  "us.anthropic.claude-sonnet-4-5-20250929-v1:0",
		Secrets:       &memSecrets{m: map[string][]byte{}},
		MaskRegistry:  secretmask.NewRegistry(),
		Now:           func() time.Time { return awsSSOTestFixedNow },
		Audit:         audit,
	}}
	blob := putAWSSSOBlob(t, s, awsSSOTestFixedNow.Add(-time.Minute))
	return s, audit, blob
}

// storedSSOBlob reads back what the store now holds.
func storedSSOBlob(t *testing.T, s *Server) awsSSOBlob {
	t.Helper()
	b, found, err := s.readAWSSSOBlob(context.Background(), awsSSOScope{})
	if err != nil || !found {
		t.Fatalf("read stored SSO blob: found=%v err=%v", found, err)
	}
	return b
}

// TestAWSSSORefresh_RotatesAndRepersists is the happy path: dispatch redeems the
// refresh token, the new access token lands in the sandbox cache the run gets,
// the ROTATED refresh token is persisted (the whole reason the control plane owns
// this), and a success row names what happened.
func TestAWSSSORefresh_RotatesAndRepersists(t *testing.T) {
	s, audit, blob := ssoRefreshServer(t)
	calls := fakeOIDC(t, func(w http.ResponseWriter, body map[string]string, _ int) {
		if body["grantType"] != "refresh_token" {
			t.Errorf("grantType = %q; want refresh_token", body["grantType"])
		}
		if body["refreshToken"] != blob.RefreshToken || body["clientId"] != blob.ClientID || body["clientSecret"] != blob.ClientSecret {
			t.Errorf("request did not carry the stored registration: %v", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"accessToken": "fresh-access-token-abcdefghij", "expiresIn": 3600,
			"refreshToken": "rotated-refresh-token-abcdefghij",
		})
	})

	ba := s.resolveBedrockAuth(context.Background(), "claude-code", false, true /* modelRun */, true /* refresh */, nil, awsSSOScope{})
	if !ba.ready || !ba.ssoInject || ba.ssoRefreshFailure != "" {
		t.Fatalf("ready=%v ssoInject=%v failure=%q; want a renewed, ready SSO lane", ba.ready, ba.ssoInject, ba.ssoRefreshFailure)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("CreateToken calls = %d; want 1", got)
	}
	// The cache the SANDBOX gets must be built from the REFRESHED blob, not from
	// the pre-refresh read.
	files := decodeSSOFiles(t, ba.env[awsSSOConfigEnvVar])
	var cache map[string]any
	if err := json.Unmarshal([]byte(files[".aws/sso/cache/"+awsSSOCacheFileName(awsSSOProfileName)+".json"]), &cache); err != nil {
		t.Fatalf("SSO cache file is not valid JSON: %v", err)
	}
	if cache["accessToken"] != "fresh-access-token-abcdefghij" {
		t.Errorf("cache accessToken = %v; want the REFRESHED token (the cache was built from the stale read)", cache["accessToken"])
	}
	stored := storedSSOBlob(t, s)
	if stored.AccessToken != "fresh-access-token-abcdefghij" || stored.RefreshToken != "rotated-refresh-token-abcdefghij" {
		t.Fatalf("stored blob did not take the rotated pair: %+v", stored)
	}
	if want := awsSSOTestFixedNow.Add(time.Hour); !stored.ExpiresAt.Equal(want) {
		t.Errorf("stored ExpiresAt = %v; want now+expiresIn (%v)", stored.ExpiresAt, want)
	}
	rows := audit.find("harness.credential.refresh")
	if len(rows) != 1 || rows[0].Outcome != "success" {
		t.Fatalf("audit rows = %+v; want one success harness.credential.refresh row", rows)
	}
	if got := s.metrics.ssoRefreshOutcomes[ssoRefreshOutcomeSuccess]; got != 1 {
		t.Errorf("wardyn_sso_refresh_total{outcome=success} = %d, want 1", got)
	}
	var data map[string]any
	if err := json.Unmarshal(rows[0].Data, &data); err != nil {
		t.Fatalf("audit data: %v", err)
	}
	for _, k := range []string{"owner", "expires_at", "registration_expires_at", "rotated"} {
		if _, ok := data[k]; !ok {
			t.Errorf("audit data missing %q: %v", k, data)
		}
	}
	if data["rotated"] != true {
		t.Errorf("rotated = %v; want true", data["rotated"])
	}
	// Both new values must be masked globally — one credential serves every later run.
	snap := s.cfg.MaskRegistry.Snapshot(uuid.Nil)
	var sawAccess, sawRefresh bool
	for _, v := range snap {
		switch string(v) {
		case "fresh-access-token-abcdefghij":
			sawAccess = true
		case "rotated-refresh-token-abcdefghij":
			sawRefresh = true
		}
	}
	if !sawAccess || !sawRefresh {
		t.Errorf("mask registry missing the renewed values (access=%v refresh=%v)", sawAccess, sawRefresh)
	}
}

// TestAWSSSORefresh_AbsentRefreshTokenKeepsTheOldOne: CreateToken documents
// refreshToken as present "if present". An absent one means KEEP the stored
// token — blanking it would turn a renewable credential into a dead one on the
// very call that renewed it.
func TestAWSSSORefresh_AbsentRefreshTokenKeepsTheOldOne(t *testing.T) {
	s, _, blob := ssoRefreshServer(t)
	fakeOIDC(t, func(w http.ResponseWriter, _ map[string]string, _ int) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"accessToken": "fresh-access-token-abcdefghij", "expiresIn": 3600,
		})
	})

	ba := s.resolveBedrockAuth(context.Background(), "claude-code", false, true, true, nil, awsSSOScope{})
	if !ba.ssoInject {
		t.Fatalf("ssoInject = false; want the renewed lane (failure=%q)", ba.ssoRefreshFailure)
	}
	stored := storedSSOBlob(t, s)
	if stored.RefreshToken != blob.RefreshToken {
		t.Fatalf("RefreshToken = %q; want the ORIGINAL %q kept", stored.RefreshToken, blob.RefreshToken)
	}
	if stored.AccessToken != "fresh-access-token-abcdefghij" {
		t.Errorf("AccessToken = %q; want the renewed one", stored.AccessToken)
	}
}

// TestAWSSSORefresh_InvalidGrantMarksDeadAndNeverFallsThroughToAPIKey is C2's
// finding at its root: a spent refresh token must not become a silent switch to
// a DIFFERENT auth mechanism. The lane goes not-ready and carries the sentence,
// the credential is marked dead FOR THAT TOKEN (so the next dispatch does not
// redeem it again), and nothing here hands the run an Anthropic api-key.
func TestAWSSSORefresh_InvalidGrantMarksDeadAndNeverFallsThroughToAPIKey(t *testing.T) {
	s, audit, blob := ssoRefreshServer(t)
	calls := fakeOIDC(t, func(w http.ResponseWriter, _ map[string]string, _ int) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid_grant", "error_description": "refresh token is invalid"})
	})

	ba := s.resolveBedrockAuth(context.Background(), "claude-code", false, true, true, nil, awsSSOScope{})
	if ba.ready || ba.ssoInject {
		t.Fatalf("ready=%v ssoInject=%v; want both false on a spent credential", ba.ready, ba.ssoInject)
	}
	if ba.ssoRefreshFailure != awsSSORefreshSpentRefusal(false) {
		t.Fatalf("ssoRefreshFailure = %q; want the spent sentence", ba.ssoRefreshFailure)
	}
	for k := range ba.env {
		if strings.Contains(k, "ANTHROPIC_API_KEY") {
			t.Errorf("env carries %q — a dead SSO session must never become an api-key run", k)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("CreateToken calls = %d; want 1 (a spent credential is never retried)", got)
	}
	rows := audit.find("harness.credential.refresh")
	if len(rows) != 1 || rows[0].Outcome != "failure" {
		t.Fatalf("audit rows = %+v; want one failure harness.credential.refresh row", rows)
	}
	if got := s.metrics.ssoRefreshOutcomes[ssoRefreshOutcomeSpent]; got != 1 {
		t.Errorf("wardyn_sso_refresh_total{outcome=spent} = %d, want 1", got)
	}
	// Dead-marked, keyed to the token: a SECOND dispatch does not redeem again.
	if !s.awsSSOTokenSpent(awsSSOTokenFingerprint(blob.RefreshToken)) {
		t.Fatal("the spent refresh token was not dead-marked")
	}
	ba2 := s.resolveBedrockAuth(context.Background(), "claude-code", false, true, true, nil, awsSSOScope{})
	if ba2.ssoRefreshFailure != awsSSORefreshSpentRefusal(false) {
		t.Errorf("second dispatch failure = %q; want the spent sentence", ba2.ssoRefreshFailure)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("CreateToken calls after the second dispatch = %d; want 1 (the dead mark short-circuits)", got)
	}
	// Keyed to the TOKEN, not the credential: a fresh capture is not pre-dead.
	next := blob
	next.RefreshToken = "recaptured-refresh-token-abcdefghij"
	if s.awsSSOTokenSpent(awsSSOTokenFingerprint(next.RefreshToken)) {
		t.Error("a newly captured refresh token reads spent — the mark is keyed to the credential, not the token")
	}
}

// TestAWSSSORefresh_TwoTransientFailures_AttemptsAndBothErrors is Finding 5:
// the one retry already existed — make it LEGIBLE. Two transient failures (a
// network story, not a credential one) must report attempts=2 with BOTH
// attempts' errors in the failure row.
func TestAWSSSORefresh_TwoTransientFailures_AttemptsAndBothErrors(t *testing.T) {
	s, audit, _ := ssoRefreshServer(t)
	calls := fakeOIDC(t, func(w http.ResponseWriter, _ map[string]string, call int) {
		w.WriteHeader(http.StatusBadRequest)
		code := "slow_down"
		if call == 2 {
			code = "internal_failure"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"error": code})
	})

	ba := s.resolveBedrockAuth(context.Background(), "claude-code", false, true, true, nil, awsSSOScope{})
	if ba.ssoInject {
		t.Fatal("ssoInject = true after two transient failures; want the lane not ready")
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("CreateToken calls = %d, want 2", got)
	}
	rows := audit.find("harness.credential.refresh")
	if len(rows) != 1 || rows[0].Outcome != "failure" {
		t.Fatalf("audit rows = %+v, want one failure row", rows)
	}
	var data struct {
		Attempts int    `json:"attempts"`
		Error    string `json:"error"`
		Spent    bool   `json:"spent"`
	}
	if err := json.Unmarshal(rows[0].Data, &data); err != nil {
		t.Fatalf("decode audit data: %v", err)
	}
	if data.Attempts != 2 {
		t.Errorf("attempts = %d, want 2", data.Attempts)
	}
	if data.Spent {
		t.Error("two transient failures must not dead-mark the credential")
	}
	if !strings.Contains(data.Error, "slow_down") || !strings.Contains(data.Error, "internal_failure") {
		t.Errorf("error = %q, want BOTH attempts' errors joined", data.Error)
	}
}

// TestAWSSSORefresh_TransientThenInvalidGrant_SpentWithAttempts: an EOF then
// invalid_grant already says spent:true — this pins attempts:2 sitting beside
// it, so a reader can tell "one dropped packet, then AWS said no" from "AWS
// said no twice".
func TestAWSSSORefresh_TransientThenInvalidGrant_SpentWithAttempts(t *testing.T) {
	s, audit, blob := ssoRefreshServer(t)
	calls := fakeOIDC(t, func(w http.ResponseWriter, _ map[string]string, call int) {
		w.WriteHeader(http.StatusBadRequest)
		code := "slow_down"
		if call == 2 {
			code = "invalid_grant"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"error": code})
	})

	ba := s.resolveBedrockAuth(context.Background(), "claude-code", false, true, true, nil, awsSSOScope{})
	if ba.ready || ba.ssoInject {
		t.Fatalf("ready=%v ssoInject=%v; want both false on a spent credential", ba.ready, ba.ssoInject)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("CreateToken calls = %d, want 2 (the retry ran before the token was known spent)", got)
	}
	if !s.awsSSOTokenSpent(awsSSOTokenFingerprint(blob.RefreshToken)) {
		t.Fatal("the spent refresh token was not dead-marked")
	}
	rows := audit.find("harness.credential.refresh")
	if len(rows) != 1 || rows[0].Outcome != "failure" {
		t.Fatalf("audit rows = %+v, want one failure row", rows)
	}
	var data struct {
		Attempts int  `json:"attempts"`
		Spent    bool `json:"spent"`
	}
	if err := json.Unmarshal(rows[0].Data, &data); err != nil {
		t.Fatalf("decode audit data: %v", err)
	}
	if data.Attempts != 2 || !data.Spent {
		t.Errorf("data = %+v, want attempts=2 spent=true", data)
	}
}

// TestAWSSSORefresh_FirstAttemptSuccess_NoAttemptsFieldOnSuccessRow pins the
// other side of the same design decision: a first-attempt success carries NO
// attempts field — only the failure row needs to say how many tries it took.
func TestAWSSSORefresh_FirstAttemptSuccess_NoAttemptsFieldOnSuccessRow(t *testing.T) {
	s, audit, _ := ssoRefreshServer(t)
	fakeOIDC(t, func(w http.ResponseWriter, _ map[string]string, _ int) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"accessToken": "fresh-access-token-abcdefghij", "expiresIn": 3600,
		})
	})

	ba := s.resolveBedrockAuth(context.Background(), "claude-code", false, true, true, nil, awsSSOScope{})
	if !ba.ssoInject {
		t.Fatalf("ssoInject = false; want a clean renewal (failure=%q)", ba.ssoRefreshFailure)
	}
	rows := audit.find("harness.credential.refresh")
	if len(rows) != 1 || rows[0].Outcome != "success" {
		t.Fatalf("audit rows = %+v, want one success row", rows)
	}
	if strings.Contains(string(rows[0].Data), "attempts") {
		t.Errorf("a first-attempt success carries no attempts field; data=%s", rows[0].Data)
	}
}

// TestAWSSSORefresh_RetriedSuccess_AttemptsTwoOnSuccessRow is N1: a success
// row carries attempts:2 when the retry is what made it succeed — "took two
// tries" is as much a network signal on a success row as on a failure one.
func TestAWSSSORefresh_RetriedSuccess_AttemptsTwoOnSuccessRow(t *testing.T) {
	s, audit, _ := ssoRefreshServer(t)
	fakeOIDC(t, func(w http.ResponseWriter, _ map[string]string, call int) {
		if call == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "slow_down"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"accessToken": "fresh-access-token-abcdefghij", "expiresIn": 3600,
		})
	})

	ba := s.resolveBedrockAuth(context.Background(), "claude-code", false, true, true, nil, awsSSOScope{})
	if !ba.ssoInject {
		t.Fatalf("ssoInject = false; want a clean renewal on the retry (failure=%q)", ba.ssoRefreshFailure)
	}
	rows := audit.find("harness.credential.refresh")
	if len(rows) != 1 || rows[0].Outcome != "success" {
		t.Fatalf("audit rows = %+v, want one success row", rows)
	}
	var data struct {
		Attempts int `json:"attempts"`
	}
	if err := json.Unmarshal(rows[0].Data, &data); err != nil {
		t.Fatalf("decode audit data: %v", err)
	}
	if data.Attempts != 2 {
		t.Errorf("attempts = %d, want 2 (the retry is what succeeded)", data.Attempts)
	}
	if got := s.metrics.ssoRefreshOutcomes[ssoRefreshOutcomeSuccess]; got != 1 {
		t.Errorf("wardyn_sso_refresh_total{outcome=success} = %d, want 1", got)
	}
}

// TestAWSSSORefresh_SlowDownRetriesOnceAndNeverDeadMarks is the row a
// status-code classifier fails: slow_down arrives as HTTP 400, exactly like
// invalid_grant. It must get ONE retry, fail THIS run visibly, leave the stored
// blob untouched, and NOT dead-mark — the next dispatch redeems normally.
func TestAWSSSORefresh_SlowDownRetriesOnceAndNeverDeadMarks(t *testing.T) {
	s, audit, blob := ssoRefreshServer(t)
	throttled := true
	calls := fakeOIDC(t, func(w http.ResponseWriter, _ map[string]string, _ int) {
		if throttled {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "slow_down"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"accessToken": "fresh-access-token-abcdefghij", "expiresIn": 3600,
			"refreshToken": "rotated-refresh-token-abcdefghij",
		})
	})

	ba := s.resolveBedrockAuth(context.Background(), "claude-code", false, true, true, nil, awsSSOScope{})
	if ba.ssoInject {
		t.Fatal("ssoInject = true after a throttled renewal; want the lane not ready")
	}
	if ba.ssoRefreshFailure != awsSSORefreshUnavailableSentence {
		t.Fatalf("ssoRefreshFailure = %q; want the TRANSIENT sentence, not the spent one", ba.ssoRefreshFailure)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("CreateToken calls = %d; want 2 (one retry, then give up)", got)
	}
	if s.awsSSOTokenSpent(awsSSOTokenFingerprint(blob.RefreshToken)) {
		t.Fatal("a throttle dead-marked the credential — one AWS throttle would sign the whole fleet out")
	}
	if stored := storedSSOBlob(t, s); stored.AccessToken != blob.AccessToken || stored.RefreshToken != blob.RefreshToken {
		t.Fatalf("the stored blob was modified by a failed renewal: %+v", stored)
	}
	if rows := audit.find("harness.credential.refresh"); len(rows) != 1 || rows[0].Outcome != "failure" {
		t.Fatalf("audit rows = %+v; want one failure row", rows)
	}
	if got := s.metrics.ssoRefreshOutcomes[ssoRefreshOutcomeUnavailable]; got != 1 {
		t.Errorf("wardyn_sso_refresh_total{outcome=unavailable} = %d, want 1 (the access token was already expired)", got)
	}

	// The next dispatch redeems normally once AWS answers.
	throttled = false
	ba2 := s.resolveBedrockAuth(context.Background(), "claude-code", false, true, true, nil, awsSSOScope{})
	if !ba2.ssoInject || ba2.ssoRefreshFailure != "" {
		t.Fatalf("second dispatch: ssoInject=%v failure=%q; want a clean renewal", ba2.ssoInject, ba2.ssoRefreshFailure)
	}
	if stored := storedSSOBlob(t, s); stored.RefreshToken != "rotated-refresh-token-abcdefghij" {
		t.Errorf("second dispatch did not persist the rotated token: %+v", stored)
	}
}

// TestAWSSSORefresh_PersistFailureStillServesTheRun: the redeem succeeded, so the
// old pair is already spent at AWS — refusing the run would throw away a
// credential we hold. Serve this run from memory and audit the persist failure.
func TestAWSSSORefresh_PersistFailureStillServesTheRun(t *testing.T) {
	s, audit, _ := ssoRefreshServer(t)
	calls := fakeOIDC(t, func(w http.ResponseWriter, _ map[string]string, _ int) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"accessToken": "fresh-access-token-abcdefghij", "expiresIn": 3600,
			"refreshToken": "rotated-refresh-token-abcdefghij",
		})
	})
	s.cfg.Secrets = &putFailingSecrets{Store: s.cfg.Secrets}

	ba := s.resolveBedrockAuth(context.Background(), "claude-code", false, true, true, nil, awsSSOScope{})
	if !ba.ssoInject || ba.ssoRefreshFailure != "" {
		t.Fatalf("ssoInject=%v failure=%q; want the run served from the in-memory refreshed blob", ba.ssoInject, ba.ssoRefreshFailure)
	}
	files := decodeSSOFiles(t, ba.env[awsSSOConfigEnvVar])
	if !strings.Contains(files[".aws/sso/cache/"+awsSSOCacheFileName(awsSSOProfileName)+".json"], "fresh-access-token-abcdefghij") {
		t.Error("the sandbox cache does not carry the refreshed access token")
	}
	rows := audit.find("harness.credential.refresh")
	if len(rows) != 1 || rows[0].Outcome != "failure" {
		t.Fatalf("audit rows = %+v; want the persist failure audited", rows)
	}
	if !strings.Contains(string(rows[0].Data), "persist_error") {
		t.Errorf("audit data does not name the persist failure: %s", rows[0].Data)
	}
	// The old pair is spent at AWS whatever the store says (0.7.7, review-2):
	// the next pass over the STORED (old) blob — the create caller's dispatch,
	// or the next launch — is refused as spent without another token round trip.
	again := s.resolveBedrockAuth(context.Background(), "claude-code", false, true, true, nil, awsSSOScope{})
	if again.ssoInject || !strings.Contains(again.ssoRefreshFailure, "can no longer be renewed") {
		t.Errorf("second pass: ssoInject=%v failure=%q; want the spent refusal off the stored old pair", again.ssoInject, again.ssoRefreshFailure)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("CreateToken calls = %d; want 1 — the spent mark answers the second pass", got)
	}
}

// TestAWSSSORefresh_SingleFlightRedeemsOnce: two concurrent dispatches of the
// same principal must produce ONE redeem. The second takes the owner lock,
// re-reads the blob the first persisted, finds it fresh and skips — without the
// re-read INSIDE the lock it would redeem the stale token, get invalid_grant and
// clobber the pair just persisted.
func TestAWSSSORefresh_SingleFlightRedeemsOnce(t *testing.T) {
	s, _, _ := ssoRefreshServer(t)
	release := make(chan struct{})
	calls := fakeOIDC(t, func(w http.ResponseWriter, _ map[string]string, n int) {
		if n == 1 {
			<-release // hold the first flight open so the second must queue on the lock
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"accessToken": "fresh-access-token-abcdefghij", "expiresIn": 3600,
			"refreshToken": "rotated-refresh-token-abcdefghij",
		})
	})

	var wg sync.WaitGroup
	out := make([]bedrockAuth, 2)
	for i := range out {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out[i] = s.resolveBedrockAuth(context.Background(), "claude-code", false, true, true, nil, awsSSOScope{})
		}(i)
	}
	// Both goroutines are now racing for the owner lock; let the holder finish.
	time.Sleep(150 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := calls.Load(); got != 1 {
		t.Fatalf("CreateToken calls = %d; want exactly 1 (per-owner single-flight)", got)
	}
	for i, ba := range out {
		if !ba.ssoInject || ba.ssoRefreshFailure != "" {
			t.Errorf("dispatch %d: ssoInject=%v failure=%q; want both dispatches served", i, ba.ssoInject, ba.ssoRefreshFailure)
		}
	}
	if stored := storedSSOBlob(t, s); stored.RefreshToken != "rotated-refresh-token-abcdefghij" {
		t.Errorf("stored refresh token = %q; the second flight clobbered the rotated pair", stored.RefreshToken)
	}
}

// TestAWSSSORefresh_ZeroRegistrationExpiryCountsAsLive pins the IsZero rule:
// RegistrationExpiresAt is a time.Time under omitempty, which encoding/json never
// omits, so a helper that saw no registration expiry serialises the zero time.
// That must read LIVE — the OIDC response is the oracle — or every credential
// captured by such a helper would be refused renewal.
func TestAWSSSORefresh_ZeroRegistrationExpiryCountsAsLive(t *testing.T) {
	s, _, blob := ssoRefreshServer(t)
	blob.RegistrationExpiresAt = time.Time{}
	storeSSOBlob(t, s, blob)
	calls := fakeOIDC(t, func(w http.ResponseWriter, _ map[string]string, _ int) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"accessToken": "fresh-access-token-abcdefghij", "expiresIn": 3600,
		})
	})

	if blob.registrationLapsed(awsSSOTestFixedNow) {
		t.Fatal("a ZERO RegistrationExpiresAt read as lapsed; want live")
	}
	ba := s.resolveBedrockAuth(context.Background(), "claude-code", false, true, true, nil, awsSSOScope{})
	if !ba.ssoInject || calls.Load() != 1 {
		t.Fatalf("ssoInject=%v calls=%d; want the renewal attempted and the lane ready", ba.ssoInject, calls.Load())
	}
	if b := setupBedrockSSOPresent(t, s); !b {
		t.Error("setupBedrock reports the session absent; a zero registration expiry must count as live there too")
	}
}

// TestAWSSSORefresh_SkewRenewsAhead: a token with MORE than the skew window left
// is used as-is (no call); one inside the window is renewed even though it has
// not expired, so no run boots inside the SDK's own 5-minute refresh window.
func TestAWSSSORefresh_SkewRenewsAhead(t *testing.T) {
	for _, tc := range []struct {
		name      string
		remaining time.Duration
		wantCalls int32
	}{
		{"well inside validity", awsSSORefreshSkew + time.Hour, 0},
		{"inside the skew window", awsSSORefreshSkew - time.Minute, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _ := ssoRefreshServer(t)
			putAWSSSOBlob(t, s, awsSSOTestFixedNow.Add(tc.remaining))
			calls := fakeOIDC(t, func(w http.ResponseWriter, _ map[string]string, _ int) {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"accessToken": "fresh-access-token-abcdefghij", "expiresIn": 3600,
				})
			})
			ba := s.resolveBedrockAuth(context.Background(), "claude-code", false, true, true, nil, awsSSOScope{})
			if !ba.ssoInject {
				t.Fatalf("ssoInject = false (failure=%q)", ba.ssoRefreshFailure)
			}
			if got := calls.Load(); got != tc.wantCalls {
				t.Errorf("CreateToken calls = %d; want %d", got, tc.wantCalls)
			}
		})
	}
}

// TestAWSSSOCacheOmitsTheRefresherFields: ONE refresher per token. Whenever the
// blob carries a refresh token the sandbox cache must NOT, nor the client
// registration the SDK's own refresher would need — otherwise a long run rotates
// the token in-run and the next dispatch's redeem fails invalid_grant.
func TestAWSSSOCacheOmitsTheRefresherFields(t *testing.T) {
	blob := awsSSOBlob{
		AccessToken: "sso-access-token-1234567890", RefreshToken: "sso-refresh-token-1234567890",
		ClientID: "sso-client-id", ClientSecret: "sso-client-secret-1234567890",
		StartURL: "https://example.awsapps.com/start", Region: "us-east-1",
		AccountID: "123456789012", RoleName: "WardynBedrockRole",
		ExpiresAt: awsSSOTestFixedNow.Add(time.Hour),
	}
	var cache map[string]any
	if err := json.Unmarshal([]byte(awsSSOCacheFileContents(blob, false)), &cache); err != nil {
		t.Fatalf("cache is not valid JSON: %v", err)
	}
	for _, k := range []string{"refreshToken", "clientId", "clientSecret"} {
		if _, ok := cache[k]; ok {
			t.Errorf("cache carries %q; the control plane owns the rotating secret, so the sandbox must not", k)
		}
	}
	for _, k := range []string{"accessToken", "expiresAt", "startUrl", "region"} {
		if _, ok := cache[k]; !ok {
			t.Errorf("cache is missing %q — the SDK validates these on load", k)
		}
	}
	// A blob with NOTHING to rotate keeps today's bytes: the registration fields
	// are harmless where no refresh token exists.
	blob.RefreshToken = ""
	if err := json.Unmarshal([]byte(awsSSOCacheFileContents(blob, false)), &cache); err != nil {
		t.Fatalf("cache is not valid JSON: %v", err)
	}
	if cache["clientId"] != "sso-client-id" {
		t.Errorf("clientId = %v; want it kept when there is no refresh token to rotate", cache["clientId"])
	}
}

// TestAWSSSORefresh_AdvisoryNeverRedeems: the dry-run passes must not spend a
// one-use token. resolveRunLLMAccess (the create-path ADVISORY) resolves Bedrock
// with refresh=false, so no CreateToken call is made and the stored blob is
// untouched — while the verdict is still READY. The real launch's gate is the
// one pass at create that DOES redeem (runs_create_sso_refresh_test.go);
// preflight's gate is pinned there too.
func TestAWSSSORefresh_AdvisoryNeverRedeems(t *testing.T) {
	s, _, blob := ssoRefreshServer(t)
	calls := fakeOIDC(t, func(w http.ResponseWriter, _ map[string]string, _ int) {
		t.Error("the advisory redeemed the refresh token — a dry run must never spend a rotating credential")
		w.WriteHeader(http.StatusInternalServerError)
	})

	llm := s.resolveRunLLMAccess(context.Background(), createRunRequest{Agent: "claude-code"},
		types.RunPolicySpec{}, nil, nil, "")
	if llm == nil || !llm.Provisioned {
		t.Fatalf("resolveRunLLMAccess = %+v; want a provisioned Bedrock verdict for an expired-but-renewable session", llm)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("CreateToken calls = %d; want 0", got)
	}
	if stored := storedSSOBlob(t, s); stored.AccessToken != blob.AccessToken || stored.RefreshToken != blob.RefreshToken {
		t.Errorf("the create path mutated the stored credential: %+v", stored)
	}
}

// setupBedrockSSOPresent is a one-line read of the wizard's SSO term.
func setupBedrockSSOPresent(t *testing.T, s *Server) bool {
	t.Helper()
	return s.setupBedrock(context.Background(), nil, awsSSOScope{}).SSOPresent
}

// TestSetupHarnessCreds_RenewableHonoursTheRegistration is the surface the
// setup row reads. Renewable must be the SAME predicate dispatch and
// setupBedrock apply: a refresh token whose OIDC client registration has LAPSED
// renews nothing, so a row computing it off the refresh token alone would render
// the "nothing to do" state for a credential dispatch has already given up on —
// while setupBedrock reported the session absent. The two must not disagree.
func TestSetupHarnessCreds_RenewableHonoursTheRegistration(t *testing.T) {
	for _, tc := range []struct {
		name          string
		registration  time.Time
		refreshToken  string
		spent         bool
		wantRenewable bool
		wantSSOChip   bool
	}{
		{"live registration", awsSSOTestFixedNow.Add(30 * 24 * time.Hour), "sso-refresh-token-1234567890", false, true, true},
		{"zero registration counts as live", time.Time{}, "sso-refresh-token-1234567890", false, true, true},
		{"LAPSED registration renews nothing", awsSSOTestFixedNow.Add(-time.Hour), "sso-refresh-token-1234567890", false, false, false},
		{"no refresh token at all", time.Time{}, "", false, false, false},
		// N2 (0.7.6 review): a LIVE registration alone must not win over a
		// refresh token AWS has already retired — renewable(now) reads true
		// for this blob (refresh token present, registration far out), but
		// nothing redeems it. Both surfaces must consult the spent set.
		{"spent, otherwise live registration", awsSSOTestFixedNow.Add(30 * 24 * time.Hour), "sso-refresh-token-1234567890", true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, blob := ssoRefreshServer(t) // blob's access token expired a minute ago
			blob.RegistrationExpiresAt = tc.registration
			blob.RefreshToken = tc.refreshToken
			storeSSOBlob(t, s, blob)
			if tc.spent {
				s.markAWSSSOTokenSpent(awsSSOTokenFingerprint(tc.refreshToken))
			}

			rows, _, ma := s.setupHarnessCreds(context.Background(), types.SiteConfig{}, awsSSOScope{})
			var row SetupHarness
			for _, r := range rows {
				if r.Provider == awsSSOProvider {
					row = r
				}
			}
			if !row.Captured || !row.Expired {
				t.Fatalf("row = %+v; want a captured, expired AWS SSO row", row)
			}
			if row.Renewable != tc.wantRenewable {
				t.Errorf("Renewable = %v; want %v", row.Renewable, tc.wantRenewable)
			}
			// setupBedrock's SSO term must agree — the whole point of one predicate.
			if got := setupBedrockSSOPresent(t, s); got != tc.wantSSOChip {
				t.Errorf("setupBedrock SSOPresent = %v; want %v — the setup row and the launch gate disagree", got, tc.wantSSOChip)
			}
			// And so must the setup ROW the operator reads.
			check, shown := harnessCredentialCheck(row, ma)
			if !shown {
				t.Fatal("harnessCredentialCheck returned no row for a captured credential")
			}
			wantStatus := "warn"
			if tc.wantRenewable {
				wantStatus = "ok"
			}
			if check.Status != wantStatus {
				t.Errorf("setup row status = %q; want %q (detail=%q fix=%q)", check.Status, wantStatus, check.Detail, check.Fix)
			}
			if !tc.wantRenewable && !strings.Contains(check.Fix, "AWS SSO login") {
				t.Errorf("setup row Fix = %q; an unrenewable session must send the operator back to the login", check.Fix)
			}
		})
	}
}

// TestAWSSSORefresh_TransientFailureServesAStillValidToken: needsRefresh fires a
// whole skew window (10 min) AHEAD of expiry, so most renewals run against a
// token that is still perfectly good. One throttle there must NOT cost the run
// its credential — before the dispatch-time mechanism gate lands, an un-ready SSO
// lane would silently walk on to the api-key placeholder. Serve the token in
// hand, carry no refusal sentence, dead-mark nothing.
func TestAWSSSORefresh_TransientFailureServesAStillValidToken(t *testing.T) {
	s, audit, blob := ssoRefreshServer(t)
	// Five minutes of validity left: inside the skew window (so a renewal is
	// attempted) but NOT expired (so the token still works).
	putAWSSSOBlob(t, s, awsSSOTestFixedNow.Add(5*time.Minute))
	calls := fakeOIDC(t, func(w http.ResponseWriter, _ map[string]string, _ int) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "slow_down"})
	})

	ba := s.resolveBedrockAuth(context.Background(), "claude-code", false, true, true, nil, awsSSOScope{})
	if !ba.ready || !ba.ssoInject {
		t.Fatalf("ready=%v ssoInject=%v; want the still-valid token served", ba.ready, ba.ssoInject)
	}
	if ba.ssoRefreshFailure != "" {
		t.Errorf("ssoRefreshFailure = %q; want empty — nothing failed for THIS run, it has a working token", ba.ssoRefreshFailure)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("CreateToken calls = %d; want 2 (the renewal was still attempted, with its one retry)", got)
	}
	if s.awsSSOTokenSpent(awsSSOTokenFingerprint(blob.RefreshToken)) {
		t.Fatal("a throttle dead-marked the credential")
	}
	// The sandbox gets the token that still works, and the failure is still audited.
	files := decodeSSOFiles(t, ba.env[awsSSOConfigEnvVar])
	if !strings.Contains(files[".aws/sso/cache/"+awsSSOCacheFileName(awsSSOProfileName)+".json"], blob.AccessToken) {
		t.Error("the sandbox cache does not carry the still-valid access token")
	}
	if rows := audit.find("harness.credential.refresh"); len(rows) != 1 || rows[0].Outcome != "failure" {
		t.Fatalf("audit rows = %+v; want the failed renewal still audited", rows)
	}
	if got := s.metrics.ssoRefreshOutcomes[ssoRefreshOutcomeTransportError]; got != 1 {
		t.Errorf("wardyn_sso_refresh_total{outcome=transport_error} = %d, want 1 (the access token was still valid)", got)
	}
}
