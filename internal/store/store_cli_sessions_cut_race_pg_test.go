// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The token call's write after a sessions-only cut its late check did not see: the cut's sweep ran
// before the new row existed, so CreateCLISession has to read the cut against the approval's own
// sequence (the issue the session records). The session from before the cut is the control the sweep
// reaches; a personal API token written after the same cut keeps the sessions-only exemption.
// Guarded by WARDYN_TEST_PG: skipped cleanly when unset, must PASS when set.
func TestPG_CLISession_CreateAfterUnseenSessionsCut(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	const sub, email = "sub-cut", "cut@corp.example"
	old, _, oldRaw := seedCLISession(t, st, sub, email)
	approved, err := store.ReadRevocationSeq(ctx, pool) // the approval's issue (DecideCLISignIn's rev_seq)
	if err != nil {
		t.Fatal(err)
	}
	// CutSessions's write, committed after the token call's late check answered "not cut".
	if err := store.ExecRevocationCut(ctx, pool, `INSERT INTO oidc_session_cuts (sub, cut_at) VALUES ($1, clock_timestamp())
		ON CONFLICT (sub) DO UPDATE SET cut_at = EXCLUDED.cut_at`, sub, types.CLISessionEndSessionsCut); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetCLISession(ctx, old.ID); got.EndedAt == nil {
		t.Errorf("the session from before the cut = %+v, want ended", got)
	}
	if _, err := st.GetAPITokenByRaw(ctx, oldRaw); err == nil {
		t.Errorf("the access token of the session from before the cut still authenticates")
	}

	// The token call's write, guarded as createCLISession guards it (guardOwner with the approval's epoch).
	guarded := store.WithIdentityGuard(ctx, store.IdentityGuard{Principal: sub, Epoch: 0})
	now := time.Now().UTC()
	sess := store.CLISessionRow{ID: uuid.New(), Identity: store.CLIIdentity{Principal: sub, Email: email, Role: "user",
		UserType: types.UserTypeStandard, RevSeq: approved}, IdentityStampedAt: now, Machine: "laptop", Store: types.CLIStoreKeychain,
		PublicKey: cliTestKey(t), CreatedAt: now, ExpiresAt: now.Add(24 * time.Hour)}
	exp := now.Add(time.Hour)
	raw := "wdn_" + uuid.NewString()
	if _, err := st.CreateCLISession(guarded, sess, "wdc_"+uuid.NewString(), types.APIToken{ID: uuid.New(), Principal: sub, Email: email,
		Role: "user", UserType: types.UserTypeStandard, Name: "CLI: laptop", CreatedAt: now, ExpiresAt: &exp, RevSeq: approved,
		Kind: types.APITokenKindCLIAccess, CLISessionID: &sess.ID}, raw); err == nil {
		if got, _ := st.GetCLISession(ctx, sess.ID); got.EndedAt == nil {
			t.Errorf("CreateCLISession after an unseen sessions-only cut wrote a live session: %+v", got)
		}
		if _, err := st.GetAPITokenByRaw(ctx, raw); err == nil {
			t.Errorf("CreateCLISession after an unseen sessions-only cut wrote an access token that passes apiTokenLive")
		}
	}

	personal := "wdn_" + uuid.NewString()
	if _, err := st.CreateAPIToken(store.WithIdentityGuard(ctx, store.IdentityGuard{Principal: sub, Epoch: -1}), types.APIToken{ID: uuid.New(),
		Principal: sub, Email: email, Role: "user", UserType: types.UserTypeStandard, Name: "ci", CreatedAt: now, RevSeq: approved}, personal); err != nil {
		t.Errorf("a personal API token after a sessions-only cut = %v, want written (the exemption)", err)
	}
	if _, err := st.GetAPITokenByRaw(ctx, personal); err != nil {
		t.Errorf("the personal API token after a sessions-only cut = %v, want it to authenticate", err)
	}
}

// sessionsCutSQL is CutSessions's write ("Sign out everywhere"), for store.ExecRevocationCut.
const sessionsCutSQL = `INSERT INTO oidc_session_cuts (sub, cut_at) VALUES ($1, clock_timestamp())
	ON CONFLICT (sub) DO UPDATE SET cut_at = EXCLUDED.cut_at`

// stmtHold is a pool tracer that holds the first statement whose SQL contains match at its start, its
// transaction open: held is closed when the statement is reached, and it runs once free is called.
type stmtHold struct {
	match        string
	once, freed  sync.Once
	held, freeCh chan struct{}
}

func (h *stmtHold) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if strings.Contains(d.SQL, h.match) {
		h.once.Do(func() { close(h.held); <-h.freeCh })
	}
	return ctx
}

func (h *stmtHold) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (h *stmtHold) free() { h.freed.Do(func() { close(h.freeCh) }) }

// heldStore is a store on its own pool over pool's database whose first CLI session insert is held
// (after CreateCLISession's revocation-order lock and cut check), and the hold.
func heldStore(t *testing.T, pool *pgxpool.Pool) (store.PG, *stmtHold) {
	t.Helper()
	h := &stmtHold{match: "INSERT INTO cli_sessions", held: make(chan struct{}), freeCh: make(chan struct{})}
	cfg := pool.Config()
	cfg.ConnConfig.Tracer = h
	p, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	t.Cleanup(h.free) // runs first: a failed test never leaves the held statement waiting
	return store.NewPG(p), h
}

// waitHeld waits for h's statement to be reached.
func waitHeld(ctx context.Context, t *testing.T, h *stmtHold) {
	t.Helper()
	select {
	case <-h.held:
	case <-ctx.Done():
		t.Fatal("CreateCLISession never reached its insert")
	}
}

// waitLockWaiters waits until n backends of pool's database wait on a lock of wait_event event
// ("" = any), failing if a write in early finishes first (it did not wait on the held transaction).
func waitLockWaiters(ctx context.Context, t *testing.T, pool *pgxpool.Pool, event string, n int, early ...chan error) {
	t.Helper()
	for {
		for _, ch := range early {
			if len(ch) > 0 {
				t.Fatalf("a write finished while CreateCLISession held its transaction open: %v", <-ch)
			}
		}
		var got int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock' AND ($1 = '' OR wait_event = $1)`, event).Scan(&got); err != nil {
			t.Fatalf("waiting for %d lock waiters (%q): %v", n, event, err)
		}
		if got >= n {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("waiting for %d lock waiters (%q): %d after the deadline", n, event, got)
		case <-time.After(5 * time.Millisecond): // poll interval; the condition, not the delay, orders the test
		}
	}
}

// cliWrite is a token call's write for principal, recording the approval's issue revSeq.
func cliWrite(t *testing.T, principal, email string, revSeq int64) (store.CLISessionRow, types.APIToken, string) {
	t.Helper()
	now := time.Now().UTC()
	sess := store.CLISessionRow{ID: uuid.New(), Identity: store.CLIIdentity{Principal: principal, Email: email, Role: "user",
		UserType: types.UserTypeStandard, RevSeq: revSeq}, IdentityStampedAt: now, Machine: "laptop", Store: types.CLIStoreKeychain,
		PublicKey: cliTestKey(t), CreatedAt: now, ExpiresAt: now.Add(24 * time.Hour)}
	exp := now.Add(time.Hour)
	return sess, types.APIToken{ID: uuid.New(), Principal: principal, Email: email, Role: "user", UserType: types.UserTypeStandard,
		Name: "CLI: laptop", CreatedAt: now, ExpiresAt: &exp, RevSeq: revSeq, Kind: types.APITokenKindCLIAccess, CLISessionID: &sess.ID}, "wdn_" + uuid.NewString()
}

// wantEnded fails unless the session is ended and its access token no longer authenticates.
func wantEnded(t *testing.T, st store.PG, id uuid.UUID, raw, what string) {
	t.Helper()
	if got, err := st.GetCLISession(context.Background(), id); err != nil || got.EndedAt == nil {
		t.Errorf("%s: session = %+v, %v; want ended", what, got, err)
	}
	if _, err := st.GetAPITokenByRaw(context.Background(), raw); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("%s: access token = %v, want ErrNotFound", what, err)
	}
}

// A cut that starts while the token call's write holds the revocation-order lock waits on its exclusive
// lock until that write commits, and its sweep then ends the new session: the write is held open at its
// insert (after the lock and the cut check), the cut is seen waiting, and only then is the write released.
func TestPG_CLISession_CutDuringCreateWaitsAndEndsTheNewSession(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	held, hold := heldStore(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const sub, email = "sub-wait", "wait@corp.example"
	approved, err := store.ReadRevocationSeq(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	sess, tok, raw := cliWrite(t, sub, email, approved)
	created := make(chan error, 1)
	go func() {
		_, err := held.CreateCLISession(store.WithIdentityGuard(ctx, store.IdentityGuard{Principal: sub, Epoch: 0}), sess, "wdc_"+uuid.NewString(), tok, raw)
		created <- err
	}()
	waitHeld(ctx, t, hold)

	cut := make(chan error, 1)
	go func() { cut <- store.ExecRevocationCut(ctx, pool, sessionsCutSQL, sub, types.CLISessionEndSessionsCut) }()
	waitLockWaiters(ctx, t, pool, "advisory", 1, cut, created)
	hold.free()
	if err := <-created; err != nil {
		t.Fatalf("CreateCLISession past its cut check = %v, want written", err)
	}
	if err := <-cut; err != nil {
		t.Fatalf("the cut = %v", err)
	}
	wantEnded(t, st, sess.ID, raw, "the session written while the cut waited")
}

// A suspension of the same person, a sessions-only cut and the token call's write all finish under a
// bounded context: the write takes the identity rows FOR SHARE and then the shared revocation-order lock,
// the suspension its rows FOR UPDATE and then the exclusive lock, the cut only the exclusive lock. First
// with the write held open at its insert while the suspension and the cut queue behind it, then free-running.
func TestPG_CLISession_CreateCutAndSuspensionDoNotDeadlock(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	held, hold := heldStore(t, pool)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	suspend := func(p store.PrincipalIdentity, sub string) error {
		_, err := st.SuspendIdentity(ctx, store.SuspendPlan{IdentityID: p.ID, Principal: sub, Principals: []string{sub}, CutoffSubs: []string{sub}})
		return err
	}
	create := func(s store.PG, sub, email string) (uuid.UUID, string, error) {
		approved, err := store.ReadRevocationSeq(ctx, pool)
		if err != nil {
			return uuid.Nil, "", err
		}
		sess, tok, raw := cliWrite(t, sub, email, approved)
		_, err = s.CreateCLISession(store.WithIdentityGuard(ctx, store.IdentityGuard{Principal: sub, Epoch: 0}), sess, "wdc_"+uuid.NewString(), tok, raw)
		return sess.ID, raw, err
	}

	// Held: the write holds the rows and the shared lock; the suspension waits on a row, the cut on the lock.
	const sub, email = "sub-three", "three@corp.example"
	person := signedIn(t, st, sub, email, leaverOID)
	var id uuid.UUID
	var raw string
	created := make(chan error, 1)
	go func() {
		var err error
		id, raw, err = create(held, sub, email)
		created <- err
	}()
	waitHeld(ctx, t, hold)
	suspended := make(chan error, 1)
	go func() { suspended <- suspend(person, sub) }()
	waitLockWaiters(ctx, t, pool, "", 1, suspended, created)
	cut := make(chan error, 1)
	go func() { cut <- store.ExecRevocationCut(ctx, pool, sessionsCutSQL, sub, types.CLISessionEndSessionsCut) }()
	waitLockWaiters(ctx, t, pool, "advisory", 1, cut, created)
	hold.free()
	for name, ch := range map[string]chan error{"create": created, "suspension": suspended, "cut": cut} {
		if err := <-ch; err != nil {
			t.Fatalf("held round: %s = %v", name, err)
		}
	}
	wantEnded(t, st, id, raw, "held round")

	// Free-running: the three start together; the write is refused or written and then ended.
	for i := range 5 {
		sub, email := fmt.Sprintf("sub-free-%d", i), fmt.Sprintf("free-%d@corp.example", i)
		person := signedIn(t, st, sub, email, "")
		start := make(chan struct{})
		var wg sync.WaitGroup
		var cerr, serr, xerr error
		var id uuid.UUID
		var raw string
		wg.Add(3)
		go func() { defer wg.Done(); <-start; id, raw, cerr = create(st, sub, email) }()
		go func() { defer wg.Done(); <-start; serr = suspend(person, sub) }()
		go func() {
			defer wg.Done()
			<-start
			xerr = store.ExecRevocationCut(ctx, pool, sessionsCutSQL, sub, types.CLISessionEndSessionsCut)
		}()
		close(start)
		wg.Wait()
		if serr != nil || xerr != nil || (cerr != nil && !errors.Is(cerr, store.ErrIdentityDeactivated)) {
			t.Fatalf("free round %d: create = %v, suspension = %v, cut = %v", i, cerr, serr, xerr)
		}
		if cerr == nil {
			wantEnded(t, st, id, raw, fmt.Sprintf("free round %d", i))
		}
	}
}
