// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"testing"
	"time"
)

// #2137 on the cross-replica (Postgres advisory) locks: a credential's lock held
// on one replica stops a renewal of that credential on another and not one of
// the person's other credential; the whole-person erase stops both.
// Needs WARDYN_TEST_PG.
func TestPG_AWSSSOLock_CredentialLocksAcrossReplicas(t *testing.T) {
	p := newReplicaPair(t)
	ctx := context.Background()
	a, b := awsSSOTestScope(), otherCredentialOf(awsSSOTestOwner)
	if err := p.a.storeAWSSSOBlob(ctx, b, putAWSSSOBlob(t, p.a, awsSSOTestFixedNow.Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	free := func(scope awsSSOScope) bool {
		t.Helper()
		_, release, ok, err := p.b.tryLockAWSSSOCredential(ctx, scope)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			release()
		}
		return ok
	}

	_, unlock, err := p.a.lockAWSSSOCredential(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if free(a) {
		t.Error("a renewal of the same credential took its lock on another replica")
	}
	if !free(b) {
		t.Error("a renewal of the person's other credential waited on another replica")
	}
	unlock()

	_, unlock, err = p.a.lockAWSSSOEveryCredential(ctx, awsSSOTestOwner)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if free(a) || free(b) {
		t.Error("a renewal took a credential's lock on another replica during the whole-person erase")
	}
}
