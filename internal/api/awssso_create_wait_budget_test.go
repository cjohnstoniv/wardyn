// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The bounds of create's wait for a renewal in flight (awaitAWSSSORenewalInFlight):
// the budget, a read that does not answer, a client that goes away, a store
// without row revisions, and the documented gap a persist past the budget leaves.

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
)

// shortCreateWait shortens the wait's budget for one test.
func shortCreateWait(t *testing.T, budget time.Duration) {
	t.Helper()
	old := awsSSOCreateWaitBudget
	awsSSOCreateWaitBudget = budget
	t.Cleanup(func() { awsSSOCreateWaitBudget = old })
}

// A flight still renewing when the budget is spent: the run is created on the
// token in hand, as before the wait, with no unavailable sentence.
func TestPG_CreateWait_HeldRenewalOutlivingTheBudgetLaunches(t *testing.T) {
	shortCreateWait(t, time.Second)
	srv, p := createWaitFixture(t)
	_, entered, release := heldOIDC(t, renewedPair)
	holder := startHolder(t, p, entered)

	start := time.Now()
	res := postCreate(t, srv)
	waited := time.Since(start)
	release()
	<-holder

	if res.code != http.StatusCreated {
		t.Errorf("create = %d %s, want 201 on the token in hand", res.code, res.body)
	}
	if waited < time.Second || waited > 5*time.Second {
		t.Errorf("create took %v, want about the 1s budget", waited)
	}
	if n := runRowCount(srv); n != 1 {
		t.Errorf("run rows = %d, want 1", n)
	}
}

// The documented gap: a holder whose persist fails only after the budget marks
// the pair spent after the run exists. Rare (it needs a failed persist), and
// pinned here so a change to it is deliberate.
func TestPG_CreateWait_PersistFailingPastTheBudgetLeavesTheRun(t *testing.T) {
	shortCreateWait(t, time.Second)
	srv, p := createWaitFixture(t)
	p.b.cfg.Secrets = putScript{Store: p.b.cfg.Secrets, only: createRenewalScope().ssoSecret(), n: new(atomic.Int32),
		hook: func(int) error {
			time.Sleep(700 * time.Millisecond)
			return errors.New("secret store is wedged")
		}}
	_, entered, release := heldOIDC(t, renewedPair)
	holder := startHolder(t, p, entered)

	res := make(chan *httpResult, 1)
	go func() { res <- postCreate(t, srv) }()
	time.Sleep(100 * time.Millisecond)
	release()
	got := <-res
	<-holder

	if got.code != http.StatusCreated || runRowCount(srv) != 1 {
		t.Fatalf("create = %d %s with %d rows, want the run created before the holder's persist failed", got.code, got.body, runRowCount(srv))
	}
	spent, err := p.a.awsSSOTokenSpentNow(context.Background(), awsSSOTokenFingerprint(createWaitBlob().RefreshToken))
	if err != nil || !spent {
		t.Errorf("the in-hand pair spent = %v (%v), want it marked spent once the holder's persist failed", spent, err)
	}
}

// blockingRevisions is a store whose revision read never answers until its
// context ends.
type blockingRevisions struct{ secretstore.Store }

func (b blockingRevisions) Revision(ctx context.Context, _ string) (string, error) {
	<-ctx.Done()
	return "", ctx.Err()
}

func (b blockingRevisions) For(owner string) secretstore.Store {
	return blockingRevisions{b.Store.For(owner)}
}

// heldElsewhere holds the owner's renewal lock in this process, as another
// flight would, until the test ends.
func heldElsewhere(t *testing.T, srv *Server) {
	t.Helper()
	_, unlock, err := srv.lockAWSSSOOwner(context.Background(), createRenewalScope().owner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(unlock)
}

// createWaitServer is a create server holding a session create would serve,
// with its renewal lock held by another flight.
func createWaitServer(t *testing.T) *Server {
	t.Helper()
	srv := createRenewalFixture(t)
	storeSSOBlobFor(t, srv, createRenewalOwner, createWaitBlob())
	heldElsewhere(t, srv)
	return srv
}

// createTimeRenewal is create's renewal of the stored session, read first as
// the door reads it, and how long it took.
func createTimeRenewal(ctx context.Context, srv *Server) (awsSSOBlob, string, time.Duration) {
	ctx = secretstore.WithPurpose(withCreateRenewal(ctx), secretstore.PurposeSSORefresh)
	held, _, _ := srv.readAWSSSOBlob(ctx, createRenewalScope())
	start := time.Now()
	got, failure := srv.refreshAWSSSOBlob(ctx, createRenewalScope(), held)
	return got, failure, time.Since(start)
}

// revisionedSecrets is a store that keeps row revisions, as the Postgres store
// does: the kind of store the wait watches.
type revisionedSecrets struct{ secretstore.Store }

func (r revisionedSecrets) Revision(ctx context.Context, name string) (string, error) {
	v, err := r.Get(ctx, name)
	if errors.Is(err, secretstore.ErrNotFound) {
		return "", nil
	}
	return string(v), err
}

func (r revisionedSecrets) For(owner string) secretstore.Store {
	return revisionedSecrets{r.Store.For(owner)}
}

// A lock that could not be tried at all (the lock pool is full) is not a
// renewal in flight: create launches on the token in hand at once, with no wait.
func TestCreateWait_LockThatCouldNotBeTriedLaunchesAtOnce(t *testing.T) {
	srv := createRenewalFixture(t)
	srv.cfg.Secrets = revisionedSecrets{srv.cfg.Secrets}
	storeSSOBlobFor(t, srv, createRenewalOwner, createWaitBlob())
	calls := renewedOIDC(t)
	bearer := providerAdminToken(srv, createRenewalOwner)
	srv.locks.override = exhaustedPool()

	start := time.Now()
	w := do(t, srv, http.MethodPost, "/api/v1/runs", bearer, createRenewalBody)
	took := time.Since(start)

	if w.Code != http.StatusCreated || runRowCount(srv) != 1 {
		t.Errorf("create = %d %s with %d rows, want the run created on the token in hand", w.Code, w.Body.String(), runRowCount(srv))
	}
	if took > 500*time.Millisecond {
		t.Errorf("create took %v, want no wait: a lock that could not be tried says nothing about a renewal in flight", took)
	}
	if got, _ := createRenewalStored(t, srv); calls.Load() != 0 || got.AccessToken != createWaitBlob().AccessToken {
		t.Errorf("CreateToken calls = %d, stored token %q, want nothing renewed without the lock", calls.Load(), got.AccessToken)
	}
}

// A metadata read that does not answer ends the wait within one tick, on the
// token in hand.
func TestCreateWait_BlockedMetadataReadEndsWithinATick(t *testing.T) {
	srv := createWaitServer(t)
	srv.cfg.Secrets = blockingRevisions{srv.cfg.Secrets}
	got, failure, took := createTimeRenewal(context.Background(), srv)
	if failure != "" || got.AccessToken != createWaitBlob().AccessToken {
		t.Errorf("renewal = %q, %q, want the token in hand", got.AccessToken, failure)
	}
	if took > 2*awsSSOCreateWaitTick {
		t.Errorf("the wait took %v behind a read that never answered, want at most one tick (%v)", took, awsSSOCreateWaitTick)
	}
}

// A client that goes away ends the wait within one tick.
func TestCreateWait_CancelledClientEndsWithinATick(t *testing.T) {
	srv := createWaitServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	const after = 300 * time.Millisecond
	time.AfterFunc(after, cancel)
	_, failure, took := createTimeRenewal(ctx, srv)
	if failure != "" {
		t.Errorf("failure = %q, want none", failure)
	}
	if took > after+awsSSOCreateWaitTick+100*time.Millisecond {
		t.Errorf("the wait took %v after a cancel at %v, want it ended within one tick", took, after)
	}
}

// On a store without row revisions, a new pair is found by its expiry.
func TestCreateWait_UnguardedStoreServesTheNewPair(t *testing.T) {
	srv := createWaitServer(t)
	newer := createWaitBlob()
	newer.AccessToken, newer.RefreshToken, newer.ExpiresAt = "newer-access-token-1234567890", "newer-refresh-token-1234567890", time.Now().Add(time.Hour)
	time.AfterFunc(300*time.Millisecond, func() { storeSSOBlobFor(t, srv, createRenewalOwner, newer) })
	got, failure, took := createTimeRenewal(context.Background(), srv)
	if failure != "" || got.AccessToken != newer.AccessToken {
		t.Errorf("renewal = %q, %q, want the newer pair", got.AccessToken, failure)
	}
	if took > 3*time.Second {
		t.Errorf("found the newer pair after %v, want within a second or two", took)
	}
}

// A row removed meanwhile, or a pair marked spent in this process: the spent
// sentence, so create makes no row.
func TestCreateWait_RowGoneOrSpentRefuses(t *testing.T) {
	for name, act := range map[string]func(srv *Server){
		"row gone": func(srv *Server) {
			_ = srv.cfg.Secrets.For(createRenewalOwner).Delete(context.Background(), createRenewalScope().ssoSecret())
		},
		"marked spent": func(srv *Server) {
			srv.markAWSSSOTokenSpent(context.Background(), awsSSOTokenFingerprint(createWaitBlob().RefreshToken), createRenewalOwner)
		},
	} {
		t.Run(name, func(t *testing.T) {
			srv := createWaitServer(t)
			time.AfterFunc(300*time.Millisecond, func() { act(srv) })
			if _, failure, _ := createTimeRenewal(context.Background(), srv); failure != awsSSORefreshSpentSentence {
				t.Errorf("failure = %q, want the spent sentence", failure)
			}
		})
	}
}

// The budget follows the token exchange it waits out.
func TestCreateWait_BudgetFollowsTheTokenExchange(t *testing.T) {
	if want := 2*awsSSORefreshTimeout + awsSSORefreshRetryDelay + time.Second; awsSSOCreateWaitBudget != want {
		t.Errorf("budget = %v, want %v", awsSSOCreateWaitBudget, want)
	}
}
