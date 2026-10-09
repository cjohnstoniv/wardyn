// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
)

func TestPG_MaskErasureAWSWaitKeepsTheDoorGeneration(t *testing.T) {
	l := newMaskLab(t)
	fail := false
	a, b := erasureReplica(l, &fail), l.replicaMasking(newLabPool(t, l))
	a.srv.locks.override = db.NewPGLocker(l.pool, 2)
	b.srv.locks.override = db.NewPGLocker(l.pool, 2)
	b.srv.cfg.Now = time.Now
	l.personRun(createRenewalOwner, "wait erasure")
	storeSSOBlobFor(t, b.srv, createRenewalOwner, createWaitBlob())
	calls := renewedOIDC(t)
	_, release, err := a.srv.lockAWSSSOOwner(t.Context(), createRenewalOwner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	next := createWaitBlob()
	next.AccessToken = "fresh-pair-after-mask-erase"
	next.RefreshToken = "fresh-refresh-after-mask-erase"
	next.ExpiresAt = time.Now().Add(time.Hour)
	lock := &createWaitAfterMiss{Locker: b.srv.locker(), afterMiss: func() {
		// The door already holds its credential; the first poll has not run.
		maskEraseRequest(t, l, a, createRenewalOwner)
		storeSSOBlobFor(t, a.srv, createRenewalOwner, next)
	}}
	b.srv.locks.override = lock
	got, denial, err := b.srv.providerLiveness(withCreateRenewal(t.Context()), awsSSOTestProvider(), "claude-code", createRenewalOwner, true)
	if err != nil || denial.msg != "" || got.AccessToken != next.AccessToken {
		t.Fatalf("changed-pair wait = %q, %q, %v", got.AccessToken, denial.msg, err)
	}
	if lock.tries != 1 || lock.blocks != 0 {
		t.Fatalf("wait used %d tries and %d blocking acquisitions", lock.tries, lock.blocks)
	}
	if calls.Load() != 0 {
		t.Fatalf("waiting request exchanged %d tokens", calls.Load())
	}
	if err := b.srv.maskSSOBlob(got, createRenewalScope()); !errors.Is(err, secretmask.ErrErased) {
		t.Fatalf("the wait replaced its door snapshot: %v", err)
	}
	maskAssertErased(t, l, createRenewalOwner, a, b)
	fresh, found, err := b.srv.readAWSSSOBlob(t.Context(), createRenewalScope())
	if err != nil || !found {
		t.Fatalf("fresh credential read = %v, %v", found, err)
	}
	if err := b.srv.maskSSOBlob(fresh, createRenewalScope()); err != nil {
		t.Fatalf("new operation could not register: %v", err)
	}
}
