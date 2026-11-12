// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// windowAuthorityOf is the window authority of a request that bearer admits, taken through the real token
// authentication.
func windowAuthorityOf(t *testing.T, e *cliEnv, bearer string) macWindowAuthority {
	t.Helper()
	var got macWindowAuthority
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = macWindowAuthorityFrom(r.Context(), types.ActorHuman, principalFromRequest(r), e.clock)
	})
	deny := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) })
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("Authorization", "Bearer "+bearer)
	rec := httptest.NewRecorder()
	e.srv.apiTokenAuth(next, deny).ServeHTTP(rec, r)
	if rec.Code == http.StatusUnauthorized || !got.Token {
		t.Fatalf("the bearer did not authenticate as a token: %d %+v", rec.Code, got)
	}
	return got
}

// A window bound to a CLI sign-in outlives a refresh, which revokes the access token it replaces, and ends with
// the session. Run against the real sign-in, refresh and end paths.
func TestPG_MacWindow_CLISessionBinding(t *testing.T) {
	ctx := context.Background()
	e := newCLIEnv(t)
	c, g1 := e.signIn(e.user, nil)
	a := windowAuthorityOf(t, e, g1.AccessToken)
	if a.CLISessionID != g1.SessionID || a.TokenID != uuid.Nil {
		t.Fatalf("authority = %+v, want bound to session %s alone", a, g1.SessionID)
	}
	if gone, err := e.srv.macWindowRevoked(ctx, a); gone || err != nil {
		t.Fatalf("a live session = revoked %v, err %v", gone, err)
	}
	e.advance(time.Minute)
	if w := c.refresh(g1.SessionID, g1.Secret); w.Code != http.StatusOK {
		t.Fatalf("refresh = %d %s", w.Code, w.Body)
	}
	if code, _ := e.me(g1.AccessToken); code != http.StatusUnauthorized {
		t.Fatalf("the rotated-past access token = %d, want 401", code)
	}
	if gone, err := e.srv.macWindowRevoked(ctx, a); gone || err != nil {
		t.Fatalf("after a rotation = revoked %v, err %v, want the window kept", gone, err)
	}
	if _, err := e.pg.EndCLISession(ctx, g1.SessionID, "", types.CLISessionEndSignedOut, e.clock); err != nil {
		t.Fatal(err)
	}
	if gone, err := e.srv.macWindowRevoked(ctx, a); !gone || err != nil {
		t.Fatalf("after the session ended = revoked %v, err %v, want revoked", gone, err)
	}
}

// A window bound to a personal API token ends when the token is revoked through DELETE /api/v1/me/tokens/{id}.
func TestPG_MacWindow_PersonalTokenBinding(t *testing.T) {
	ctx := context.Background()
	e := newCLIEnv(t)
	tok := decodeJSON[types.APIToken](t, e.call(e.user, http.MethodPost, "/api/v1/me/tokens", `{"name":"win"}`))
	a := windowAuthorityOf(t, e, tok.Token)
	if a.TokenID != tok.ID || a.CLISessionID != uuid.Nil {
		t.Fatalf("authority = %+v, want bound to token %s alone", a, tok.ID)
	}
	if gone, err := e.srv.macWindowRevoked(ctx, a); gone || err != nil {
		t.Fatalf("a live token = revoked %v, err %v", gone, err)
	}
	if w := e.call(e.user, http.MethodDelete, "/api/v1/me/tokens/"+tok.ID.String(), ""); w.Code != http.StatusNoContent {
		t.Fatalf("revoke = %d %s", w.Code, w.Body)
	}
	if _, err := e.pg.GetLiveAPIToken(ctx, tok.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetLiveAPIToken after the revoke = %v, want ErrNotFound", err)
	}
	if gone, err := e.srv.macWindowRevoked(ctx, a); !gone || err != nil {
		t.Fatalf("after the revoke = revoked %v, err %v, want revoked", gone, err)
	}
}
