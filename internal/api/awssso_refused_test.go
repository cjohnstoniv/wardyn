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
	if got := awsSSOErrorDescription(strings.Repeat("é", 300), blob); len(got) > awsSSODescriptionMax {
		t.Errorf("description length %d, want at most %d", len(got), awsSSODescriptionMax)
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

// A refused, non-spent session must read the same everywhere: once its access
// token has expired the readiness grade is expired_signin (the banner and the
// Settings chip key on it), Review refuses with the same sign-in door dispatch
// does, and a fresh sign-in, which carries a new refresh token, clears it.
func TestAWSSSORefused_GradesSignedOutUntilASignIn(t *testing.T) {
	s, audit, blob := ssoRefreshServer(t)
	answer := map[string]any{"error": "invalid_request", "error_description": "Invalid request"}
	fakeOIDC(t, func(w http.ResponseWriter, _ map[string]string, _ int) {
		if answer["error"] != nil {
			w.WriteHeader(http.StatusBadRequest)
		}
		_ = json.NewEncoder(w).Encode(answer)
	})
	ctx, p := context.Background(), awsSSOTestProvider()
	want := connectDenial(p.ID, fmt.Sprintf(mpBRRefused, "invalid_request: Invalid request"))
	grade := func() SetupProviderAccess {
		var row SetupProviderAccess
		s.gradeProviderBedrockSSO(ctx, &row, p, awsSSOTestOwner)
		return row
	}

	if _, d, _ := s.providerBedrockRefusal(ctx, p, "claude-code", awsSSOTestOwner, true); d != want {
		t.Fatalf("dispatch denial %+v, want %+v", d, want)
	}
	if row := grade(); row.State != modelAccessExpiredSignin || row.Action != modelAccessSignInAction {
		t.Errorf("grade after a refusal = %q / %q, want expired_signin with the sign-in action", row.State, row.Action)
	}
	if _, d, _ := s.providerBedrockRefusal(ctx, p, "claude-code", awsSSOTestOwner, false); d != want {
		t.Errorf("Review denial %+v, want the dispatch's %+v", d, want)
	}
	var data map[string]any
	if rows := audit.find("harness.credential.refresh"); len(rows) != 1 || json.Unmarshal(rows[0].Data, &data) != nil || data["registration"] != "present" {
		t.Errorf("refresh rows = %+v, want one carrying registration: present", rows)
	}

	// Expired but renewable on purpose: only a mark keyed by the refresh token
	// leaves this one live and passing Review.
	fresh := blob
	fresh.RefreshToken = "a-new-sign-ins-refresh-token"
	storeSSOBlob(t, s, fresh)
	if !fresh.expired(awsSSOTestFixedNow) || !fresh.renewable(awsSSOTestFixedNow) {
		t.Fatal("the fresh blob must be expired and renewable")
	}
	if row := grade(); row.State != modelAccessLive {
		t.Errorf("grade after a fresh sign-in = %q, want live", row.State)
	}
	if _, d, _ := s.providerBedrockRefusal(ctx, p, "claude-code", awsSSOTestOwner, false); d.msg != "" {
		t.Errorf("Review after a fresh sign-in refused: %+v", d)
	}

	// A later renewal AWS accepts clears the mark on the same refresh token.
	storeSSOBlob(t, s, blob)
	answer = map[string]any{"accessToken": "renewed-access-token-abcdefghij", "expiresIn": 3600}
	if _, d, _ := s.providerBedrockRefusal(ctx, p, "claude-code", awsSSOTestOwner, true); d.msg != "" {
		t.Fatalf("renewal after AWS recovered refused: %+v", d)
	}
	if _, marked := s.awsSSORefusedFor(blob); marked {
		t.Error("a successful renewal left the refused mark in place")
	}
}

func TestAWSSSOBlobLastUsable_PairlessIsTheAccessTokensExpiry(t *testing.T) {
	b := awsSSOBlob{RefreshToken: "rt", ExpiresAt: awsSSOTestFixedNow, RegistrationExpiresAt: awsSSOTestFixedNow.Add(90 * 24 * time.Hour)}
	if got := b.lastUsable(); !got.Equal(b.ExpiresAt) {
		t.Errorf("pair-less lastUsable = %v, want the access token's expiry %v", got, b.ExpiresAt)
	}
	b.ClientID, b.ClientSecret = "cid", "csec"
	if got := b.lastUsable(); !got.Equal(b.RegistrationExpiresAt) {
		t.Errorf("lastUsable with the pair = %v, want the registration's expiry", got)
	}
}

// A refused session that still has a valid token inside the refresh skew is
// `expiring` until its own expiry (dispatch serves it until then), as a spent one is.
func TestAWSSSOCredentialState_RefusedInsideTheSkewIsExpiring(t *testing.T) {
	now := awsSSOTestFixedNow
	b := awsSSOBlob{RefreshToken: "rt", ClientID: "cid", ClientSecret: "csec",
		ExpiresAt: now.Add(5 * time.Minute), RegistrationExpiresAt: now.Add(90 * 24 * time.Hour)}
	if got := awsSSOCredentialState(b, true, false, true, now, awsSSORefreshSkew); got != modelAccessExpiring {
		t.Errorf("refused inside the skew = %q, want expiring", got)
	}
	if got := modelAccessDeadline(b, true, false, true, now, awsSSORefreshSkew); got != b.ExpiresAt.UTC().Format(time.RFC3339) {
		t.Errorf("refused deadline = %q, want the access token's own expiry", got)
	}
	if got := awsSSOCredentialState(b, true, false, false, now, awsSSORefreshSkew); got != modelAccessLive {
		t.Errorf("the same blob unrefused = %q, want live", got)
	}
}

func TestAWSSSORefusedMarksAreBounded(t *testing.T) {
	s := &Server{}
	for i := range awsSSORefusedMax + 10 {
		s.markAWSSSORefused(context.Background(), fmt.Sprintf("fp-%d", i), "", "invalid_request")
	}
	if n := len(s.ssoRefreshRefused); n != awsSSORefusedMax {
		t.Fatalf("refused marks = %d, want the bound %d", n, awsSSORefusedMax)
	}
	last := fmt.Sprintf("fp-%d", awsSSORefusedMax+9)
	if s.ssoRefreshRefused[last] != "invalid_request" {
		t.Error("the newest refusal was evicted")
	}
	s.markAWSSSORefused(context.Background(), last, "", "access_denied")
	if n := len(s.ssoRefreshRefused); n != awsSSORefusedMax || s.ssoRefreshRefused[last] != "access_denied" {
		t.Errorf("re-marking a known token: %d marks, detail %q; want no eviction and the new detail", n, s.ssoRefreshRefused[last])
	}
}
