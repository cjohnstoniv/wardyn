// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAWSSSOBlobRenewable_RequiresTheRegistrationPair(t *testing.T) {
	now := awsSSOTestFixedNow
	full := awsSSOBlob{RefreshToken: "rt", ClientID: "cid", ClientSecret: "csec", RegistrationExpiresAt: now.Add(time.Hour)}
	for name, tc := range map[string]struct {
		edit func(*awsSSOBlob)
		want bool
	}{
		"refresh token and pair":   {func(*awsSSOBlob) {}, true},
		"no client id":             {func(b *awsSSOBlob) { b.ClientID = "" }, false},
		"no client secret":         {func(b *awsSSOBlob) { b.ClientSecret = "" }, false},
		"no pair at all":           {func(b *awsSSOBlob) { b.ClientID, b.ClientSecret = "", "" }, false},
		"no refresh token":         {func(b *awsSSOBlob) { b.RefreshToken = "" }, false},
		"registration lapsed":      {func(b *awsSSOBlob) { b.RegistrationExpiresAt = now.Add(-time.Second) }, false},
		"zero registration expiry": {func(b *awsSSOBlob) { b.RegistrationExpiresAt = time.Time{} }, true},
	} {
		b := full
		tc.edit(&b)
		if got := b.renewable(now); got != tc.want {
			t.Errorf("%s: renewable = %v, want %v", name, got, tc.want)
		}
	}
}

// A refresh token stored without the client registration CreateToken needs
// beside it is never sent to AWS: that call can only answer invalid_request, and
// a door keyed on that answer would loop (sign in, works, next renewal refused).
// The token in hand is served until it expires, then the sign-in door opens.
func TestAWSSSORefresh_PairlessBlobNeverCallsAWS(t *testing.T) {
	s, _, blob := ssoRefreshServer(t)
	blob.ClientID, blob.ClientSecret = "", ""
	calls := fakeOIDC(t, func(w http.ResponseWriter, _ map[string]string, _ int) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid_request"})
	})

	blob.ExpiresAt = awsSSOTestFixedNow.Add(5 * time.Minute) // inside the skew window, not expired
	storeSSOBlob(t, s, blob)
	if _, failure := ssoDispatch(s); failure != "" {
		t.Fatalf("unexpired pair-less blob: refusal %q, want its token served", failure)
	}

	blob.ExpiresAt = awsSSOTestFixedNow.Add(-time.Minute)
	storeSSOBlob(t, s, blob)
	_, d, err := s.providerBedrockRefusal(context.Background(), awsSSOTestProvider(), "claude-code", awsSSOTestOwner, true)
	if err != nil || !d.credential || d.msg != ssoSpentRefusal {
		t.Fatalf("expired pair-less blob: denial %+v err %v, want the not-signed-in sign-in door", d, err)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("CreateToken calls = %d, want 0", got)
	}
}

func TestCreateAWSSSOToken_RefusalClasses(t *testing.T) {
	for _, tc := range []struct {
		status      int
		code, desc  string
		spent       bool
		refused     bool
		wantRefusal string
	}{
		{status: 400, code: "invalid_grant", spent: true},
		{status: 400, code: "invalid_client", spent: true},
		{status: 400, code: "invalid_request", desc: "a required parameter might be missing", refused: true,
			wantRefusal: "invalid_request: a required parameter might be missing"},
		{status: 400, code: "access_denied", refused: true, wantRefusal: "access_denied"},
		{status: 400, code: "invalid_scope", refused: true, wantRefusal: "invalid_scope"},
		{status: 400, code: "unsupported_grant_type", refused: true, wantRefusal: "unsupported_grant_type"},
		{status: 400, code: "slow_down"},
		{status: 400, code: "authorization_pending"},
		{status: 500, code: "server_error"},
		{status: 503, code: "invalid_request"},
		{status: 502},
	} {
		s, _, blob := ssoRefreshServer(t)
		fakeOIDC(t, func(w http.ResponseWriter, _ map[string]string, _ int) {
			w.WriteHeader(tc.status)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": tc.code, "error_description": tc.desc})
		})
		_, err := s.createAWSSSOToken(context.Background(), blob)
		refused, isRefused := awsSSORefusal(err)
		if err == nil || errors.Is(err, errAWSSSOCredentialSpent) != tc.spent || isRefused != tc.refused {
			t.Errorf("%d %q: err %v, want spent=%v refused=%v", tc.status, tc.code, err, tc.spent, tc.refused)
			continue
		}
		if isRefused && refused.detail() != tc.wantRefusal {
			t.Errorf("%d %q: detail %q, want %q", tc.status, tc.code, refused.detail(), tc.wantRefusal)
		}
	}
}

// A coded refusal with nothing left to serve opens the console's sign-in door
// (model_credential), names AWS's answer, keeps the stored blob and is not
// retried; a transient answer keeps today's "launch again" refusal.
func TestAWSSSORefresh_RefusedOpensTheSignInDoor(t *testing.T) {
	s, audit, blob := ssoRefreshServer(t)
	calls := fakeOIDC(t, func(w http.ResponseWriter, _ map[string]string, _ int) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid_request", "error_description": "Invalid request"})
	})

	_, d, err := s.providerBedrockRefusal(context.Background(), awsSSOTestProvider(), "claude-code", awsSSOTestOwner, true)
	want := connectDenial("bedrock-sso", fmt.Sprintf(mpBRRefused, "invalid_request: Invalid request"))
	if err != nil || d != want {
		t.Fatalf("denial %+v err %v, want %+v", d, err, want)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("CreateToken calls = %d, want 1 (a refusal is not retried)", got)
	}
	if s.awsSSOTokenSpent(awsSSOTokenFingerprint(blob.RefreshToken)) {
		t.Error("a refusal marked the refresh token spent")
	}
	if stored := storedSSOBlob(t, s); stored.RefreshToken != blob.RefreshToken || stored.ClientSecret != blob.ClientSecret {
		t.Errorf("a refusal changed the stored blob: %+v", stored)
	}
	rows := audit.find("harness.credential.refresh")
	if len(rows) != 1 {
		t.Fatalf("refresh rows = %d, want 1", len(rows))
	}
	var data map[string]any
	if json.Unmarshal(rows[0].Data, &data) != nil || data["code"] != "invalid_request" || data["description"] != "Invalid request" || data["spent"] != false {
		t.Errorf("refresh row data = %s, want code, description and spent:false", rows[0].Data)
	}

	transient, _, _ := ssoRefreshServer(t)
	fakeOIDC(t, func(w http.ResponseWriter, _ map[string]string, _ int) {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "server_error"})
	})
	if _, d, _ := transient.providerBedrockRefusal(context.Background(), awsSSOTestProvider(), "claude-code", awsSSOTestOwner, true); d.credential || d.msg != ssoRenewalRefusal {
		t.Errorf("transient denial %+v, want the launch-again refusal", d)
	}
}

func TestAWSSSORefresh_RefusedWhileValidServesTheToken(t *testing.T) {
	s, _, _ := ssoRefreshServer(t)
	blob := putAWSSSOBlob(t, s, awsSSOTestFixedNow.Add(5*time.Minute))
	fakeOIDC(t, func(w http.ResponseWriter, _ map[string]string, _ int) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "access_denied"})
	})
	got, failure := s.refreshAWSSSOBlob(context.Background(), awsSSOTestScope(), blob)
	if failure != "" || got.AccessToken != blob.AccessToken {
		t.Fatalf("failure %q token %q, want the still-valid token served", failure, got.AccessToken)
	}
}

func TestAWSSSOErrorDescription_IsBoundedAndCarriesNoSecret(t *testing.T) {
	blob := awsSSOBlob{AccessToken: "at-123", RefreshToken: "rt-456", ClientSecret: "cs-789"}
	if got := awsSSOErrorDescription("bad token rt-456 sent", blob); got != "" {
		t.Errorf("a description naming the refresh token kept %q", got)
	}
	if got := awsSSOErrorDescription("line\none\x1b[31m", blob); got != "line one [31m" {
		t.Errorf("control characters kept: %q", got)
	}
	if got := awsSSOErrorDescription(strings.Repeat("é", 300), blob); len(got) > awsSSODescriptionMax+len("…") {
		t.Errorf("description length %d, want at most %d", len(got), awsSSODescriptionMax+len("…"))
	}
}

// A mid-run hold raised by a coded refusal names it `refused`: a fix (sign in)
// different from `unavailable`'s (wait).
func TestResolveAWSSSOInjection_RefusedRenewalIsAuditedRefused(t *testing.T) {
	f := newReauthFixture(t, nil)
	b := liveSSOBlob()
	b.RefreshToken = "refresh-token-aws-refuses"
	b.ExpiresAt = time.Now().Add(-time.Hour).UTC()
	b.ClientID, b.ClientSecret = "cid", "csec"
	b.RegistrationExpiresAt = time.Now().Add(30 * 24 * time.Hour).UTC()
	f.putBlob(t, "alice@example.com", b)

	oidc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_request","error_description":"Invalid request"}`))
	}))
	defer oidc.Close()
	f.srv.cfg.AWSSSOEndpointOverride = oidc.URL
	f.st.grants[0].Spec.Scope = reauthSnapshotScope(t, "alice@example.com", reauthRegion, "111122223333", "WardynAgent")
	f.brk.setHost(ssoPortalHost(reauthRegion, oidc.URL))

	if w := f.resolve(t); w.Code != http.StatusLocked {
		t.Fatalf("resolve: code = %d, want 423; body=%s", w.Code, w.Body.String())
	}
	for _, ev := range f.audit.events() {
		var d map[string]any
		if ev.Action == "credential.reauth.request" && json.Unmarshal(ev.Data, &d) == nil {
			if d["reason"] != awsSSOReauthReasonRefused {
				t.Fatalf("raise reason = %v, want %q", d["reason"], awsSSOReauthReasonRefused)
			}
			return
		}
	}
	t.Fatal("no credential.reauth.request row")
}
