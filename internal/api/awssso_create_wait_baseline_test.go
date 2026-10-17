// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
)

type createWaitRead struct {
	owner, name string
	revision    bool
}

type createWaitObservedSecrets struct {
	secretstore.Store
	owner   string
	observe func(context.Context, createWaitRead) error
}

func (s createWaitObservedSecrets) Get(ctx context.Context, name string) ([]byte, error) {
	if err := s.observe(ctx, createWaitRead{s.owner, name, false}); err != nil {
		return nil, err
	}
	return s.Store.Get(ctx, name)
}

func (s createWaitObservedSecrets) Revision(ctx context.Context, name string) (string, error) {
	if err := s.observe(ctx, createWaitRead{s.owner, name, true}); err != nil {
		return "", err
	}
	rev, guarded, err := secretstore.RevisionOf(ctx, s.Store, name)
	if err == nil && !guarded {
		err = secretstore.ErrNoRevision
	}
	return rev, err
}

func (s createWaitObservedSecrets) For(owner string) secretstore.Store {
	return createWaitObservedSecrets{s.Store.For(owner), owner, s.observe}
}

// afterMiss runs only after the real TryLock says another flight holds it, so
// the holder's completion is forced into the gap before the first poll.
type createWaitAfterMiss struct {
	db.Locker
	afterMiss     func()
	tries, blocks int
}

func (l *createWaitAfterMiss) Lock(ctx context.Context, key db.LockKey, budget time.Duration) (context.Context, func(), error) {
	l.blocks++
	return l.Locker.Lock(ctx, key, budget)
}

func (l *createWaitAfterMiss) TryLock(ctx context.Context, key db.LockKey) (context.Context, func(), bool, error) {
	l.tries++
	lctx, unlock, got, err := l.Locker.TryLock(ctx, key)
	if err == nil && !got {
		l.afterMiss()
	}
	return lctx, unlock, got, err
}

func doorBaselineRotation(t *testing.T, srv *Server, afterMiss func()) {
	t.Helper()
	shortCreateWait(t, time.Second)
	var reads []createWaitRead
	srv.cfg.Secrets = createWaitObservedSecrets{Store: srv.cfg.Secrets, observe: func(ctx context.Context, read createWaitRead) error {
		reads = append(reads, read)
		return ctx.Err()
	}}
	lock := &createWaitAfterMiss{Locker: srv.locker(), afterMiss: afterMiss}
	srv.locks.override = lock
	got, denial, err := srv.providerLiveness(withCreateRenewal(context.Background()), awsSSOTestProvider(), "claude-code", createRenewalOwner, true)
	if err != nil || denial.msg != "" || got.AccessToken != "fresh-access-token-abcdefghij" {
		t.Fatalf("door = %q, %q, %v; want the pair rotated after TryLock failed", got.AccessToken, denial.msg, err)
	}
	if lock.tries != 1 || lock.blocks != 0 {
		t.Errorf("locks = %d tried, %d blocking; want one try and no later acquisition", lock.tries, lock.blocks)
	}
	scope := createRenewalScope()
	want := []createWaitRead{
		{scope.owner, scope.ssoSecret(), true}, {scope.owner, scope.ssoSecret(), false},
		{scope.owner, scope.ssoSecret(), true}, {scope.owner, scope.ssoSecret(), false},
	}
	if !slices.Equal(reads, want) {
		t.Errorf("reads = %+v, want the owner's baseline before the door read, then one poll and one changed-pair read: %+v", reads, want)
	}
}

func TestCreateWait_DoorBaselineSeesRotationBeforeFirstPoll(t *testing.T) {
	srv := createWaitServer(t)
	doorBaselineRotation(t, srv, func() {
		next := createWaitBlob()
		next.AccessToken = "fresh-access-token-abcdefghij"
		next.RefreshToken = "rotated-refresh-token-abcdefghij"
		next.ExpiresAt = time.Now().Add(time.Hour)
		storeSSOBlobFor(t, srv, createRenewalOwner, next)
	})
}

func TestPG_CreateWait_DoorBaselineSeesRotationBeforeFirstPoll(t *testing.T) {
	srv, pair := createWaitFixture(t)
	rec := &memAudit{}
	srv.cfg.Secrets = secretstore.Audited(srv.cfg.Secrets, rec)
	calls, entered, release := heldOIDC(t, renewedPair)
	holder := startHolder(t, pair, entered)
	doorBaselineRotation(t, srv, func() {
		release()
		select {
		case <-holder:
		case <-time.After(10 * time.Second):
			t.Fatal("the holder did not finish after its token endpoint was released")
		}
	})
	if calls.Load() != 1 {
		t.Errorf("token exchanges = %d, want only the holder's renewal", calls.Load())
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	reads := 0
	for _, row := range rec.rows {
		if row.Action == "secret.read" && row.Target == createRenewalScope().ssoSecret() {
			reads++
		}
	}
	if reads != 2 {
		t.Errorf("secret.read rows = %d, want only the door and changed pair", reads)
	}
}

func TestCreateWait_MetadataFailureKeepsTheDoorToken(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failAt  int
		failure func(context.Context) error
	}{
		{"baseline error", 1, func(context.Context) error { return errors.New("metadata unavailable") }},
		{"baseline timeout", 1, func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }},
		{"no revisions", 1, func(context.Context) error { return secretstore.ErrNoRevision }},
		{"poll error", 2, func(context.Context) error { return errors.New("metadata unavailable") }},
		{"poll timeout", 2, func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := createWaitServer(t)
			revisions, values := 0, 0
			srv.cfg.Secrets = createWaitObservedSecrets{Store: srv.cfg.Secrets, observe: func(ctx context.Context, read createWaitRead) error {
				if !read.revision {
					values++
					return ctx.Err()
				}
				revisions++
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > awsSSOCreateWaitTick {
					t.Error("metadata read has no wait-tick deadline")
				}
				if revisions == tc.failAt {
					return tc.failure(ctx)
				}
				return nil
			}}
			start := time.Now()
			got, denial, err := srv.providerLiveness(withCreateRenewal(context.Background()), awsSSOTestProvider(), "claude-code", createRenewalOwner, true)
			if err != nil || denial.msg != "" || got.AccessToken != createWaitBlob().AccessToken {
				t.Errorf("door = %q, %q, %v; want the token in hand", got.AccessToken, denial.msg, err)
			}
			if revisions != tc.failAt || values != 1 {
				t.Errorf("reads = %d revisions, %d values; want %d revisions and only the door value", revisions, values, tc.failAt)
			}
			if took := time.Since(start); took > 2*awsSSOCreateWaitTick {
				t.Errorf("metadata failure took %v; want at most one tick", took)
			}
		})
	}
}

func TestCreateWait_UnchangedRowAfterLockReleaseWaitsForBudget(t *testing.T) {
	shortCreateWait(t, 3*awsSSOCreateWaitTick)
	srv := createRenewalFixture(t)
	srv.cfg.Secrets = revisionedSecrets{srv.cfg.Secrets}
	storeSSOBlobFor(t, srv, createRenewalOwner, createWaitBlob())
	_, unlock, err := srv.lockAWSSSOOwner(context.Background(), createRenewalOwner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(unlock)
	lock := &createWaitAfterMiss{Locker: srv.locker(), afterMiss: unlock}
	srv.locks.override = lock
	start := time.Now()
	got, denial, err := srv.providerLiveness(withCreateRenewal(context.Background()), awsSSOTestProvider(), "claude-code", createRenewalOwner, true)
	if err != nil || denial.msg != "" || got.AccessToken != createWaitBlob().AccessToken {
		t.Errorf("door = %q, %q, %v; want the token in hand", got.AccessToken, denial.msg, err)
	}
	if took := time.Since(start); took < awsSSOCreateWaitBudget || took > awsSSOCreateWaitBudget+4*awsSSOCreateWaitTick {
		t.Errorf("unchanged-row wait took %v, want the %v budget", took, awsSSOCreateWaitBudget)
	}
	if lock.tries != 1 || lock.blocks != 0 {
		t.Errorf("locks = %d tried, %d blocking; want one try and no lock-release polling", lock.tries, lock.blocks)
	}
}

func TestCreateWait_OnlyTheCreateDoorCapturesABaseline(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ctx     context.Context
		refresh bool
	}{
		{"read-only liveness", withCreateRenewal(context.Background()), false},
		{"dispatch", context.Background(), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := createWaitServer(t)
			srv.cfg.Secrets = createWaitObservedSecrets{Store: srv.cfg.Secrets, observe: func(_ context.Context, read createWaitRead) error {
				if read.revision {
					t.Error("liveness outside the create door read renewal metadata")
				}
				return nil
			}}
			got, denial, err := srv.providerLiveness(tc.ctx, awsSSOTestProvider(), "claude-code", createRenewalOwner, tc.refresh)
			if err != nil || denial.msg != "" || got.AccessToken != createWaitBlob().AccessToken {
				t.Errorf("door = %q, %q, %v; want the existing token", got.AccessToken, denial.msg, err)
			}
		})
	}
}
