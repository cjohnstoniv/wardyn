// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package secretmask

import (
	"bytes"
	"testing"
	"time"

	"github.com/google/uuid"
)

// CS-4 (F5): process-wide mask copies of a person's credential used to live for
// the daemon's whole life, superseded tokens included. They are keyed by the
// credential's row now: a refresh retires the values it replaced, a delete
// retires them all, and a retired value is still masked until SweepGlobals
// drops it — masking fails open, so letting go is never immediate.
func TestAddGlobal_ReplacedAndDeletedValuesAreRetiredThenSwept(t *testing.T) {
	r := NewRegistry()
	const owner, name = "alice", "wardyn-harness-aws-oauth"
	oldAccess, newAccess, refresh := "access-token-before-refresh", "access-token-after-refresh", "the-refresh-token-kept"
	bobs := "bobs-token-under-the-same-name"
	r.AddGlobal("bob", name, []byte(bobs))

	r.AddGlobal(owner, name, []byte(oldAccess), []byte(refresh))
	r.AddGlobal(owner, name, []byte(newAccess), []byte(refresh))
	masked := func(v string) bool {
		return !bytes.Contains(r.Masker(uuid.New()).Mask([]byte("x "+v+" y")), []byte(v))
	}
	for _, v := range []string{oldAccess, newAccess, refresh, bobs} {
		if !masked(v) {
			t.Fatalf("%q is not masked right after the refresh; a replaced value must stay masked until it is swept", v)
		}
	}

	if n := r.SweepGlobals(time.Now().Add(-time.Hour)); n != 0 {
		t.Fatalf("a sweep with a cutoff before the refresh dropped %d values, want 0", n)
	}
	if n := r.SweepGlobals(time.Now().Add(time.Second)); n != 1 {
		t.Fatalf("the sweep after the grace dropped %d values, want 1 (the replaced access token)", n)
	}
	if masked(oldAccess) {
		t.Error("the replaced access token is still held after the sweep: superseded tokens never leave memory")
	}
	for _, v := range []string{newAccess, refresh, bobs} {
		if !masked(v) {
			t.Errorf("%q, a current value, stopped being masked after the sweep", v)
		}
	}

	r.EvictGlobal(owner, name)
	if !masked(newAccess) || !masked(refresh) {
		t.Fatal("a deleted credential stopped being masked at once; it must stay masked until swept")
	}
	if n := r.SweepGlobals(time.Now().Add(time.Second)); n != 2 {
		t.Fatalf("the sweep after the delete dropped %d values, want 2", n)
	}
	if masked(newAccess) || masked(refresh) {
		t.Error("a deleted credential's values are still held after the sweep")
	}
	if !masked(bobs) {
		t.Error("deleting alice's credential let go of bob's under the same name")
	}
}

// A value that comes back under its key is current again, and a sweep must not
// drop it.
func TestAddGlobal_AValueThatComesBackIsNotSwept(t *testing.T) {
	r := NewRegistry()
	r.AddGlobal("", "wardyn-harness-anthropic-oauth", []byte("setup-token-one"))
	r.AddGlobal("", "wardyn-harness-anthropic-oauth", []byte("setup-token-two"))
	r.AddGlobal("", "wardyn-harness-anthropic-oauth", []byte("setup-token-one"))
	if n := r.SweepGlobals(time.Now().Add(time.Second)); n != 1 {
		t.Errorf("the sweep dropped %d values, want 1 (only setup-token-two is retired)", n)
	}
	if got := r.Masker(uuid.Nil).Mask([]byte("setup-token-one")); bytes.Contains(got, []byte("setup-token-one")) {
		t.Error("a re-captured value was swept as retired")
	}
}

// A dispatch that read the credential before a concurrent refresh registers
// the stale pair after the refresh did. MergeGlobal must not retire the
// refresh's values; the next refresh (AddGlobal) retires the stale ones.
func TestMergeGlobal_AStaleReadDoesNotRetireTheRefreshedValues(t *testing.T) {
	r := NewRegistry()
	const owner, name = "alice", "wardyn-harness-aws-oauth"
	oldAccess, oldRefresh := "access-token-before-refresh", "refresh-token-before-refresh"
	newAccess, newRefresh := "access-token-after-refresh", "refresh-token-after-refresh"
	r.AddGlobal(owner, name, []byte(oldAccess), []byte(oldRefresh))
	r.AddGlobal(owner, name, []byte(newAccess), []byte(newRefresh)) // the refresh
	r.MergeGlobal(owner, name, []byte(oldAccess), []byte(oldRefresh))

	if n := r.SweepGlobals(time.Now().Add(time.Second)); n != 0 {
		t.Fatalf("the sweep dropped %d values, want 0: every value is in use by some run", n)
	}
	masked := func(v string) bool { return !bytes.Contains(r.Masker(uuid.Nil).Mask([]byte(v)), []byte(v)) }
	for _, v := range []string{oldAccess, oldRefresh, newAccess, newRefresh} {
		if !masked(v) {
			t.Errorf("%q is not masked after the stale registration", v)
		}
	}

	r.AddGlobal(owner, name, []byte("access-token-third"), []byte(newRefresh))
	if n := r.SweepGlobals(time.Now().Add(time.Second)); n != 3 {
		t.Errorf("the sweep after the next refresh dropped %d values, want 3", n)
	}
}

// An AddGlobal whose every value is empty or below MinLen used to retire the
// credential's whole current set — an Entra answer with no refresh_token was
// enough to evict one still in use. It is a no-op now; EvictGlobal is the one
// way to let go of every value.
func TestAddGlobal_NoUsableValueRetiresNothing(t *testing.T) {
	r := NewRegistry()
	const held = "the-refresh-token-still-in-use"
	r.AddGlobal("alice", "ado", []byte(held))
	r.AddGlobal("alice", "ado", []byte(""), []byte("short"))
	if n := r.SweepGlobals(time.Now().Add(time.Second)); n != 0 {
		t.Fatalf("an AddGlobal with no usable value retired %d values, want 0", n)
	}
	if got := r.Masker(uuid.Nil).Mask([]byte(held)); bytes.Contains(got, []byte(held)) {
		t.Error("the held value was swept after an AddGlobal that carried nothing")
	}
}
