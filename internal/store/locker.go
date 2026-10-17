// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import "github.com/cjohnstoniv/wardyn/internal/db"

// Locker is the cross-replica locker over this store's database (db.LockerFor):
// every PG over one pool shares it. The api package takes its ordered locks
// through it when the store offers one, and falls back to an in-process locker
// for a store with no database (nil here, for a PG with no pool).
func (s PG) Locker() db.Locker {
	if s.Pool == nil {
		return nil
	}
	return db.LockerFor(s.Pool)
}
