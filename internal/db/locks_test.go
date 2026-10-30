// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"errors"
	"testing"
	"time"
)

// testKey is a key of class whose object is unique to the test.
func testKey(t *testing.T, class int32) LockKey { return NewLockKey(class, t.Name()) }

// requireOrderEnforced is the test over every documented nesting: for each pair
// of classes in LockOrder, taking them in order succeeds and taking them the
// other way round is refused with ErrLockOrder, nothing taken. A class added to
// LockOrder is covered the moment it is added; a class taken without being
// listed is refused.
func requireOrderEnforced(t *testing.T, l Locker) {
	t.Helper()
	ctx := context.Background()
	for i, first := range LockOrder {
		for j, second := range LockOrder {
			if i == j {
				continue
			}
			a, b := testKey(t, first), testKey(t, second)
			actx, releaseA, err := l.Lock(ctx, a, time.Second)
			if err != nil {
				t.Fatalf("lock %v: %v", a, err)
			}
			bctx, releaseB, err := l.Lock(actx, b, time.Second)
			switch {
			case i < j && err != nil:
				t.Errorf("nesting %#x then %#x is the documented order and was refused: %v", uint32(first), uint32(second), err)
			case i > j && !errors.Is(err, ErrLockOrder):
				t.Errorf("nesting %#x then %#x inverts the order and was not refused with ErrLockOrder: %v", uint32(first), uint32(second), err)
			}
			if err == nil {
				if got := len(HeldLocks(bctx)); got != 2 {
					t.Errorf("a nested hold carries %d locks, want 2", got)
				}
				releaseB()
			}
			releaseA()
		}
	}
	if _, _, err := l.Lock(ctx, LockKey{Class: 0x54455354, Obj: 1}, time.Second); !errors.Is(err, ErrLockOrder) {
		t.Errorf("a class outside LockOrder was taken: %v", err)
	}
}

// requireNestingSemantics: the same key again is re-entrant, a sibling class of
// the same rank cannot be nested, and a nested release leaves the outer held.
func requireNestingSemantics(t *testing.T, l Locker) {
	t.Helper()
	ctx := context.Background()
	k := testKey(t, AWSSSOLockClass)
	octx, releaseOuter, err := l.Lock(ctx, k, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseOuter()
	if _, again, err := l.Lock(octx, k, time.Second); err != nil {
		t.Errorf("the same key again was refused: %v", err)
	} else {
		again()
	}
	other := NewLockKey(AWSSSOLockClass, t.Name(), "another")
	if _, _, err := l.Lock(octx, other, time.Second); !errors.Is(err, ErrLockOrder) {
		t.Errorf("a second lock of the same class nested: %v", err)
	}
	inner := testKey(t, SiteConfigLockClass)
	ictx, releaseInner, err := l.Lock(octx, inner, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	releaseInner()
	if got := len(HeldLocks(ictx)); got != 2 {
		t.Errorf("held = %d, want 2 (a context's holds are immutable)", got)
	}
	// The outer lock is still held: a context that does not carry it must not get it.
	if _, release, ok, err := l.TryLock(ctx, k); err != nil || ok {
		if ok {
			release()
		}
		t.Errorf("TryLock of a held key = ok %v, err %v; want not ok", ok, err)
	}
}

// requireCredentialKeysAscend: several AWS SSO credential locks nest only in
// ascending Obj, so two whole-person erases cannot each hold what the other
// waits for; another class's second key still refuses.
func requireCredentialKeysAscend(t *testing.T, l Locker) {
	t.Helper()
	ctx := context.Background()
	lo, hi := NewLockKey(AWSSSOCredentialLockClass, t.Name(), "a"), NewLockKey(AWSSSOCredentialLockClass, t.Name(), "b")
	if lo.Obj > hi.Obj {
		lo, hi = hi, lo
	}
	octx, releaseLo, err := l.Lock(ctx, lo, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ictx, releaseHi, err := l.Lock(octx, hi, time.Second)
	if err != nil {
		t.Fatalf("a second credential lock in ascending order was refused: %v", err)
	}
	if got := len(HeldLocks(ictx)); got != 2 {
		t.Errorf("held = %d, want 2", got)
	}
	releaseHi()
	releaseLo()
	hctx, releaseHi, err := l.Lock(ctx, hi, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseHi()
	if _, _, err := l.Lock(hctx, lo, time.Second); !errors.Is(err, ErrLockOrder) {
		t.Errorf("a credential lock in descending order nested: %v", err)
	}
}

// requireReleaseEndsTheContext: the context a hold returns is the guarded
// work's and ends with the hold, whichever locker made it, so a caller that
// keeps using it after unlocking fails in a test with no database too.
func requireReleaseEndsTheContext(t *testing.T, l Locker) {
	t.Helper()
	hctx, release, err := l.Lock(context.Background(), testKey(t, SiteConfigLockClass), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if hctx.Err() != nil {
		t.Fatalf("a held lock's context has ended: %v", context.Cause(hctx))
	}
	release()
	if hctx.Err() == nil {
		t.Error("the context outlived its lock's release")
	}
}

func TestLocalLocker_ReleaseEndsTheContext(t *testing.T) {
	requireReleaseEndsTheContext(t, NewLocalLocker())
}

func TestLocalLocker_Order(t *testing.T)   { requireOrderEnforced(t, NewLocalLocker()) }
func TestLocalLocker_Nesting(t *testing.T) { requireNestingSemantics(t, NewLocalLocker()) }
func TestLocalLocker_CredentialKeysAscend(t *testing.T) {
	requireCredentialKeysAscend(t, NewLocalLocker())
}
func TestLocalLocker_Serializes(t *testing.T) {
	requireSerializes(t, NewLocalLocker(), NewLocalLocker())
}

// requireSerializes: a second locker (another replica) cannot take a held key,
// waits its budget and is then refused, never let through; once released it can.
// On the local locker the two arguments are one object, as one process is.
func requireSerializes(t *testing.T, a, b Locker) {
	t.Helper()
	ctx := context.Background()
	if _, ok := a.(*LocalLocker); ok {
		b = a
	}
	k := testKey(t, ADOSignInLockClass)
	_, release, err := a.Lock(ctx, k, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := b.TryLock(ctx, k); err != nil || ok {
		t.Fatalf("TryLock of a held key = ok %v, err %v", ok, err)
	}
	start := time.Now()
	if _, _, err := b.Lock(ctx, k, 200*time.Millisecond); !errors.Is(err, ErrLockBusy) {
		t.Fatalf("Lock of a held key = %v, want ErrLockBusy", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("a refused wait took %v, want about its 200ms budget", d)
	}
	release()
	_, release2, err := b.Lock(ctx, k, time.Second)
	if err != nil {
		t.Fatalf("Lock after release: %v", err)
	}
	release2()
}

// The order is pinned to the one documented in docs/design/0.8/0.8.6-ha.md §3
// and OPERATIONS.md: run operation, Azure DevOps run-token mint/revoke, AWS SSO (person, then credential),
// Azure DevOps sign-in, the organisation MCP server key, document locks (site config before capability
// enforcement), the audit sweep last. Reordering the table inverts every
// nesting it names, and the nestings the daemon really makes (the credential
// erase takes AWS SSO then the sign-in; a capture takes a credential lock then
// the site-config lock) run in the api suites under this order's enforcement.
func TestLockOrderIsTheDocumentedOne(t *testing.T) {
	want := []int32{RunOpLockClass, ADORunTokenLockClass, AWSSSOLockClass, AWSSSOCredentialLockClass, ADOSignInLockClass,
		ComponentKeyLockClass, SiteConfigLockClass, CapEnforcementLockClass, AuditSweepLockClass}
	if len(LockOrder) != len(want) {
		t.Fatalf("LockOrder has %d classes, want %d", len(LockOrder), len(want))
	}
	for i := range want {
		if LockOrder[i] != want[i] {
			t.Errorf("LockOrder[%d] = %#x, want %#x", i, uint32(LockOrder[i]), uint32(want[i]))
		}
	}
	seen := map[int32]bool{}
	for _, c := range []int32{LoginSupersedeLockClass, SecretRowLockClass, PushPathListLockClass, RunOutputLockClass, RecordingLockClass,
		MemberDriveLockClass} {
		seen[c] = true
	}
	for _, c := range LockOrder {
		if seen[c] {
			t.Errorf("class %#x collides with another two-argument lock class", uint32(c))
		}
		seen[c] = true
	}
}

func TestLockRefused(t *testing.T) {
	for _, err := range []error{ErrLockBusy, ErrLockNoCapacity, ErrLockUnavailable} {
		if !LockRefused(err) {
			t.Errorf("%v is not a refusal", err)
		}
	}
	if LockRefused(ErrLockOrder) || LockRefused(nil) {
		t.Error("an order violation, or nil, is not a refusal")
	}
}

// A context derived to outlive its caller keeps one cancellation: the loss of
// the Postgres lock the caller holds, even after the caller's own context
// ended first and so can no longer show the loss (#3480). A local hold, which
// cannot be lost, and no hold at all keep none.
func TestWithoutCancelUnlessLost_EndsOnlyOnTheLockLoss(t *testing.T) {
	lc := &lockConn{}
	lc.lost, lc.lose = context.WithCancelCause(context.Background())
	parent, cancelParent := context.WithCancel(context.Background())
	held := withHolds(parent, &holds{keys: []LockKey{testKey(t, RunOpLockClass)}, conn: lc})
	fenced, release := WithoutCancelUnlessLost(held)
	defer release()
	cancelParent()
	if err := fenced.Err(); err != nil {
		t.Fatalf("the caller's cancel reached the fenced context: %v", err)
	}
	if got := HeldLocks(fenced); len(got) != 1 {
		t.Fatalf("the fenced context lost the caller's holds: %v", got)
	}
	lc.lose(ErrLockLost)
	select {
	case <-fenced.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the lock's loss did not end the fenced context")
	}
	if cause := context.Cause(fenced); !errors.Is(cause, ErrLockLost) {
		t.Errorf("cause = %v, want ErrLockLost", cause)
	}

	for name, ctx := range map[string]context.Context{
		"local hold": withHolds(context.Background(), &holds{keys: []LockKey{testKey(t, RunOpLockClass)}}),
		"no hold":    context.Background(),
	} {
		c, cancel := context.WithCancel(ctx)
		out, rel := WithoutCancelUnlessLost(c)
		cancel()
		if err := out.Err(); err != nil {
			t.Errorf("%s: the caller's cancel reached the derived context: %v", name, err)
		}
		rel()
		if out.Err() == nil {
			t.Errorf("%s: release did not end the derived context", name)
		}
	}
}

// A fence derived after the lock is already lost is ended when it is handed
// back, not a moment later: no write may start on a lost lock.
func TestWithoutCancelUnlessLost_AlreadyLostIsEndedOnReturn(t *testing.T) {
	lc := &lockConn{}
	lc.lost, lc.lose = context.WithCancelCause(context.Background())
	lc.lose(ErrLockLost)
	held := withHolds(context.Background(), &holds{keys: []LockKey{testKey(t, RunOpLockClass)}, conn: lc})
	fenced, release := WithoutCancelUnlessLost(held)
	defer release()
	if fenced.Err() == nil {
		t.Fatal("a fence over an already-lost lock was live when returned")
	}
	if cause := context.Cause(fenced); !errors.Is(cause, ErrLockLost) {
		t.Errorf("cause = %v, want ErrLockLost", cause)
	}
}
