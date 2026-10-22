// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/testutil"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// sponsoredService is a service identity sponsored by the queue environment's person, and the browser
// session the sponsor signed in with before anything the test does: it carries the revocation sequence
// of that moment.
func (e *queueEnv) sponsoredService(rev *pgTestRevocations) (types.ServiceIdentity, *http.Cookie) {
	e.t.Helper()
	ctx := context.Background()
	epoch, err := e.pg.IssueLoginIdentity(ctx, store.LoginIdentity{Principal: e.personP, Issuer: "https://idp.example", TenantID: "t", ObjectID: "o-1", Email: "person@corp.example"}, time.Now())
	if err != nil {
		e.t.Fatal(err)
	}
	si, err := e.pg.CreateServiceIdentity(ctx, types.ServiceIdentity{Name: "ci-queue", Sponsor: e.personP, UserType: types.UserTypeStandard, CreatedBy: "seed"})
	if err != nil {
		e.t.Fatal(err)
	}
	ageFreshSponsorships(e.t, e.pg.Pool) // see mintReq: a mint right after the create must not race sponsor_since
	seq, err := rev.RevocationSeq(ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	payload, err := json.Marshal(oidc.Session{V: oidc.SessionCodecVersion, Sub: e.personP, Email: "person@corp.example", Role: oidc.RoleUser,
		UserType: types.UserTypeStandard, Expiry: time.Now().UTC().Add(time.Hour), IssuedAt: time.Now().UTC(), AuthorityEpoch: epoch, RevocationSeq: seq})
	if err != nil {
		e.t.Fatal(err)
	}
	return si, signedSessionCookie(payload)
}

func (e *queueEnv) mintServiceToken(c *http.Cookie, si types.ServiceIdentity) *httptest.ResponseRecorder {
	return e.call(c, http.MethodPost, "/api/v1/me/service-identities/"+si.Name+"/tokens", `{"name":"ci","ttl_seconds":3600}`)
}

// A service identity's credentials are revoked by its own principal, and its sponsor then mints a
// replacement from the browser session they already had. The replacement records the sequence read at its
// mint, which is above that revoke, so a launch it queues starts when the slot frees: the revoke ended the
// tokens that existed, not the one minted after it.
func TestPG_ReplacementServiceTokenAfterARevokeOfItsPrincipalStartsItsQueuedRun(t *testing.T) {
	e := newQueueEnv(t)
	rev := e.revocations()
	pool := e.cappedPool("Busy", 1)
	si, cookie := e.sponsoredService(rev)
	w := e.mintServiceToken(cookie, si)
	if w.Code != http.StatusCreated {
		t.Fatalf("mint the first token = %d %s", w.Code, w.Body)
	}
	leaked := decodeToken(t, w)

	rw := httptest.NewRecorder()
	e.srv.handleRevokeSessions(rw, httptest.NewRequest(http.MethodPost, "/api/v1/sessions/revoke", strings.NewReader(`{"sub":"`+si.Principal()+`"}`)))
	var cutSeq int64
	if err := e.pool.QueryRow(context.Background(), `SELECT seq FROM oidc_session_revocations WHERE sub = $1`, si.Principal()).Scan(&cutSeq); err != nil {
		t.Fatalf("the revoke of the service principal (%d %s) left no cut: %v", rw.Code, rw.Body, err)
	}
	if w := do(t, e.srv, http.MethodGet, "/api/v1/me", leaked.Token, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("the token that existed at the revoke still answers %d, want 401", w.Code)
	}

	w = e.mintServiceToken(cookie, si)
	if w.Code != http.StatusCreated {
		t.Fatalf("mint the replacement = %d %s", w.Code, w.Body)
	}
	tok := decodeToken(t, w)
	if seq := tokenRevSeq(t, e.pool, tok.ID); seq < cutSeq {
		t.Errorf("the replacement recorded sequence %d, below the revoke's %d that preceded it", seq, cutSeq)
	}
	if w := do(t, e.srv, http.MethodGet, "/api/v1/me", tok.Token, ""); w.Code != http.StatusOK {
		t.Fatalf("the replacement does not authenticate: %d %s", w.Code, w.Body)
	}
	occupant := e.occupy(pool)
	w = do(t, e.srv, http.MethodPost, "/api/v1/runs", tok.Token, endBody("background", `"runner_pool_id":"`+pool.ID.String()+`"`))
	var out queuedLaunchAnswer
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != http.StatusCreated || out.State != types.RunQueued {
		t.Fatalf("the replacement's launch into a full pool = %d %s; want 201 QUEUED", w.Code, w.Body)
	}
	e.free(occupant)
	if got := e.waitStarted(out.ID); got.SPIFFEID == "" {
		t.Errorf("the replacement's queued run is %s %q with nothing minted; want it started", got.State, got.StatusDetail)
	}
}

// cutAtTheRead is the revocation store with one cut of sub's sessions placed just before, or just after,
// the next read of the revocation sequence.
type cutAtTheRead struct {
	*pgTestRevocations
	sub    string
	before bool
	armed  atomic.Bool
}

func (r *cutAtTheRead) RevocationSeq(ctx context.Context) (int64, error) {
	fire := r.armed.CompareAndSwap(true, false)
	if fire && r.before {
		if err := r.pgTestRevocations.RevokeSub(ctx, r.sub); err != nil {
			return 0, err
		}
	}
	seq, err := r.pgTestRevocations.RevocationSeq(ctx)
	if fire && !r.before && err == nil {
		err = r.pgTestRevocations.RevokeSub(ctx, r.sub)
	}
	return seq, err
}

// A revoke of the sponsor that lands while a service token is being minted never leaves a working token.
// One the token's sequence read covers has committed before the late re-check of the sponsor's session,
// which refuses the mint; one that lands right after the read is refused by the same re-check, and would
// order above the token had the mint finished.
func TestPG_ServiceTokenMintRacingARevokeOfItsSponsorLeavesNoWorkingToken(t *testing.T) {
	for _, tc := range []struct {
		name   string
		before bool
	}{
		{"the revoke commits just before the sequence is read", true},
		{"the revoke commits just after the sequence is read", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newQueueEnv(t)
			si, cookie := e.sponsoredService(e.revocations())
			racing := &cutAtTheRead{pgTestRevocations: &pgTestRevocations{pool: e.pool, st: e.pg}, sub: e.personP, before: tc.before}
			e.srv.cfg.SessionRevocations = racing
			racing.armed.Store(true)
			w := e.mintServiceToken(cookie, si)
			if racing.armed.Load() {
				t.Fatal("the mint never read the revocation sequence, so the revoke was not placed")
			}
			if w.Code == http.StatusCreated {
				if tok := decodeToken(t, w); do(t, e.srv, http.MethodGet, "/api/v1/me", tok.Token, "").Code == http.StatusOK {
					t.Fatalf("a service token minted across a revoke of its sponsor works (rev_seq %d)", tokenRevSeq(t, e.pool, tok.ID))
				}
			}
			if w.Code != http.StatusForbidden {
				t.Errorf("the mint = %d %s; want the late re-check's 403", w.Code, w.Body)
			}
		})
	}
}

// A revoke of the sponsor still in flight when a service token is minted (it holds the revocation order
// lock and has written its cut, but not committed) holds the mint's sequence read until it commits; the
// late re-check then sees the cut and refuses the mint, so no working token comes out of it.
func TestPG_ServiceTokenMintBehindAnInFlightRevokeOfItsSponsorIsRefused(t *testing.T) {
	e := newQueueEnv(t)
	si, cookie := e.sponsoredService(e.revocations())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck // committed below on the happy path
	var pid int
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1, 0)`, db.RevocationOrderLockClass); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO oidc_session_revocations (sub, revoked_at) VALUES ($1, clock_timestamp())
		ON CONFLICT (sub) DO UPDATE SET revoked_at = EXCLUDED.revoked_at`, e.personP); err != nil {
		t.Fatal(err)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- e.mintServiceToken(cookie, si) }()
	watch := testutil.PGConn(t, e.pool) // a separate session: polling through the pool would nest under tx
	for waiting := false; !waiting; {
		if err := watch.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid)))`, pid).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		select {
		case w := <-done:
			t.Fatalf("the mint finished (%d %s) without waiting behind the revoke in flight", w.Code, w.Body)
		case <-ctx.Done():
			t.Fatal("the mint never waited behind the revoke in flight")
		case <-time.After(5 * time.Millisecond):
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	w := <-done
	if w.Code == http.StatusCreated {
		if tok := decodeToken(t, w); do(t, e.srv, http.MethodGet, "/api/v1/me", tok.Token, "").Code == http.StatusOK {
			t.Fatalf("a service token minted behind a revoke of its sponsor in flight works (rev_seq %d)", tokenRevSeq(t, e.pool, tok.ID))
		}
	}
	if w.Code != http.StatusForbidden {
		t.Errorf("the mint = %d %s; want the late re-check's 403", w.Code, w.Body)
	}
}

// A revoke by the sponsor's email reaches the tokens of the service they sponsor even when SSH keys are
// registered under the email itself: the sponsor is stored by subject, so the sweep follows the subject
// the email names in the directory, not the key owner.
func TestPG_RevokeBySponsorEmailThatOwnsKeysRevokesTheServiceTokens(t *testing.T) {
	e := newQueueEnv(t)
	si, cookie := e.sponsoredService(e.revocations())
	w := e.mintServiceToken(cookie, si)
	if w.Code != http.StatusCreated {
		t.Fatalf("mint = %d %s", w.Code, w.Body)
	}
	tok := decodeToken(t, w)
	const email = "person@corp.example"
	if _, err := e.pg.AddSSHKey(context.Background(), types.SSHPublicKey{Fingerprint: "SHA256:email-owned-key", Principal: email, Name: "k",
		PublicKey: "ssh-ed25519 AAAA " + email, Role: oidc.RoleUser, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}

	rw := httptest.NewRecorder()
	e.srv.handleRevokeSessions(rw, httptest.NewRequest(http.MethodPost, "/api/v1/sessions/revoke", strings.NewReader(`{"sub":"`+email+`"}`)))
	var revoked bool
	if err := e.pool.QueryRow(context.Background(), `SELECT revoked_at IS NOT NULL FROM api_tokens WHERE id = $1`, tok.ID).Scan(&revoked); err != nil {
		t.Fatal(err)
	}
	if !revoked {
		t.Errorf("the revoke by %s (%d %s) left the sponsored service's token live", email, rw.Code, rw.Body)
	}
}

// revokeBeforeServiceInsert holds a service-token insert until revoke has run to completion: the mint has
// passed its late re-check of the sponsor and not yet inserted the token.
type revokeBeforeServiceInsert struct {
	store.PG
	revoke func()
}

func (s revokeBeforeServiceInsert) CreateServiceToken(ctx context.Context, name, sponsor string, maxLive int, t types.APIToken, raw string) (types.APIToken, error) {
	s.revoke()
	return s.PG.CreateServiceToken(ctx, name, sponsor, maxLive, t, raw)
}

// A revoke by the sponsor's email, with SSH keys registered under the email itself, that lands between a
// service-token mint's late re-check and its insert leaves no working token: the cut reaches the subject
// the email names, the one the token's sponsor check reads.
func TestPG_RevokeBySponsorEmailThatOwnsKeysEndsATokenMintedAcrossIt(t *testing.T) {
	e := newQueueEnv(t)
	si, cookie := e.sponsoredService(e.revocations())
	const email = "person@corp.example"
	if _, err := e.pg.AddSSHKey(context.Background(), types.SSHPublicKey{Fingerprint: "SHA256:email-owned-key", Principal: email, Name: "k",
		PublicKey: "ssh-ed25519 AAAA " + email, Role: oidc.RoleUser, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	var rw *httptest.ResponseRecorder
	e.srv.cfg.Store = revokeBeforeServiceInsert{PG: e.pg, revoke: func() {
		rw = httptest.NewRecorder()
		e.srv.handleRevokeSessions(rw, httptest.NewRequest(http.MethodPost, "/api/v1/sessions/revoke", strings.NewReader(`{"sub":"`+email+`"}`)))
	}}
	w := e.mintServiceToken(cookie, si)
	e.srv.cfg.Store = e.pg
	if rw == nil || rw.Code/100 != 2 {
		t.Fatalf("the revoke did not complete inside the mint (%v); mint = %d %s", rw, w.Code, w.Body)
	}
	if w.Code != http.StatusCreated {
		t.Fatalf("mint = %d %s, want the insert to land after the revoke", w.Code, w.Body)
	}
	if tok := decodeToken(t, w); do(t, e.srv, http.MethodGet, "/api/v1/me", tok.Token, "").Code != http.StatusUnauthorized {
		t.Fatalf("a service token minted across a revoke by its sponsor's email works (rev_seq %d)", tokenRevSeq(t, e.pool, tok.ID))
	}
}
