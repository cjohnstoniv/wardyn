// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

// The attach lease's support arm (AcquireAttachLease, ReserveAttachLease) against Postgres: the owner takes a
// lease a helper holds, nobody else does, and once the owner holds it a second owner tab no longer matches.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_AttachLeaseSupportArm walks the design's value list row by row: each step names the row it checks.
func TestPG_AttachLeaseSupportArm(t *testing.T) {
	ctx := context.Background()
	pool := runsPGPoolIsolated(t)
	pg := store.NewPG(pool)
	run := persistRun(t, ctx, pool, newRun(types.RunRunning)).ID
	lease := func(holder uuid.UUID, principal string, support bool) store.AttachLease {
		return store.AttachLease{RunID: run, HolderID: holder, Replica: "r1", Principal: principal, Source: "web", Since: time.Now().UTC(), Support: support}
	}
	acquire := func(l store.AttachLease, from uuid.UUID) bool {
		t.Helper()
		ok, err := pg.AcquireAttachLease(ctx, l, from, time.Minute)
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		return ok
	}
	held := func() store.AttachLease {
		t.Helper()
		l, found, err := pg.GetAttachLease(ctx, run)
		if err != nil || !found {
			t.Fatalf("lease = %+v found=%v err=%v", l, found, err)
		}
		return l
	}
	supportCol := func() bool {
		t.Helper()
		var v bool
		if err := pool.QueryRow(ctx, `SELECT support FROM run_attach_leases WHERE run_id = $1`, run).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	release := func(holder uuid.UUID) {
		t.Helper()
		if ok, err := pg.ReleaseAttachLease(ctx, run, holder); err != nil || !ok {
			t.Fatalf("release %s = %v, %v", holder, ok, err)
		}
	}

	// Row "live owner (support false) | helper (support true)": refused.
	owner, helper, helper2, owner2 := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	if !acquire(lease(owner, "owner", false), uuid.Nil) {
		t.Fatal("none | any: a free lease was refused")
	}
	if acquire(lease(helper, "helper", true), uuid.Nil) {
		t.Error("live owner | helper: the helper took the owner's lease")
	}
	// Row "live owner | another owner tab": refused.
	if acquire(lease(owner2, "owner", false), uuid.Nil) {
		t.Error("live owner | another owner tab: refused row was taken")
	}
	// Row "same holder | same holder": renewed.
	if !acquire(lease(owner, "owner", false), uuid.Nil) {
		t.Error("same holder | same holder: not renewed")
	}
	release(owner)

	// Row "live helper (support true) | the same helper, another tab": refused.
	if !acquire(lease(helper, "helper", true), uuid.Nil) || !supportCol() {
		t.Fatal("none | helper: the helper could not take a free lease, or support is not recorded")
	}
	if acquire(lease(helper2, "helper", true), uuid.Nil) {
		t.Error("live helper | the same helper, another tab: refused row was taken")
	}
	// Row "live helper | owner": taken, epoch +1, support false.
	before := held()
	if !acquire(lease(owner, "owner", false), uuid.Nil) {
		t.Fatal("live helper | owner: refused, the owner must win the lease")
	}
	if after := held(); after.HolderID != owner || after.Epoch != before.Epoch+1 || supportCol() {
		t.Errorf("after the owner took it: %+v support %v, want the owner's, epoch %d, support false", after, supportCol(), before.Epoch+1)
	}
	if ok, _ := pg.HoldsAttachLease(ctx, run, helper); ok {
		t.Error("the helper still holds a lease the owner took")
	}
	// Row "helper's lease taken by the owner | another owner tab": refused.
	if acquire(lease(owner2, "owner", false), uuid.Nil) {
		t.Error("helper's lease taken by the owner | another owner tab: the second tab took the owner's lease")
	}
	release(owner)

	// Row "reserved for P | P": taken. Row "helper lease reserved for a taker, another acquirer": refused.
	if !acquire(lease(helper, "helper", true), uuid.Nil) {
		t.Fatal("none | helper: a free lease was refused")
	}
	prev, reservation, found, err := pg.ReserveAttachLease(ctx, run, uuid.Nil, "owner", "r1", time.Minute)
	if err != nil || !found || prev.HolderID != helper || reservation == uuid.Nil {
		t.Fatalf("reserve over the helper's lease = %+v %s found=%v err=%v", prev, reservation, found, err)
	}
	if supportCol() {
		t.Error("a reservation kept the replaced helper lease's support flag")
	}
	if acquire(lease(owner2, "someone-else", false), uuid.Nil) {
		t.Error("helper lease reserved for a taker | an owner tab that is not the taker: the reservation was taken")
	}
	if acquire(lease(helper2, "helper", true), uuid.Nil) {
		t.Error("helper lease reserved for a taker | a helper: the reservation was taken")
	}
	if !acquire(lease(owner, "owner", false), uuid.Nil) || held().HolderID != owner || supportCol() {
		t.Error("reserved for P | P: the taker could not take its reservation")
	}
	release(owner)

	// Row "expired | any": taken, whoever held it.
	if !acquire(lease(helper, "helper", true), uuid.Nil) {
		t.Fatal("none | helper: a free lease was refused")
	}
	if _, err := pool.Exec(ctx, `UPDATE run_attach_leases SET expires_at = now() - interval '1 second' WHERE run_id = $1`, run); err != nil {
		t.Fatal(err)
	}
	if !acquire(lease(owner2, "owner", false), uuid.Nil) || held().HolderID != owner2 || supportCol() {
		t.Error("expired helper lease | owner: not taken as a free one")
	}
}
