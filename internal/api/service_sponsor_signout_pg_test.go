// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"testing"
	"time"
)

// A sponsor's session-only sign-out (#3790) leaves the service they sponsor working: its token still
// authenticates and a launch it queued stays live. A full revoke of the sponsor stops both. The two tests
// are a pair, so the rule cannot drift to one side.

func TestPG_SponsorSessionsOnlySignOutKeepsServiceWorking(t *testing.T) {
	e := newSvcEnv(t)
	si := e.mustCreate(t, svcName, svcSponsor)
	tok := e.mustMint(t, e.sponsor, svcName)
	if got := e.as(t, tok.Token, http.MethodGet, "/api/v1/me", "").Code; got != http.StatusOK {
		t.Fatalf("service GET /me before the sign-out: %d, want 200", got)
	}
	time.Sleep(10 * time.Millisecond)
	before := time.Now()
	time.Sleep(10 * time.Millisecond)

	if w := doSSO(t, e.h.srv, http.MethodPost, "/api/v1/sessions/revoke", e.sec, `{"sub":"`+svcSponsor+`","sessions_only":true}`); w.Code != http.StatusNoContent {
		t.Fatalf("sessions_only revoke = %d %s", w.Code, w.Body.String())
	}

	if got := e.as(t, tok.Token, http.MethodGet, "/api/v1/me", "").Code; got != http.StatusOK {
		t.Errorf("service GET /me after the sponsor's sessions_only sign-out: %d (auth.fail %q), want 200", got, e.lastAuthFail())
	}
	if live, err := e.h.srv.queueOwnerLive(t.Context(), serviceAuthority(si, svcSponsor), before); err != nil || !live {
		t.Errorf("queueOwnerLive after the sponsor's sessions_only sign-out = %v, %v; want true", live, err)
	}
}

func TestPG_SponsorFullRevokeStopsServiceTokenAndQueue(t *testing.T) {
	e := newSvcEnv(t)
	si := e.mustCreate(t, svcName, svcSponsor)
	tok := e.mustMint(t, e.sponsor, svcName)
	if got := e.as(t, tok.Token, http.MethodGet, "/api/v1/me", "").Code; got != http.StatusOK {
		t.Fatalf("service GET /me before the revoke: %d, want 200", got)
	}
	time.Sleep(10 * time.Millisecond)
	before := time.Now()
	time.Sleep(10 * time.Millisecond)

	// The sponsor's full cut is written directly: the revoke handler also sweeps the sponsored tokens, which
	// would stop the token before the sponsor check this test pins (token_move_pg_test.go covers the sweep).
	if err := e.rev.RevokeSub(t.Context(), svcSponsor); err != nil {
		t.Fatal(err)
	}

	if got := e.as(t, tok.Token, http.MethodGet, "/api/v1/me", "").Code; got != http.StatusUnauthorized || e.lastAuthFail() != authFailedServiceSponsorInactive {
		t.Errorf("service GET /me after the sponsor's full revoke: %d (auth.fail %q), want 401 %q", got, e.lastAuthFail(), authFailedServiceSponsorInactive)
	}
	if live, err := e.h.srv.queueOwnerLive(t.Context(), serviceAuthority(si, svcSponsor), before); err != nil || live {
		t.Errorf("queueOwnerLive after the sponsor's full revoke = %v, %v; want false", live, err)
	}
}
