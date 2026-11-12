// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"

	"github.com/cjohnstoniv/wardyn/internal/db"
)

// lockLost reports a ctx that carries a Postgres hold whose lock the database no longer holds on that
// hold's connection (db.LockHeld). A ctx with no hold is never lost. It does not read the ctx's cause: a
// hold whose parent ended first never shows a later loss there. A variable so a test can stand in for the
// lost-lock answer, which only Postgres gives.
var lockLost = func(ctx context.Context) bool { return db.HeldLocks(ctx) != nil && db.LockHeld(ctx) != nil }

// teardownUnlessLost runs teardown on ref unless ctx's lock is lost, when it returns db.ErrLockLost
// without calling it.
func teardownUnlessLost(ctx context.Context, ref string, teardown func(context.Context, string) error) error {
	if lockLost(ctx) {
		return db.ErrLockLost
	}
	return teardown(ctx, ref)
}
