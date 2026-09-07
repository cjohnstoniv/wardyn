// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

// heldBody is a request body whose first Read blocks until the test releases
// it: the client-controlled pause the API server has no ReadTimeout against
// (boot_serve.go sets none by design). `entered` closes the moment the server
// begins reading, which is the instant the request is past every gate and
// inside the mint handler.
type heldBody struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	payload []byte
	off     int
}

func newHeldBody(payload string) *heldBody {
	return &heldBody{entered: make(chan struct{}), release: make(chan struct{}), payload: []byte(payload)}
}

func (b *heldBody) Read(p []byte) (int, error) {
	b.once.Do(func() {
		close(b.entered)
		<-b.release
	})
	if b.off >= len(b.payload) {
		return 0, io.EOF
	}
	n := copy(p, b.payload[b.off:])
	b.off += n
	return n, nil
}

// TestAPITokenMintCannotOutliveTheLeverItRacedWith is F143's residue.
//
// The read-side cutoff check closed only the half of the window where the row's
// created_at happens to land at-or-before the cutoff. handleCreateAPIToken
// stamped created_at with s.cfg.Now() at INSERT time — after decodeStrict has
// read the request body — while the gate that authorized the mint ran in
// oidc.Middleware before the handler was ever entered, and the API server sets
// no ReadTimeout. A caller who holds the mint request's body open across POST
// /sessions/revoke therefore got created_at AFTER the cutoff: it escaped the
// sweep's ListAPITokens snapshot exactly as before, AND passed the new check.
// That is the finding's `actual` unchanged — a permanent wdn_ credential
// surviving the incident lever that answered 204.
//
// Driven through the real router, the real handleCreateAPIToken, the real
// handleRevokeSessions and the real apiTokenAuth, on a controlled clock so the
// ordering is exact rather than timing-dependent:
//
//	t0  the mint request is admitted and starts reading its body — and stops
//	t1  POST /sessions/revoke {"all":true} stamps the cutoff, sweeps, 204s
//	t2  the body is released; the INSERT commits
func TestAPITokenMintCannotOutliveTheLeverItRacedWith(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	var (
		clockMu sync.Mutex
		clock   = base
	)
	now := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return clock
	}
	advance := func(d time.Duration) {
		clockMu.Lock()
		defer clockMu.Unlock()
		clock = base.Add(d)
	}

	h := newHarness(t)
	st := newTokenMemStore()
	cfg := baseTestConfig(h, st)
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Now = now
	rev := newCutoffRevocations()
	rev.nowFunc = now // the cutoff is stamped from the same clock the mint reads
	cfg.SessionRevocations = rev
	srv := New(cfg)

	// The control that proves the sweep itself works: a token minted before the
	// incident, which the lever must kill.
	if _, err := st.CreateAPIToken(context.Background(), types.APIToken{
		ID: uuid.New(), Principal: "sub-alice", Email: "alice@corp.example",
		Role: string(oidc.RoleMember), Name: "pre-incident", CreatedAt: base,
	}, "wdn_preincident"); err != nil {
		t.Fatal(err)
	}

	// t0: alice's mint is admitted and blocks with its body half-sent.
	body := newHeldBody(`{"name":"ci"}`)
	type result struct{ rec *httptest.ResponseRecorder }
	done := make(chan result, 1)
	go func() {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/me/tokens", body)
		r.AddCookie(ssoSession(t, "sub-alice", "alice@corp.example", oidc.RoleMember))
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, r)
		done <- result{rec}
	}()
	select {
	case <-body.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the mint request never reached its body read")
	}

	// t1: the incident lever fires while that request is still in flight.
	advance(time.Second)
	if w := doSSO(t, srv, http.MethodPost, "/api/v1/sessions/revoke",
		ssoSession(t, "sub-super", "super@corp.example", oidc.RoleAdmin), `{"all":true}`); w.Code != http.StatusNoContent {
		t.Fatalf("revoke-all = %d, want 204; body=%s", w.Code, w.Body.String())
	}
	if w := do(t, srv, http.MethodGet, "/api/v1/me", "wdn_preincident", ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("the pre-incident control token = %d, want 401 — the lever itself is broken, so this test proves nothing", w.Code)
	}

	// t2: the held request finishes, long after the lever answered 204.
	advance(2 * time.Second)
	close(body.release)
	var rec *httptest.ResponseRecorder
	select {
	case r := <-done:
		rec = r.rec
	case <-time.After(10 * time.Second):
		t.Fatal("the mint request never completed")
	}

	switch rec.Code {
	case http.StatusForbidden:
		// The mint is refused outright: the cutoff was already committed when
		// the handler re-asked, so no dead credential is minted at all.
		if got := rec.Body.String(); !strings.Contains(got, apiTokenNoHumanRefusal) {
			t.Errorf("raced mint refused with %q; want the byte-identical no-signed-in-human sentence, "+
				"or this handler is an oracle for \"that revoke has landed\"", got)
		}
	case http.StatusCreated:
		var created struct {
			Token string `json:"token"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil || created.Token == "" {
			t.Fatalf("decode mint response: %v; body=%s", err, rec.Body.String())
		}
		if w := do(t, srv, http.MethodGet, "/api/v1/me", created.Token, ""); w.Code != http.StatusUnauthorized {
			t.Errorf("a wdn_ token minted by a request that held its body open across {\"all\":true} authenticates "+
				"with %d after the lever answered 204; want 401. api_tokens has no expiry and this row escaped both "+
				"the sweep's snapshot and the created_at cutoff, so it is a PERMANENT credential.", w.Code)
		}
	default:
		t.Fatalf("raced mint = %d, want 403 (refused) or 201 (minted dead); body=%s", rec.Code, rec.Body.String())
	}

	// The lever is not a permanent lockout: a mint admitted AFTER the cutoff
	// still works, so the close above cannot have been "refuse everything".
	advance(3 * time.Second)
	w := doSSO(t, srv, http.MethodPost, "/api/v1/me/tokens",
		ssoSession(t, "sub-alice", "alice@corp.example", oidc.RoleMember), `{"name":"after"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("a mint admitted after the cutoff = %d, want 201; body=%s", w.Code, w.Body.String())
	}
	var after struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &after); err != nil {
		t.Fatal(err)
	}
	if g := do(t, srv, http.MethodGet, "/api/v1/me", after.Token, ""); g.Code != http.StatusOK {
		t.Errorf("a token minted after the cutoff = %d, want 200 — revoke-all is an incident lever, not a lockout; body=%s",
			g.Code, g.Body.String())
	}
}

// TestAPITokenMintFailsClosedOnRevocationOutage: the mint's own revocation
// re-check is a security gate, so an unanswerable store must refuse the mint
// rather than read as "not revoked".
func TestAPITokenMintFailsClosedOnRevocationOutage(t *testing.T) {
	h := newHarness(t)
	cfg := baseTestConfig(h, newTokenMemStore())
	cfg.OIDC = &oidc.Authenticator{}
	rev := newCutoffRevocations()
	rev.err = context.DeadlineExceeded
	cfg.SessionRevocations = rev
	srv := New(cfg)

	w := doSSO(t, srv, http.MethodPost, "/api/v1/me/tokens",
		ssoSession(t, "sub-alice", "alice@corp.example", oidc.RoleMember), `{"name":"ci"}`)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("mint during a revocation-store outage = %d, want 500 — an unanswerable revocation check must "+
			"never mint a credential; body=%s", w.Code, w.Body.String())
	}
}
