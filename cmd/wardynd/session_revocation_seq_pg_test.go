// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// stepped is how long the modelled step of the host clock is: a cut written after the step carries a
// revoked_at this much earlier than the issue time of a credential issued before it.
const stepped = 800 * time.Millisecond

// isolatedRevocationPool is a migrated database of this test's own, for a test that watches the sequence.
func isolatedRevocationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	pool, err := db.Connect(ctx, emptyDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	return pool
}

func seqSub(t *testing.T, pool *pgxpool.Pool, prefix string) string {
	t.Helper()
	sub := prefix + uuid.NewString()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM oidc_session_revocations WHERE sub = $1`, sub)
		_, _ = pool.Exec(context.Background(), `DELETE FROM oidc_session_cuts WHERE sub = $1`, sub)
	})
	return sub
}

// cutAfterBackwardStep writes the revoke the way a clock that stepped back would stamp it: its revoked_at
// reads earlier than anything issued before it.
func cutAfterBackwardStep(t *testing.T, pool *pgxpool.Pool, sub string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO oidc_session_revocations (sub, revoked_at) VALUES ($1, clock_timestamp() - $2::interval)`,
		sub, stepped.String()); err != nil {
		t.Fatal(err)
	}
}

// TestPG_RevokeBeatsASessionIssuedBeforeABackwardClockStep models a step of the host clock (NTP, a VM
// resync) between a login and a revoke: the cookie's iat reads later than a cutoff stamped afterwards.
// The cookie recorded the revocation sequence at its issue, so the cut that drew a greater one ends it.
func TestPG_RevokeBeatsASessionIssuedBeforeABackwardClockStep(t *testing.T) {
	pool := revocationPool(t)
	ctx := context.Background()
	rev := &pgSessionRevocations{pool: pool, now: func() time.Time { return time.Now().Add(-stepped) }}
	sub := seqSub(t, pool, "auth0|clock-step-")

	seq, err := rev.RevocationSeq(ctx)
	if err != nil || seq < 1 {
		t.Fatalf("RevocationSeq = %d, %v; want a position of at least 1", seq, err)
	}
	issued := oidc.Issue{At: time.Now(), Seq: seq} // login, before the step
	time.Sleep(25 * time.Millisecond)
	if st, err := rev.SessionStatus(ctx, sub, "", issued, 0); err != nil || st != oidc.SessionLive {
		t.Fatalf("before any cut = %v, %v; want live", st, err)
	}
	cutAfterBackwardStep(t, pool, sub) // the revoke, after the step
	if st, err := rev.SessionStatus(ctx, sub, "", issued, 0); err != nil || st != oidc.SessionRevoked {
		t.Fatalf("a session issued before the revoke survived it because the clock stepped back between them: %v, %v", st, err)
	}
	if revoked, err := rev.IsSessionRevokedSeq(ctx, sub, "", seq); err != nil || !revoked {
		t.Fatalf("IsSessionRevokedSeq = %v, %v; want revoked", revoked, err)
	}
	if revoked, err := oidc.IsRevoked(ctx, rev, sub, "", issued); err != nil || !revoked {
		t.Fatalf("oidc.IsRevoked = %v, %v; want revoked", revoked, err)
	}

	// A session issued after the cut is not ended by it, whatever the stepped clock says of its time.
	after, err := rev.RevocationSeq(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st, err := rev.SessionStatus(ctx, sub, "", oidc.Issue{At: time.Now().Add(-time.Hour), Seq: after}, 0); err != nil || st != oidc.SessionLive {
		t.Fatalf("a session issued after the cut = %v, %v; want live", st, err)
	}
}

// An API token and an SSH key record the sequence in their row and are ordered by it.
func TestPG_RevokeBeatsATokenAndAKeyIssuedBeforeABackwardClockStep(t *testing.T) {
	pool := revocationPool(t)
	ctx := context.Background()
	rev := &pgSessionRevocations{pool: pool}
	st := store.NewPG(pool)
	sub := seqSub(t, pool, "auth0|clock-step-cred-")
	seq, err := rev.RevocationSeq(ctx)
	if err != nil {
		t.Fatal(err)
	}
	raw := "wdn_" + uuid.NewString()
	if _, err := st.CreateAPIToken(ctx, types.APIToken{ID: uuid.New(), Principal: sub, Role: "user", UserType: "standard", Name: "t", RevSeq: seq}, raw); err != nil {
		t.Fatal(err)
	}
	fp := "SHA256:" + uuid.NewString()
	if _, err := st.AddSSHKey(ctx, types.SSHPublicKey{Fingerprint: fp, Principal: sub, Name: "k", PublicKey: "ssh-ed25519 AAAA", Role: "member", CreatedAt: time.Now(), RevSeq: seq}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM api_tokens WHERE principal = $1`, sub)
		_, _ = pool.Exec(context.Background(), `DELETE FROM ssh_public_keys WHERE principal = $1`, sub)
	})
	tok, err := st.GetAPITokenByRaw(ctx, raw)
	if err != nil || tok.RevSeq != seq {
		t.Fatalf("token row = %+v, %v; want rev_seq %d", tok, err, seq)
	}
	key, err := st.GetSSHKeyByFingerprint(ctx, fp)
	if err != nil || key.RevSeq != seq {
		t.Fatalf("key row = %+v, %v; want rev_seq %d", key, err, seq)
	}
	time.Sleep(25 * time.Millisecond)
	cutAfterBackwardStep(t, pool, sub)
	// created_at reads after the cut's revoked_at: a time comparison would keep both.
	if !tok.CreatedAt.After(time.Now().Add(-stepped - time.Minute)) {
		t.Fatalf("test premise: token created_at %v", tok.CreatedAt)
	}
	if got, err := rev.SessionStatus(ctx, sub, "", oidc.Issue{At: tok.CreatedAt, Seq: tok.RevSeq}, -1); err != nil || got != oidc.SessionRevoked {
		t.Errorf("api token issued before the revoke = %v, %v; want revoked", got, err)
	}
	if got, err := rev.SessionStatus(ctx, sub, "", oidc.Issue{At: key.CreatedAt, Seq: key.RevSeq}, -1); err != nil || got != oidc.SessionRevoked {
		t.Errorf("ssh key registered before the revoke = %v, %v; want revoked", got, err)
	}
	// A session-only cut does not end either credential.
	other := seqSub(t, pool, "auth0|clock-step-cut-")
	if err := rev.CutSessions(ctx, other); err != nil {
		t.Fatal(err)
	}
	if got, err := rev.SessionStatus(ctx, other, "", oidc.Issue{At: time.Now(), Seq: seq}, -1); err != nil || got != oidc.SessionLive {
		t.Errorf("a session-only cut ended a token or key: %v, %v", got, err)
	}
}

// cutWaitsForAnIssuerReading runs cut while another connection holds the revocation-order lock shared, as an
// issuer reading the sequence does: the cut must wait for it, without having drawn its seq, and complete
// once it is released.
func cutWaitsForAnIssuerReading(t *testing.T, pool *pgxpool.Pool, cut func() error) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock_shared($1, 0)`, db.RevocationOrderLockClass); err != nil {
		t.Fatal(err)
	}
	lastValue := func() int64 {
		var n int64
		if err := pool.QueryRow(ctx, `SELECT last_value FROM oidc_revocation_seq`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := lastValue()
	done := make(chan error, 1)
	go func() { done <- cut() }()
	select {
	case err := <-done:
		t.Fatalf("a cut completed (%v) while an issuer held the revocation-order lock: it does not take it", err)
	case <-time.After(250 * time.Millisecond):
	}
	// The cut draws its seq only once it holds the lock: a draw made before it would sit below a value an
	// issuer reads next, which the lock exists to prevent. pool must be a database nothing else draws from.
	if now := lastValue(); now != before {
		t.Fatalf("the sequence moved from %d to %d while the cut waited for the lock: it drew its seq before taking it", before, now)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the cut never completed after the issuer released the lock")
	}
}

// Unbind, suspension, revoke-all and sign-out-everywhere each end a credential issued before them, with the
// credential's issue time read later than the cut's (the clock stepped back), and none ends one issued after.
func TestPG_EveryCutEndsAnEarlierCredentialByItsSequence(t *testing.T) {
	pool := isolatedRevocationPool(t) // cutWaitsForAnIssuerReading watches the sequence, which no other test may draw from
	ctx := context.Background()
	rev := &pgSessionRevocations{pool: pool}
	st := store.NewPG(pool)
	future := func() time.Time { return time.Now().Add(time.Minute) } // a stepped clock reads the credential later than the cut

	identity := func(principal string) store.PrincipalIdentity {
		t.Helper()
		row, err := st.UpsertLoginIdentity(ctx, store.LoginIdentity{Principal: principal, Issuer: "https://idp.example/" + principal,
			TenantID: uuid.NewString(), ObjectID: uuid.NewString()}, time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(), `DELETE FROM principal_identities WHERE id = $1`, row.ID)
		})
		return row
	}
	cases := []struct {
		name string
		// prepare returns the cut for sub, after any setup the cut needs.
		prepare func(sub string) func() error
		// session is true for a cut a token or key is not read against.
		session bool
	}{
		{"revoke", func(sub string) func() error { return func() error { return rev.RevokeSub(ctx, sub) } }, false},
		{"sign out everywhere", func(sub string) func() error { return func() error { return oidc.CutSessions(ctx, rev, sub) } }, true},
		{"suspension", func(sub string) func() error {
			row := identity(sub)
			return func() error {
				_, err := st.SuspendIdentity(ctx, store.SuspendPlan{IdentityID: row.ID, Principal: sub, Principals: []string{sub}, CutoffSubs: []string{sub}})
				return err
			}
		}, false},
		{"unbind", func(sub string) func() error {
			row := identity(sub)
			return func() error { return st.UnbindIdentity(ctx, row.ID, sub) }
		}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sub := seqSub(t, pool, "auth0|every-cut-")
			cut := c.prepare(sub)
			seq, err := rev.RevocationSeq(ctx)
			if err != nil {
				t.Fatal(err)
			}
			cutWaitsForAnIssuerReading(t, pool, cut)
			issued := oidc.Issue{At: future(), Seq: seq}
			if revoked, err := oidc.IsRevoked(ctx, rev, sub, "", issued); err != nil || !revoked {
				t.Fatalf("a session issued before the %s: IsRevoked = %v, %v; want revoked", c.name, revoked, err)
			}
			tokenStatus, err := rev.SessionStatus(ctx, sub, "", issued, -1)
			switch {
			case err != nil:
				t.Fatal(err)
			case c.session && tokenStatus != oidc.SessionLive:
				t.Fatalf("the %s ended a token or key: %v", c.name, tokenStatus)
			case !c.session && tokenStatus == oidc.SessionLive:
				t.Fatalf("a token or key issued before the %s = live; want ended", c.name)
			}
			after, err := rev.RevocationSeq(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if revoked, err := rev.IsSessionRevokedSeq(ctx, sub, "", after); err != nil || revoked {
				t.Fatalf("a session issued after the %s: revoked = %v, %v; want live", c.name, revoked, err)
			}
		})
	}

	t.Run("revoke all", func(t *testing.T) {
		sub := seqSub(t, pool, "auth0|every-cut-all-")
		seq, err := rev.RevocationSeq(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM oidc_session_revocations WHERE sub = ''`) })
		cutWaitsForAnIssuerReading(t, pool, func() error { return rev.RevokeAll(ctx) })
		if got, err := rev.SessionStatus(ctx, sub, "", oidc.Issue{At: future(), Seq: seq}, -1); err != nil || got != oidc.SessionRevoked {
			t.Fatalf("a credential issued before the revoke-all = %v, %v; want revoked", got, err)
		}
		after, _ := rev.RevocationSeq(ctx)
		if got, err := rev.SessionStatus(ctx, sub, "", oidc.Issue{At: time.Now().Add(-time.Hour), Seq: after}, -1); err != nil || got != oidc.SessionLive {
			t.Fatalf("a credential issued after the revoke-all = %v, %v; want live", got, err)
		}
	})
}

// A credential without a sequence (a cookie issued before it existed, or one a replica that predates it
// issued) is ordered on time as before: a later cut ends it and an earlier one does not.
func TestPG_LegacyCredentialWithoutASequenceIsStillEndedByALaterCut(t *testing.T) {
	pool := revocationPool(t)
	ctx := context.Background()
	rev := &pgSessionRevocations{pool: pool}
	sub := seqSub(t, pool, "auth0|legacy-")

	issued := time.Now().UTC()
	time.Sleep(25 * time.Millisecond)
	if err := rev.RevokeSub(ctx, sub); err != nil {
		t.Fatal(err)
	}
	if revoked, err := rev.IsSessionRevoked(ctx, sub, "", issued); err != nil || !revoked {
		t.Fatalf("legacy credential issued before the cut = %v, %v; want revoked", revoked, err)
	}
	if revoked, err := oidc.IsRevoked(ctx, rev, sub, "", oidc.Issue{At: issued}); err != nil || !revoked {
		t.Fatalf("oidc.IsRevoked without a sequence = %v, %v; want revoked", revoked, err)
	}
	if st, err := rev.SessionStatus(ctx, sub, "", oidc.Issue{}, 0); err != nil || st != oidc.SessionRevoked {
		t.Fatalf("a credential with neither a time nor a sequence = %v, %v; want revoked (fail closed)", st, err)
	}
	time.Sleep(25 * time.Millisecond)
	if revoked, err := rev.IsSessionRevoked(ctx, sub, "", time.Now().UTC()); err != nil || revoked {
		t.Fatalf("legacy credential issued after the cut = %v, %v; want live", revoked, err)
	}
}

// Rolling upgrade. A replica that predates the sequence writes its cut without drawing one and without the
// lock: the trigger draws it, so a credential that recorded a sequence is still ordered against that cut,
// including when the old statement updates an existing row. And a cookie an old replica issues carries no
// sequence, so a cut a new replica writes ends it on time, as before.
func TestPG_RollingUpgradeMixedReplicasStayFailClosed(t *testing.T) {
	pool := revocationPool(t)
	ctx := context.Background()
	rev := &pgSessionRevocations{pool: pool}

	t.Run("an old replica cuts", func(t *testing.T) {
		sub := seqSub(t, pool, "auth0|rolling-old-cut-")
		seq, err := rev.RevocationSeq(ctx)
		if err != nil {
			t.Fatal(err)
		}
		const oldStatement = `INSERT INTO oidc_session_revocations (sub, revoked_at) VALUES ($1, clock_timestamp())
			ON CONFLICT (sub) DO UPDATE SET revoked_at = EXCLUDED.revoked_at`
		if _, err := pool.Exec(ctx, oldStatement, sub); err != nil {
			t.Fatal(err)
		}
		var first int64
		if err := pool.QueryRow(ctx, `SELECT seq FROM oidc_session_revocations WHERE sub = $1`, sub).Scan(&first); err != nil || first <= seq {
			t.Fatalf("an insert that names no seq drew %d (%v); want one above %d", first, err, seq)
		}
		if st, _ := rev.SessionStatus(ctx, sub, "", oidc.Issue{At: time.Now().Add(time.Minute), Seq: seq}, -1); st != oidc.SessionRevoked {
			t.Fatalf("a new credential survived a cut an old replica wrote: %v", st)
		}
		fresh, _ := rev.RevocationSeq(ctx)
		if _, err := pool.Exec(ctx, oldStatement, sub); err != nil {
			t.Fatal(err)
		}
		if st, _ := rev.SessionStatus(ctx, sub, "", oidc.Issue{At: time.Now().Add(time.Minute), Seq: fresh}, -1); st != oidc.SessionRevoked {
			t.Fatalf("a new credential survived an old replica's update of an existing cut: %v", st)
		}
	})

	t.Run("an old replica issues", func(t *testing.T) {
		sub := seqSub(t, pool, "auth0|rolling-old-issue-")
		issued := time.Now().UTC() // a cookie with no rseq
		time.Sleep(25 * time.Millisecond)
		if err := rev.RevokeSub(ctx, sub); err != nil { // the new replica's cut
			t.Fatal(err)
		}
		if st, err := rev.SessionStatus(ctx, sub, "", oidc.Issue{At: issued}, 0); err != nil || st != oidc.SessionRevoked {
			t.Fatalf("an unsequenced cookie after a new replica's cut = %v, %v; want revoked", st, err)
		}
	})
}

// A cut holds the revocation-order lock from before it draws its sequence until it commits, and an issuer
// reads the sequence under the shared lock: the value an issuer reads can never sit below a cut that is
// still uncommitted when the read returns.
func TestPG_IssueWaitsForACutThatHasDrawnItsSequence(t *testing.T) {
	pool := revocationPool(t)
	ctx := context.Background()
	rev := &pgSessionRevocations{pool: pool}
	sub := seqSub(t, pool, "auth0|issue-vs-cut-")

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := store.LockRevocationOrder(ctx, tx); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO oidc_session_revocations (sub, revoked_at) VALUES ($1, now())`, sub); err != nil {
		t.Fatal(err)
	}
	var drawn int64
	if err := tx.QueryRow(ctx, `SELECT seq FROM oidc_session_revocations WHERE sub = $1`, sub).Scan(&drawn); err != nil {
		t.Fatal(err)
	}

	read := make(chan int64, 1)
	go func() {
		seq, err := rev.RevocationSeq(ctx)
		if err != nil {
			seq = -1
		}
		read <- seq
	}()
	select {
	case seq := <-read:
		t.Fatalf("an issuer read %d while a cut that drew %d was uncommitted: the credential would record a value the cut does not exceed", seq, drawn)
	case <-time.After(300 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case seq := <-read:
		if seq < drawn {
			t.Fatalf("issuer read %d after the cut committed; want at least its %d", seq, drawn)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the issuer never returned after the cut committed")
	}
}

// Issuers and cuts race. An issuer whose read finished before a cut began is ended by it; one whose read
// began after the cut returned is not; the rest may go either way, but never differently on a second look.
func TestPG_IssueRacingCutKeepsTheOrder(t *testing.T) {
	pool := revocationPool(t)
	ctx := context.Background()
	rev := &pgSessionRevocations{pool: pool}
	sub := seqSub(t, pool, "auth0|race-")

	type issued struct {
		seq        int64
		start, end time.Time
	}
	var mu sync.Mutex
	var all []issued
	var cutStart, cutEnd time.Time
	var stop atomic.Bool
	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				start := time.Now()
				seq, err := rev.RevocationSeq(ctx)
				end := time.Now()
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				all = append(all, issued{seq, start, end})
				mu.Unlock()
			}
		}()
	}
	time.Sleep(150 * time.Millisecond)
	cutStart = time.Now()
	if err := rev.RevokeSub(ctx, sub); err != nil {
		t.Fatal(err)
	}
	cutEnd = time.Now()
	time.Sleep(150 * time.Millisecond)
	stop.Store(true)
	wg.Wait()

	var before, after int
	for _, i := range all {
		revoked, err := rev.IsSessionRevokedSeq(ctx, sub, "", i.seq)
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case i.end.Before(cutStart):
			before++
			if !revoked {
				t.Fatalf("a credential whose sequence was read (at %v) before the cut began (%v) survived it", i.end, cutStart)
			}
		case i.start.After(cutEnd):
			after++
			if revoked {
				t.Fatalf("a credential whose sequence was read (at %v) after the cut returned (%v) was ended by it", i.start, cutEnd)
			}
		}
	}
	if before == 0 || after == 0 {
		t.Fatalf("the race never produced both sides: %d before, %d after", before, after)
	}
}
