// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// restampToken sets a token's stamp the way a login's re-stamp does: nil is a row nothing ever stamped.
func restampToken(st *tokenMemStore, id uuid.UUID, at *time.Time) {
	st.mu.Lock()
	defer st.mu.Unlock()
	t := st.byID[id]
	t.IdentityStampedAt = at
	st.byID[id] = t
}

// TestAPITokenAuth_RoleStampTTL: under WARDYN_ROLE_STAMP_TTL a token whose stamp is older is refused 401
// role_stamp_stale with an authz.denied row naming its owner, until a re-stamp makes it work again; with
// the TTL unset the same token is untouched. A refused token is not "used".
func TestAPITokenAuth_RoleStampTTL(t *testing.T) {
	ago := func(d time.Duration) *time.Time { at := time.Now().UTC().Add(-d); return &at }

	h := newHarness(t)
	st := newTokenMemStore()
	cfg := baseTestConfig(h, st)
	cfg.OIDC = &oidc.Authenticator{}
	cfg.RoleStampTTL = time.Hour
	srv := New(cfg)

	sess := ssoSession(t, tokenMemberSub, tokenMemberMail, oidc.RoleUser)
	raw, created := mintToken(t, srv, sess, "ci")
	get := func() int { return do(t, srv, http.MethodGet, "/api/v1/me", raw, "").Code }

	if got := get(); got != http.StatusOK {
		t.Fatalf("fresh stamp: code = %d, want 200", got)
	}

	restampToken(st, created.ID, ago(2*time.Hour))
	st.mu.Lock()
	delete(st.touched, created.ID)
	st.mu.Unlock()
	w := do(t, srv, http.MethodGet, "/api/v1/me", raw, "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("stale stamp: code = %d, want 401; body=%s", w.Code, w.Body.String())
	}
	var body struct{ Reason string }
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Reason != "role_stamp_stale" {
		t.Errorf("stale stamp body = %s, want reason role_stamp_stale", w.Body.String())
	}
	if reasons := auditReasons(t, srv, "authz.denied"); !slices.Equal(reasons, []string{"role_stamp_stale"}) {
		t.Errorf("authz.denied reasons = %v, want [role_stamp_stale]", reasons)
	}
	for _, ev := range srv.cfg.Audit.(*recRecorder).snapshot() {
		if ev.Action == "authz.denied" && ev.Actor != tokenMemberSub {
			t.Errorf("authz.denied actor = %q, want the token's owner %q", ev.Actor, tokenMemberSub)
		}
	}
	st.mu.Lock()
	_, touched := st.touched[created.ID]
	st.mu.Unlock()
	if touched {
		t.Error("a refused token was recorded as used")
	}

	// A stamp nothing ever set is stale, not fresh.
	restampToken(st, created.ID, nil)
	if got := get(); got != http.StatusUnauthorized {
		t.Errorf("nil stamp: code = %d, want 401", got)
	}

	// The owner signs in again: the re-stamp is what makes it work.
	restampToken(st, created.ID, ago(time.Minute))
	if got := get(); got != http.StatusOK {
		t.Errorf("after re-stamp: code = %d, want 200", got)
	}

	// TTL off: nothing changes, however old the stamp.
	restampToken(st, created.ID, ago(1000*time.Hour))
	srv.cfg.RoleStampTTL = 0
	if got := get(); got != http.StatusOK {
		t.Errorf("TTL unset, old stamp: code = %d, want 200", got)
	}
}
