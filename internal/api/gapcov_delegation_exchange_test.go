// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

var (
	gapCovPortalID     = uuid.MustParse("00000000-0000-0000-0000-00000000de01")
	gapCovPortalSecret = delegateCredentialPrefix + "portal-credential"
)

// gapCovDelegateStore answers the portal lookup and counts the mints, so a refusal can be shown to
// have minted nothing. The rest of the delegation store panics: the exchange must not reach it.
type gapCovDelegateStore struct {
	store.Store
	byRaw  map[string]types.Delegate
	getErr error
	mints  int
}

func (s *gapCovDelegateStore) GetDelegateByRaw(_ context.Context, raw string) (types.Delegate, error) {
	if s.getErr != nil {
		return types.Delegate{}, s.getErr
	}
	if d, ok := s.byRaw[raw]; ok {
		return d, nil
	}
	return types.Delegate{}, store.ErrNotFound
}

func (s *gapCovDelegateStore) MintDelegatedToken(context.Context, types.DelegatedToken, string, time.Time) (types.DelegatedToken, error) {
	s.mints++
	return types.DelegatedToken{}, errors.New("not part of these tests")
}

func (s *gapCovDelegateStore) CreateDelegate(context.Context, types.Delegate, string) (types.Delegate, error) {
	panic("CreateDelegate is not part of the exchange")
}
func (s *gapCovDelegateStore) ListDelegates(context.Context) ([]types.Delegate, error) {
	panic("ListDelegates is not part of the exchange")
}
func (s *gapCovDelegateStore) RevokeDelegate(context.Context, uuid.UUID, time.Time) (types.Delegate, error) {
	panic("RevokeDelegate is not part of the exchange")
}
func (s *gapCovDelegateStore) GetDelegatedTokenByRaw(context.Context, string, time.Time) (types.DelegatedToken, error) {
	panic("GetDelegatedTokenByRaw is not part of the exchange")
}
func (s *gapCovDelegateStore) GetDelegatedTokenByID(context.Context, uuid.UUID, time.Time) (types.DelegatedToken, error) {
	panic("GetDelegatedTokenByID is not part of the exchange")
}

func gapCovPortalStore() *gapCovDelegateStore {
	return &gapCovDelegateStore{byRaw: map[string]types.Delegate{
		gapCovPortalSecret: {ID: gapCovPortalID, Name: "portal", IdPClientID: "idp-client", Group: "portal-users"},
	}}
}

func gapCovForm(extra url.Values) url.Values {
	f := url.Values{
		"grant_type":         {grantTypeTokenExchange},
		"subject_token":      {"a-person-token"},
		"subject_token_type": {tokenTypeAccessToken},
	}
	for k, v := range extra {
		f[k] = v
	}
	return f
}

func gapCovExchange(t *testing.T, srv *Server, user, secret string, basic bool, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/token", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if basic {
		r.SetBasicAuth(user, secret)
	}
	w := httptest.NewRecorder()
	srv.handleTokenExchange(w, r)
	return w
}

func gapCovOAuthError(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body oauthError
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %q: %v", w.Body, err)
	}
	return body.Error
}

// Every way the portal fails to authenticate is the same 401 invalid_client carrying the Basic
// challenge, with the specific cause only in the auth.fail row, and nothing is minted.
func TestGapCovTokenExchangeClientAuthentication(t *testing.T) {
	for _, tc := range []struct {
		name       string
		user       string
		secret     string
		basic      bool
		wantReason string
	}{
		{"no credentials at all", "", "", false, "missing_client_credential"},
		{"a secret that is not a portal credential", gapCovPortalID.String(), "wdk_not-a-portal-credential", true, "missing_client_credential"},
		{"a credential no portal holds", gapCovPortalID.String(), delegateCredentialPrefix + "unknown", true, "invalid_client_credential"},
		{"another portal's id with this credential", "00000000-0000-0000-0000-00000000de02", gapCovPortalSecret, true, "invalid_client_credential"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ds := gapCovPortalStore()
			h := newHarness(t)
			srv := New(baseTestConfig(h, ds))

			w := gapCovExchange(t, srv, tc.user, tc.secret, tc.basic, gapCovForm(nil))

			if w.Code != http.StatusUnauthorized || gapCovOAuthError(t, w) != "invalid_client" || !strings.HasPrefix(w.Header().Get("WWW-Authenticate"), "Basic ") {
				t.Fatalf("answer = %d %s challenge %q, want 401 invalid_client with the Basic challenge", w.Code, w.Body, w.Header().Get("WWW-Authenticate"))
			}
			rows := govCovAudits(h, "auth.fail")
			if len(rows) != 1 || rows[0].Actor != delegationAuthActor || !strings.Contains(string(rows[0].Data), `"reason":"`+tc.wantReason+`"`) {
				t.Fatalf("auth.fail rows = %+v, want one from %s with reason %s", rows, delegationAuthActor, tc.wantReason)
			}
			if ds.mints != 0 {
				t.Errorf("%d token(s) minted by a refused client", ds.mints)
			}
		})
	}
}

// A store with no delegation tables answers 501, and a lookup that fails is a 503 that is not an
// authentication verdict.
func TestGapCovTokenExchangeStoreAvailability(t *testing.T) {
	t.Run("a store without the delegation tables", func(t *testing.T) {
		srv := New(baseTestConfig(newHarness(t), &roleMapStore{}))
		w := gapCovExchange(t, srv, gapCovPortalID.String(), gapCovPortalSecret, true, gapCovForm(nil))
		if w.Code != http.StatusNotImplemented || gapCovOAuthError(t, w) != "server_error" {
			t.Fatalf("answer = %d %s, want 501 server_error", w.Code, w.Body)
		}
	})
	t.Run("a failed portal lookup", func(t *testing.T) {
		ds := gapCovPortalStore()
		ds.getErr = errors.New("gapcov: lookup refused")
		h := newHarness(t)
		srv := New(baseTestConfig(h, ds))
		w := gapCovExchange(t, srv, gapCovPortalID.String(), gapCovPortalSecret, true, gapCovForm(nil))
		if w.Code != http.StatusServiceUnavailable || gapCovOAuthError(t, w) != "temporarily_unavailable" {
			t.Fatalf("answer = %d %s, want 503 temporarily_unavailable", w.Code, w.Body)
		}
		if rows := govCovAudits(h, "auth.fail"); len(rows) != 0 {
			t.Errorf("%d auth.fail rows for a lookup the store failed; it is not a credential verdict", len(rows))
		}
	})
}

// A request that is not shaped as an RFC 8693 exchange is refused with its own error code before the
// person's token is looked at.
func TestGapCovTokenExchangeFormRefusals(t *testing.T) {
	for _, tc := range []struct {
		name     string
		form     url.Values
		wantCode string
	}{
		{"another grant type", gapCovForm(url.Values{"grant_type": {"client_credentials"}}), "unsupported_grant_type"},
		{"an actor token", gapCovForm(url.Values{"actor_token": {"x"}}), "invalid_request"},
		{"another requested token type", gapCovForm(url.Values{"requested_token_type": {tokenTypeIDToken}}), "invalid_request"},
		{"no subject token", gapCovForm(url.Values{"subject_token": {""}}), "invalid_request"},
		{"an unknown subject token type", gapCovForm(url.Values{"subject_token_type": {"urn:example:other"}}), "invalid_request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ds := gapCovPortalStore()
			srv := New(baseTestConfig(newHarness(t), ds))
			w := gapCovExchange(t, srv, gapCovPortalID.String(), gapCovPortalSecret, true, tc.form)
			if w.Code != http.StatusBadRequest || gapCovOAuthError(t, w) != tc.wantCode {
				t.Fatalf("answer = %d %s, want 400 %s", w.Code, w.Body, tc.wantCode)
			}
			if ds.mints != 0 {
				t.Errorf("%d token(s) minted for a malformed request", ds.mints)
			}
		})
	}

	t.Run("a body that is not a form", func(t *testing.T) {
		srv := New(baseTestConfig(newHarness(t), gapCovPortalStore()))
		r := httptest.NewRequest(http.MethodPost, "/api/v1/token", strings.NewReader("grant_type=%zz"))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.SetBasicAuth(gapCovPortalID.String(), gapCovPortalSecret)
		w := httptest.NewRecorder()
		srv.handleTokenExchange(w, r)
		if w.Code != http.StatusBadRequest || gapCovOAuthError(t, w) != "invalid_request" {
			t.Fatalf("answer = %d %s, want 400 invalid_request", w.Code, w.Body)
		}
	})
	t.Run("a token in the query string is not read", func(t *testing.T) {
		srv := New(baseTestConfig(newHarness(t), gapCovPortalStore()))
		r := httptest.NewRequest(http.MethodPost, "/api/v1/token?subject_token=a-person-token&subject_token_type="+url.QueryEscape(tokenTypeAccessToken)+
			"&grant_type="+url.QueryEscape(grantTypeTokenExchange), nil)
		r.SetBasicAuth(gapCovPortalID.String(), gapCovPortalSecret)
		w := httptest.NewRecorder()
		srv.handleTokenExchange(w, r)
		if w.Code != http.StatusBadRequest || gapCovOAuthError(t, w) != "unsupported_grant_type" {
			t.Fatalf("answer = %d %s, want 400 unsupported_grant_type (the query is ignored)", w.Code, w.Body)
		}
	})
}

// A well-formed exchange the deployment cannot honour is a 400 invalid_grant that names the portal in
// a denied delegation.exchange row and mints nothing: no SSO, or a subject token that is itself a
// delegated token.
func TestGapCovTokenExchangeDeniesWhatItCannotHonour(t *testing.T) {
	for _, tc := range []struct {
		name       string
		subject    string
		wantReason string
	}{
		{"SSO is not configured", "a-person-token", "sso_not_configured"},
		{"the subject token is a delegated token", delegatedTokenPrefix + "already-delegated", "subject_token_delegated"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ds := gapCovPortalStore()
			h := newHarness(t)
			cfg := baseTestConfig(h, ds)
			srv := New(cfg)
			if tc.wantReason == "subject_token_delegated" {
				srv.cfg.OIDC = newAccessAuth(t, nil, "", nil, nil)
			}

			w := gapCovExchange(t, srv, gapCovPortalID.String(), gapCovPortalSecret, true, gapCovForm(url.Values{"subject_token": {tc.subject}}))

			if w.Code != http.StatusBadRequest || gapCovOAuthError(t, w) != "invalid_grant" {
				t.Fatalf("answer = %d %s, want 400 invalid_grant", w.Code, w.Body)
			}
			rows := govCovAudits(h, "delegation.exchange")
			if len(rows) != 1 || rows[0].Outcome != "denied" || rows[0].Actor != delegateActor(gapCovPortalID) ||
				!strings.Contains(string(rows[0].Data), `"reason":"`+tc.wantReason+`"`) {
				t.Fatalf("delegation.exchange rows = %+v, want one denied row by the portal with reason %s", rows, tc.wantReason)
			}
			if ds.mints != 0 {
				t.Errorf("%d token(s) minted by a refused exchange", ds.mints)
			}
		})
	}
}
