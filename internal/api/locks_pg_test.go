// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The cross-replica locks against a real Postgres, with TWO servers on two
// connection pools to one database: the shape of two replicas. A single-server
// test cannot show any of this, since two goroutines in one process shared a
// mutex before and the in-process locker still serializes them.
//
// Guarded by WARDYN_TEST_PG (via throwawayPGPool): skipped cleanly when unset,
// must PASS when set.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"filippo.io/age"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	secretspg "github.com/cjohnstoniv/wardyn/internal/secretstore/pg"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

// replicaPair is two servers over one database, each with its own pool (so its
// own lock connections) and its own secret-store handle.
type replicaPair struct {
	a, b         *Server
	poolA, poolB *pgxpool.Pool
	audit        *memAudit
}

func newReplicaPair(t *testing.T) *replicaPair {
	t.Helper()
	poolA := throwawayPGPool(t)
	poolB, err := db.Connect(context.Background(), poolA.Config().ConnString())
	if err != nil {
		t.Fatalf("second pool: %v", err)
	}
	t.Cleanup(poolB.Close)
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	p := &replicaPair{poolA: poolA, poolB: poolB, audit: &memAudit{}}
	mk := func(pool *pgxpool.Pool) *Server {
		sec, err := secretspg.New(pool, id)
		if err != nil {
			t.Fatal(err)
		}
		return &Server{cfg: Config{
			Store:        store.NewPG(pool),
			Secrets:      sec,
			MaskRegistry: secretmask.NewRegistry(),
			Now:          func() time.Time { return awsSSOTestFixedNow },
			Audit:        p.audit,
		}}
	}
	p.a, p.b = mk(poolA), mk(poolB)
	return p
}

// killLockHolder terminates the backend holding an advisory lock of class in
// this database: a failover, as the lock holder sees it.
func killLockHolder(t *testing.T, pool *pgxpool.Pool, class int32) {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM (
		SELECT pg_terminate_backend(pid) FROM pg_locks
		WHERE locktype = 'advisory' AND objsubid = 2 AND classid::bigint = $1
		  AND database = (SELECT oid FROM pg_database WHERE datname = current_database())) t`,
		int64(uint32(class))).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("terminated %d backends holding a lock of class %#x, want 1", n, uint32(class))
	}
}

// spendOnceOIDC is an SSO-OIDC endpoint that honours a refresh token once: it
// answers the first redemption (after a pause, so that two unserialized callers
// overlap) with a rotated pair, and every later redemption of the same token
// with invalid_grant, as AWS does.
func spendOnceOIDC(t *testing.T) (calls *atomic.Int32, refused *atomic.Int32) {
	t.Helper()
	var mu sync.Mutex
	spent := map[string]bool{}
	refused = new(atomic.Int32)
	calls = fakeOIDC(t, func(w http.ResponseWriter, body map[string]string, _ int) {
		mu.Lock()
		again := spent[body["refreshToken"]]
		spent[body["refreshToken"]] = true
		mu.Unlock()
		if again {
			refused.Add(1)
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid_grant"})
			return
		}
		time.Sleep(300 * time.Millisecond)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"accessToken": "fresh-access-token-abcdefghij", "expiresIn": 3600, "refreshToken": "rotated-refresh-token-abcdefghij"})
	})
	return calls, refused
}

// passthroughLocker is the in-process mutex's cross-replica behaviour: it
// serializes nothing. The control that shows the tests below can fail.
type passthroughLocker struct{}

func (passthroughLocker) Lock(ctx context.Context, _ db.LockKey, _ time.Duration) (context.Context, func(), error) {
	return ctx, func() {}, nil
}

func (passthroughLocker) TryLock(ctx context.Context, _ db.LockKey) (context.Context, func(), bool, error) {
	return ctx, func() {}, true, nil
}

// A concurrent AWS SSO refresh on two servers sharing one database spends the
// refresh token once.
func TestPG_AWSRefresh_ConcurrentTwoServersSpendTheRefreshTokenOnce(t *testing.T) {
	for _, c := range []struct {
		name   string
		locker db.Locker
		serial bool
	}{
		{"cross-replica locks", nil, true},
		{"control: no lock double-spends", passthroughLocker{}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := newReplicaPair(t)
			if c.locker != nil {
				p.a.locks.override, p.b.locks.override = c.locker, c.locker
			}
			blob := putAWSSSOBlob(t, p.a, awsSSOTestFixedNow.Add(-time.Minute)) // expired: both must renew
			calls, refused := spendOnceOIDC(t)

			var wg sync.WaitGroup
			msgs := make([]string, 2)
			for i, s := range []*Server{p.a, p.b} {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_, msgs[i] = s.refreshAWSSSOBlob(context.Background(), awsSSOTestScope(), blob)
				}()
			}
			wg.Wait()

			if !c.serial {
				if calls.Load() < 2 || refused.Load() == 0 {
					t.Fatalf("control: %d CreateToken calls, %d refused; want a double spend, or the test proves nothing", calls.Load(), refused.Load())
				}
				return
			}
			if n := calls.Load(); n != 1 {
				t.Errorf("CreateToken was called %d times for one refresh token, want 1", n)
			}
			if n := refused.Load(); n != 0 {
				t.Errorf("%d redemptions were refused as spent: the token was redeemed twice", n)
			}
			for i, m := range msgs {
				if m != "" {
					t.Errorf("server %d refused its caller: %q", i, m)
				}
			}
			if got := storedSSOBlob(t, p.a); got.RefreshToken != "rotated-refresh-token-abcdefghij" {
				t.Errorf("stored refresh token = %q, want the rotated one", got.RefreshToken)
			}
		})
	}
}

// delayedPutSecrets holds each Put for a moment, so two redemptions that read
// the same sign-in overlap before either writes. Revision is forwarded: the
// compare-and-set must keep working through it.
type delayedPutSecrets struct {
	secretstore.Store
	delay time.Duration
}

func (d delayedPutSecrets) For(owner string) secretstore.Store {
	return delayedPutSecrets{Store: d.Store.For(owner), delay: d.delay}
}

func (d delayedPutSecrets) Put(ctx context.Context, name string, value []byte) error {
	time.Sleep(d.delay)
	return d.Store.Put(ctx, name, value)
}

func (d delayedPutSecrets) Revision(ctx context.Context, name string) (string, error) {
	r, ok := d.Store.(secretstore.Revisioned)
	if !ok {
		return "", secretstore.ErrNoRevision
	}
	return r.Revision(ctx, name)
}

// A concurrent Azure DevOps sign-in refresh on two servers sharing one database
// spends the refresh token once: every redemption reads the token the previous
// one stored, none is refused as spent.
func TestPG_ADOEntraRefresh_ConcurrentTwoServersSpendTheRefreshTokenOnce(t *testing.T) {
	for _, c := range []struct {
		name   string
		locker db.Locker
	}{
		{"cross-replica locks", nil},
		{"control: no lock double-spends", passthroughLocker{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := newReplicaPair(t)
			f := newADOFixture(t)
			subject := f.fake.Subject()
			p.a.cfg.ADOEntra, p.b.cfg.ADOEntra = f.srv.cfg.ADOEntra, f.srv.cfg.ADOEntra
			p.a.cfg.Now, p.b.cfg.Now = f.srv.cfg.Now, f.srv.cfg.Now
			f.srv = p.a
			if w := f.capture(t, subject); w.Code != http.StatusFound {
				t.Fatalf("capture: status %d body %q", w.Code, w.Body.String())
			}
			for _, s := range []*Server{p.a, p.b} {
				s.cfg.Secrets = delayedPutSecrets{Store: s.cfg.Secrets, delay: 200 * time.Millisecond}
				if c.locker != nil {
					s.locks.override = c.locker
				}
			}

			var wg sync.WaitGroup
			var refusedAsDead atomic.Int32
			for i := range 6 {
				s := []*Server{p.a, p.b}[i%2]
				wg.Add(1)
				go func() {
					defer wg.Done()
					if _, err := s.RedeemADOEntraAccess(context.Background(), f.cfg, subject, f.cfg.Scopes); errors.Is(err, ErrADOEntraDeadCredential) {
						refusedAsDead.Add(1)
					} else if err != nil && c.locker == nil {
						t.Errorf("redeem: %v", err)
					}
				}()
			}
			wg.Wait()

			if c.locker != nil {
				if refusedAsDead.Load() == 0 {
					t.Fatal("control: no redemption was refused as spent, so the test proves nothing")
				}
				return
			}
			if n := refusedAsDead.Load(); n != 0 {
				t.Errorf("%d redemptions were refused as spent: a rotating refresh token was redeemed twice", n)
			}
			got, found := f.stored(t, subject)
			if !found {
				t.Fatal("the credential vanished")
			}
			if live, known := f.fake.RefreshTokenState(got.RefreshToken); !known || !live {
				t.Errorf("the stored refresh token is not live (live=%v known=%v)", live, known)
			}
		})
	}
}

// The nested credential erase (AWS lock, then the sign-in lock) completes on a
// lock pool with ONE connection: the inner lock reuses the outer's.
func TestPG_CredentialErase_NestsOnOneLockConnection(t *testing.T) {
	p := newReplicaPair(t)
	p.a.locks.override = db.NewPGLocker(p.poolA, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	const owner, rowID = awsSSOTestOwner, "ado-row-erase"
	putAWSSSOBlob(t, p.a, awsSSOTestFixedNow.Add(time.Hour))
	if err := p.a.storeADOEntraBlob(ctx, owner, rowID, adoEntraBlob{
		RefreshToken: "alices-refresh-token", Scopes: []string{"vso.code"},
		TenantID: "tenant", ClientID: "client", Subject: owner,
	}); err != nil {
		t.Fatal(err)
	}

	// A second connection for the inner lock would wait out the one-connection
	// pool and be refused; the context bounds the test if the erase hangs.
	var rep secretstore.EraseReport
	if err := p.a.eraseLocked(ctx, owner, rowID, &rep); err != nil {
		t.Fatalf("erase: %v", err)
	}
	if rep.Count < 2 {
		t.Errorf("the erase deleted %d credentials, want both the AWS SSO session and the Azure DevOps sign-in", rep.Count)
	}
	if _, found, _ := p.a.readAWSSSOBlob(ctx, awsSSOTestScope()); found {
		t.Error("the AWS SSO credential survived the erase")
	}
	if _, found, _ := p.a.readADOEntraBlob(ctx, owner, rowID); found {
		t.Error("the Azure DevOps sign-in survived the erase")
	}
}

// A lock lost mid-refresh does not overwrite a newer stored token: the refresh
// reads the row, loses its lock, a newer pair lands, and the refresh's final Put
// (compare-and-set on the row it read) leaves that pair alone.
func TestPG_AWSRefresh_LostLockDoesNotOverwriteANewerToken(t *testing.T) {
	old := db.LockWatchInterval
	db.LockWatchInterval = time.Hour // the watcher stays out of it: the compare-and-set is under test
	t.Cleanup(func() { db.LockWatchInterval = old })
	p := newReplicaPair(t)
	blob := putAWSSSOBlob(t, p.a, awsSSOTestFixedNow.Add(-time.Minute))

	newer := blob
	newer.AccessToken, newer.RefreshToken = "newer-access-token-1234567890", "newer-refresh-token-1234567890"
	newer.ExpiresAt = awsSSOTestFixedNow.Add(2 * time.Hour)
	fakeOIDC(t, func(w http.ResponseWriter, _ map[string]string, _ int) {
		// The redemption is in flight, under the lock. The lock's connection
		// dies, and another replica refreshes and stores its pair.
		killLockHolder(t, p.poolA, db.AWSSSOLockClass)
		storeSSOBlob(t, p.b, newer)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"accessToken": "stale-access-token-abcdefghij", "expiresIn": 3600, "refreshToken": "stale-refresh-token-abcdefghij"})
	})

	got, msg := p.a.refreshAWSSSOBlob(context.Background(), awsSSOTestScope(), blob)
	if msg != "" {
		t.Fatalf("refresh refused: %q", msg)
	}
	if got.AccessToken != "stale-access-token-abcdefghij" {
		t.Errorf("the redeemed pair is not served to its caller: %q", got.AccessToken)
	}
	if after := storedSSOBlob(t, p.b); after != newer {
		t.Errorf("the lost-lock refresh overwrote the newer stored pair: %+v", after)
	}
	var superseded bool
	for _, e := range p.audit.rows {
		var d map[string]any
		_ = json.Unmarshal(e.Data, &d)
		superseded = superseded || d["superseded"] == true
	}
	if !superseded {
		t.Error("the refresh did not record that it was superseded")
	}
}

// A lock lost mid-refresh cancels the guarded work: the redemption in flight is
// abandoned instead of running on without the lock.
func TestPG_AWSRefresh_LostLockCancelsTheRedemption(t *testing.T) {
	old := db.LockWatchInterval
	db.LockWatchInterval = 20 * time.Millisecond
	t.Cleanup(func() { db.LockWatchInterval = old })
	p := newReplicaPair(t)
	blob := putAWSSSOBlob(t, p.a, awsSSOTestFixedNow.Add(8*time.Minute)) // inside the skew, still servable

	cancelled := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		// The body is read first: the server only watches for a client that went
		// away once the handler has consumed it.
		_, _ = io.Copy(io.Discard, r.Body)
		killLockHolder(t, p.poolA, db.AWSSSOLockClass)
		select {
		case <-time.After(5 * time.Second):
		case <-r.Context().Done():
			cancelled <- struct{}{}
		}
	}))
	t.Cleanup(srv.Close)
	prev := awsSSOTokenURL
	awsSSOTokenURL = func(string) string { return srv.URL + "/token" }
	t.Cleanup(func() { awsSSOTokenURL = prev })
	got, msg := p.a.refreshAWSSSOBlob(context.Background(), awsSSOTestScope(), blob)
	if msg != "" || got.AccessToken != blob.AccessToken {
		t.Fatalf("refresh = %+v %q, want the token in hand served", got, msg)
	}
	// The server sees the cancelled request a moment after the client gives up,
	// so wait for it; an uncancelled handler sends nothing for 5s.
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Error("the redemption in flight was not cancelled when the lock was lost")
	}
	if after := storedSSOBlob(t, p.a); after != blob {
		t.Errorf("a refresh that lost its lock wrote the store: %+v", after)
	}
}

// beforePutSecrets runs hook once, just before the first Put reaches the store:
// the instant between a redemption and the write of its rotation. Revision is
// forwarded, so the compare-and-set stays in force through it.
type beforePutSecrets struct {
	secretstore.Store
	hook *func()
}

func (b beforePutSecrets) For(owner string) secretstore.Store {
	return beforePutSecrets{Store: b.Store.For(owner), hook: b.hook}
}

func (b beforePutSecrets) Put(ctx context.Context, name string, value []byte) error {
	if h := *b.hook; h != nil {
		*b.hook = nil
		h()
	}
	return b.Store.Put(ctx, name, value)
}

func (b beforePutSecrets) Revision(ctx context.Context, name string) (string, error) {
	return b.Store.(secretstore.Revisioned).Revision(ctx, name)
}

// The same for the Azure DevOps sign-in: a redemption that lost its lock before
// storing its rotation leaves the newer sign-in another replica stored.
func TestPG_ADOEntraRefresh_LostLockDoesNotOverwriteANewerToken(t *testing.T) {
	old := db.LockWatchInterval
	db.LockWatchInterval = time.Hour // the watcher stays out of it: the compare-and-set is under test
	t.Cleanup(func() { db.LockWatchInterval = old })
	p := newReplicaPair(t)
	f := newADOFixture(t)
	subject := f.fake.Subject()
	f.srv = p.a
	p.a.cfg.ADOEntra, p.a.cfg.Now = func(context.Context) (ADOEntraConfig, bool, error) { return f.cfg, true, nil }, func() time.Time { return adoTestNow }
	if w := f.capture(t, subject); w.Code != http.StatusFound {
		t.Fatalf("capture: status %d body %q", w.Code, w.Body.String())
	}
	captured, _ := f.stored(t, subject)

	newer := captured
	newer.RefreshToken, newer.RenewedAt = "newer-refresh-token-from-another-replica", adoTestNow.Add(time.Minute)
	raw := p.a.cfg.Secrets
	hook := func() {
		killLockHolder(t, p.poolA, db.ADOSignInLockClass)
		if err := p.b.storeADOEntraBlob(context.Background(), subject, f.cfg.RowID, newer); err != nil {
			t.Error(err)
		}
	}
	p.a.cfg.Secrets = beforePutSecrets{Store: raw, hook: &hook}

	if _, err := p.a.RedeemADOEntraAccess(context.Background(), f.cfg, subject, f.cfg.Scopes); err != nil {
		t.Fatalf("redeem: %v", err)
	}
	if hook != nil {
		t.Fatal("the redemption stored nothing, so the test reached no write")
	}
	got, found, err := p.b.readADOEntraBlob(context.Background(), subject, f.cfg.RowID)
	if err != nil || !found {
		t.Fatalf("read: found=%v err=%v", found, err)
	}
	if got.RefreshToken != newer.RefreshToken {
		t.Errorf("the lost-lock redemption overwrote the newer sign-in: stored refresh token %q, want %q", got.RefreshToken, newer.RefreshToken)
	}
}

// A refresh the authority answers invalid_grant to, whose lock was lost while
// the call was in flight, does not delete the newer pair another replica stored:
// the delete is the same compare-and-set as the Put.
func TestPG_AWSRefresh_LostLockSpentAnswerKeepsANewerToken(t *testing.T) {
	old := db.LockWatchInterval
	db.LockWatchInterval = time.Hour // the watcher stays out of it: the compare-and-set is under test
	t.Cleanup(func() { db.LockWatchInterval = old })
	p := newReplicaPair(t)
	blob := putAWSSSOBlob(t, p.a, awsSSOTestFixedNow.Add(-time.Minute))

	newer := blob
	newer.AccessToken, newer.RefreshToken = "newer-access-token-1234567890", "newer-refresh-token-1234567890"
	newer.ExpiresAt = awsSSOTestFixedNow.Add(2 * time.Hour)
	fakeOIDC(t, func(w http.ResponseWriter, _ map[string]string, _ int) {
		killLockHolder(t, p.poolA, db.AWSSSOLockClass)
		storeSSOBlob(t, p.b, newer)
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "invalid_grant", "error_description": "refresh token is invalid"})
	})

	if _, msg := p.a.refreshAWSSSOBlob(context.Background(), awsSSOTestScope(), blob); msg != awsSSORefreshSpentSentence {
		t.Fatalf("refresh answered %q, want the spent sentence", msg)
	}
	b, found, err := p.b.readAWSSSOBlob(context.Background(), awsSSOTestScope())
	if err != nil || !found {
		t.Fatalf("the newer stored pair was deleted by the lost-lock holder: found=%v err=%v", found, err)
	}
	if b != newer {
		t.Errorf("the newer stored pair changed: %+v", b)
	}
}

// afterGetSecrets runs hook once, just after the first read of name returns:
// the instant between a redemption's read and its token call.
type afterGetSecrets struct {
	secretstore.Store
	name string
	hook *func()
}

func (a afterGetSecrets) For(owner string) secretstore.Store {
	return afterGetSecrets{Store: a.Store.For(owner), name: a.name, hook: a.hook}
}

func (a afterGetSecrets) Get(ctx context.Context, name string) ([]byte, error) {
	v, err := a.Store.Get(ctx, name)
	if h := *a.hook; h != nil && name == a.name && err == nil {
		*a.hook = nil
		h()
	}
	return v, err
}

func (a afterGetSecrets) Revision(ctx context.Context, name string) (string, error) {
	return a.Store.(secretstore.Revisioned).Revision(ctx, name)
}

// The same for the Azure DevOps sign-in: a redemption the authority refuses as
// dead, after its lock was lost, leaves the newer sign-in another replica stored.
func TestPG_ADOEntraRefresh_LostLockDeadAnswerKeepsANewerToken(t *testing.T) {
	old := db.LockWatchInterval
	db.LockWatchInterval = time.Hour // the watcher stays out of it: the compare-and-set is under test
	t.Cleanup(func() { db.LockWatchInterval = old })
	p := newReplicaPair(t)
	f := newADOFixture(t)
	subject := f.fake.Subject()
	f.srv = p.a
	p.a.cfg.ADOEntra, p.a.cfg.Now = func(context.Context) (ADOEntraConfig, bool, error) { return f.cfg, true, nil }, func() time.Time { return adoTestNow }
	if w := f.capture(t, subject); w.Code != http.StatusFound {
		t.Fatalf("capture: status %d body %q", w.Code, w.Body.String())
	}
	captured, _ := f.stored(t, subject)

	newer := captured
	newer.RefreshToken, newer.RenewedAt = "newer-refresh-token-from-another-replica", adoTestNow.Add(time.Minute)
	hook := func() {
		killLockHolder(t, p.poolA, db.ADOSignInLockClass)
		if err := p.b.storeADOEntraBlob(context.Background(), subject, f.cfg.RowID, newer); err != nil {
			t.Error(err)
		}
		f.fake.SetInvalidGrant(true)
	}
	p.a.cfg.Secrets = afterGetSecrets{Store: p.a.cfg.Secrets, name: adoCapture(f.cfg).secretName, hook: &hook}

	if _, err := p.a.RedeemADOEntraAccess(context.Background(), f.cfg, subject, f.cfg.Scopes); !errors.Is(err, ErrADOEntraDeadCredential) {
		t.Fatalf("redeem = %v, want the dead-credential refusal", err)
	}
	if hook != nil {
		t.Fatal("the redemption read nothing, so the test reached no refusal")
	}
	got, found, err := p.b.readADOEntraBlob(context.Background(), subject, f.cfg.RowID)
	if err != nil || !found {
		t.Fatalf("the newer sign-in was deleted by the lost-lock holder: found=%v err=%v", found, err)
	}
	if got.RefreshToken != newer.RefreshToken {
		t.Errorf("the newer sign-in changed: stored refresh token %q, want %q", got.RefreshToken, newer.RefreshToken)
	}
}
