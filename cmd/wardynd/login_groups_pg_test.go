// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/secretstore/keydomain"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey/subjectkeytest"
)

// A sign-in never waits out a held assignment lock: the stamp gives up after loginGroupsWait.
func TestStampLoginGroups_GivesUpOnAHeldAssignmentLock(t *testing.T) {
	pool := subjectkeytest.ThrowawayDB(t)
	ctx := t.Context()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // a no-op after Commit
	if err := keydomain.LockAssignments(ctx, tx); err != nil {
		t.Fatal(err)
	}
	old := loginGroupsWait
	loginGroupsWait = 200 * time.Millisecond
	defer func() { loginGroupsWait = old }()
	done := make(chan struct{})
	go func() { stampLoginGroups(ctx, pool, "gina", []string{"eng"}, false); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stampLoginGroups outlasted its wait while an assignment write held the lock")
	}
}
