// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// cutoffRevocations is the SessionRevocations shape the real store has: a
// per-identity (plus global) timestamp, and "revoked" means issued at or before
// it. Modelled rather than stubbed — a double that answered a canned bool could
// not tell a token minted before the lever from one minted after it, which is
// the whole distinction being pinned.
type cutoffRevocations struct {
	mu      sync.Mutex
	bySub   map[string]time.Time
	global  time.Time
	err     error
	asked   int
	nowFunc func() time.Time
}

func newCutoffRevocations() *cutoffRevocations {
	return &cutoffRevocations{bySub: map[string]time.Time{}, nowFunc: func() time.Time { return time.Now().UTC() }}
}

func (c *cutoffRevocations) IsSessionRevoked(_ context.Context, sub, email string, issuedAt time.Time) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.asked++
	if c.err != nil {
		return false, c.err
	}
	cutoff := c.global
	for _, key := range []string{sub, email} {
		if key == "" {
			continue
		}
		if at, ok := c.bySub[key]; ok && at.After(cutoff) {
			cutoff = at
		}
	}
	if cutoff.IsZero() {
		return false, nil
	}
	return !issuedAt.After(cutoff), nil
}

func (c *cutoffRevocations) RevokeSub(_ context.Context, sub string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bySub[sub] = c.nowFunc()
	return nil
}

func (c *cutoffRevocations) RevokeAll(context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.global = c.nowFunc()
	return nil
}

var _ oidc.SessionRevocations = (*cutoffRevocations)(nil)

// TestAPITokenHonorsSessionRevocationCutoff is F143.
//
// POST /sessions/revoke revokes tokens by SWEEPING a ListAPITokens snapshot. A
// mint whose INSERT commits after that snapshot is taken is never reachable by
// that revoke again — api_tokens has no expiry and nothing re-checks — so the
// raced row was a PERMANENT credential surviving the incident lever that
// answered 204. The sweep works; the race simply escapes it.
//
// The close is on the READ side: apiTokenAuth compares the row's created_at
// against the same cutoff the session lane obeys. That removes the dependence on
// winning the race at all, which is why it is the fix rather than a second sweep
// pass.
func TestAPITokenHonorsSessionRevocationCutoff(t *testing.T) {
	build := func(t *testing.T) (*Server, *tokenMemStore, *cutoffRevocations) {
		t.Helper()
		h := newHarness(t)
		st := newTokenMemStore()
		cfg := baseTestConfig(h, st)
		cfg.OIDC = &oidc.Authenticator{}
		rev := newCutoffRevocations()
		cfg.SessionRevocations = rev
		return New(cfg), st, rev
	}

	// mint puts a row straight into the store with an explicit created_at, which
	// is what makes "before the cutoff" and "after the cutoff" expressible
	// without a sleep.
	mint := func(t *testing.T, st *tokenMemStore, raw, principal, email string, createdAt time.Time) {
		t.Helper()
		if _, err := st.CreateAPIToken(context.Background(), types.APIToken{
			ID: uuid.New(), Principal: principal, Email: email, Role: string(oidc.RoleAdmin),
			Name: raw, CreatedAt: createdAt,
		}, raw); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("a token minted in flight during revoke-all does not survive it", func(t *testing.T) {
		srv, st, rev := build(t)
		cutoff := time.Now().UTC()
		rev.nowFunc = func() time.Time { return cutoff }

		// The row the sweep's snapshot missed: its INSERT commits after the
		// snapshot, but it was minted by a request that had already passed the
		// revocation check, so its created_at is at-or-before the cutoff.
		mint(t, st, "wdn_inflight", "sub-alice", "alice@corp.example", cutoff.Add(-time.Millisecond))
		if err := rev.RevokeAll(context.Background()); err != nil {
			t.Fatal(err)
		}

		if w := do(t, srv, http.MethodGet, "/api/v1/me", "wdn_inflight", ""); w.Code != http.StatusUnauthorized {
			t.Errorf("in-flight token authenticates with %d after {\"all\":true}; want 401 — api_tokens has no "+
				"expiry, so a row the sweep's snapshot missed is a PERMANENT credential; body=%s", w.Code, w.Body.String())
		}
	})

	t.Run("a token minted after the cutoff still authenticates", func(t *testing.T) {
		srv, st, rev := build(t)
		cutoff := time.Now().UTC()
		rev.nowFunc = func() time.Time { return cutoff }
		if err := rev.RevokeAll(context.Background()); err != nil {
			t.Fatal(err)
		}
		// A NEW credential, minted from a session that itself cleared the
		// cutoff. Refusing this would make the lever a permanent lockout.
		mint(t, st, "wdn_after", "sub-alice", "alice@corp.example", cutoff.Add(time.Second))

		if w := do(t, srv, http.MethodGet, "/api/v1/me", "wdn_after", ""); w.Code != http.StatusOK {
			t.Errorf("token minted AFTER the cutoff = %d, want 200 — revoke-all is an incident lever, "+
				"not a permanent lockout; body=%s", w.Code, w.Body.String())
		}
	})

	t.Run("a targeted revoke reaches the named human's raced token", func(t *testing.T) {
		srv, st, rev := build(t)
		cutoff := time.Now().UTC()
		rev.nowFunc = func() time.Time { return cutoff }
		mint(t, st, "wdn_alice", "sub-alice", "alice@corp.example", cutoff.Add(-time.Millisecond))
		mint(t, st, "wdn_bob", "sub-bob", "bob@corp.example", cutoff.Add(-time.Millisecond))
		if err := rev.RevokeSub(context.Background(), "sub-alice"); err != nil {
			t.Fatal(err)
		}

		if w := do(t, srv, http.MethodGet, "/api/v1/me", "wdn_alice", ""); w.Code != http.StatusUnauthorized {
			t.Errorf("alice's raced token = %d, want 401; body=%s", w.Code, w.Body.String())
		}
		// The control that makes the arm above mean something: an untargeted
		// human is untouched.
		if w := do(t, srv, http.MethodGet, "/api/v1/me", "wdn_bob", ""); w.Code != http.StatusOK {
			t.Errorf("bob's token = %d, want 200 — a targeted revoke must not cut every principal; body=%s",
				w.Code, w.Body.String())
		}
	})

	// An unanswerable revocation check must never read as "not revoked".
	t.Run("a revocation-store outage fails closed", func(t *testing.T) {
		srv, st, rev := build(t)
		mint(t, st, "wdn_x", "sub-alice", "alice@corp.example", time.Now().UTC())
		rev.err = context.DeadlineExceeded
		if w := do(t, srv, http.MethodGet, "/api/v1/me", "wdn_x", ""); w.Code != http.StatusInternalServerError {
			t.Errorf("revocation-store outage = %d, want 500 — an unanswerable check must not authenticate; body=%s",
				w.Code, w.Body.String())
		}
	})
}
