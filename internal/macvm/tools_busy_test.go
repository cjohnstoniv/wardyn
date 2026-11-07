// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package macvm_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
)

const toolsUpdateBusyText = "A tools update of this macOS base is already running."

// A tools update takes the update lock with one try, before it claims a host ledger entry: when another
// update or a prepare holds it, it returns at once and never reaches Admit.
func TestUpdateToolsRefusesWhileAnotherHoldsTheUpdateLock(t *testing.T) {
	f, tf, _ := stage2Prep(t)
	if _, err := macvm.Prepare(context.Background(), f.opts); err != nil {
		t.Fatal(err)
	}
	unlock, err := macvm.LockUpdate(f.opts.Layout.BaseDir(0))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	o := updateOpts(f, tf, newGuest(), "0.10.0")
	o.Force = true
	admitted := false
	o.Admit = func(context.Context) (func(), error) { admitted = true; return func() {}, nil }
	// A short context so a lock that waits fails this test instead of hanging it.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, changed, err := macvm.UpdateTools(ctx, o)
	if !errors.Is(err, macvm.ErrToolsUpdateBusy) || err.Error() != toolsUpdateBusyText || changed || admitted {
		t.Fatalf("busy: changed %v admitted %v err %v", changed, admitted, err)
	}
	// Once the holder is done the same call goes ahead.
	unlock()
	if _, _, err := macvm.UpdateTools(context.Background(), o); err != nil {
		t.Fatalf("after the lock was released: %v", err)
	}
	if !admitted {
		t.Fatal("the update did not claim its entry once it held the lock")
	}
}

// The same for deriving a base from Standard.
func TestDeriveBaseRefusesWhileAnotherHoldsTheUpdateLock(t *testing.T) {
	f, tf, _ := stage2Prep(t)
	if _, err := macvm.Prepare(context.Background(), f.opts); err != nil {
		t.Fatal(err)
	}
	dir := f.opts.Layout.VariantDir("xcode-16")
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	unlock, err := macvm.LockUpdate(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	o := updateOpts(f, tf, newGuest(), "0.10.0")
	o.Force = true
	admitted := false
	o.Admit = func(context.Context) (func(), error) { admitted = true; return func() {}, nil }
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, changed, err := macvm.DeriveBase(ctx, o, "xcode-16")
	if !errors.Is(err, macvm.ErrToolsUpdateBusy) || err.Error() != toolsUpdateBusyText || changed || admitted {
		t.Fatalf("busy: changed %v admitted %v err %v", changed, admitted, err)
	}
}

// A caller whose own context ends while it waits is still told so, not that another update runs.
func TestUpdateToolsKeepsItsOwnCancellation(t *testing.T) {
	f, tf, _ := stage2Prep(t)
	if _, err := macvm.Prepare(context.Background(), f.opts); err != nil {
		t.Fatal(err)
	}
	unlock, err := macvm.LockUpdate(f.opts.Layout.BaseDir(0))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	o := updateOpts(f, tf, newGuest(), "0.10.1")
	o.Force = true
	if _, _, err := macvm.UpdateTools(ctx, o); !errors.Is(err, context.Canceled) || errors.Is(err, macvm.ErrToolsUpdateBusy) {
		t.Fatalf("a cancelled caller was told another update runs: %v", err)
	}
}
