// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package secretmask

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// fakeBackend records what the Registry commits and fails on demand.
type fakeBackend struct {
	failPut bool
	runs    int
	globals int
	evicts  int
	stale   bool
}

var errBackendDown = errors.New("backend down")

func (f *fakeBackend) PutRun(uuid.UUID, []byte) error {
	if f.failPut {
		return errBackendDown
	}
	f.runs++
	return nil
}

func (f *fakeBackend) PutGlobal(string, string, []GlobalPut, bool, time.Time) error {
	if f.failPut {
		return errBackendDown
	}
	f.globals++
	return nil
}

func (f *fakeBackend) EvictGlobal(string, string, time.Time) error {
	if f.failPut {
		return errBackendDown
	}
	f.evicts++
	return nil
}

func (f *fakeBackend) SweepGlobals(context.Context, time.Time) (int, error) { return 0, nil }
func (f *fakeBackend) PersistedRuns(context.Context) ([]uuid.UUID, error)   { return nil, nil }
func (f *fakeBackend) PurgeRuns(context.Context, []uuid.UUID) error         { return nil }
func (f *fakeBackend) EraseOwner(context.Context, string) (int, error)      { return 0, nil }
func (f *fakeBackend) Fresh(context.Context, time.Time) error {
	if f.stale {
		return errBackendDown
	}
	return nil
}

func masksValue(r *Registry, run uuid.UUID, v string) bool {
	return bytes.Contains(r.Masker(run).Mask([]byte("x "+v+" y")), placeholder)
}

// A mutator commits before it returns and caches only what committed: a failed
// commit leaves the value unknown, so the retry persists it instead of skipping
// it as already known.
func TestBackend_CommitsBeforeCachingAndAFailureLeavesNothingBehind(t *testing.T) {
	b := &fakeBackend{}
	r := NewRegistry()
	r.SetBackend(b)
	run := uuid.New()

	b.failPut = true
	if err := r.Add(run, []byte("a-value-that-cannot-commit")); !errors.Is(err, errBackendDown) {
		t.Fatalf("Add = %v, want the backend's error", err)
	}
	if err := r.AddGlobal("alice", "cred", time.Now(), []byte("a-global-that-cannot-commit")); !errors.Is(err, errBackendDown) {
		t.Fatalf("AddGlobal = %v, want the backend's error", err)
	}
	if err := r.EvictGlobal("alice", "cred", time.Now()); !errors.Is(err, errBackendDown) {
		t.Fatalf("EvictGlobal = %v, want the backend's error", err)
	}
	if masksValue(r, run, "a-value-that-cannot-commit") || masksValue(r, run, "a-global-that-cannot-commit") {
		t.Fatal("a value whose commit failed is in the cache")
	}

	b.failPut = false
	if err := r.Add(run, []byte("a-value-that-cannot-commit")); err != nil {
		t.Fatal(err)
	}
	if err := r.Add(run, []byte("a-value-that-cannot-commit")); err != nil {
		t.Fatal(err)
	}
	if b.runs != 1 {
		t.Errorf("the backend committed %d times, want 1 (a known value is not written again)", b.runs)
	}
	if err := r.AddGlobal("alice", "cred", time.Now(), []byte("a-global-that-cannot-commit"), []byte("short")); err != nil {
		t.Fatal(err)
	}
	if b.globals != 1 || !masksValue(r, run, "a-global-that-cannot-commit") {
		t.Errorf("the global was committed %d times and masked=%v", b.globals, masksValue(r, run, "a-global-that-cannot-commit"))
	}
	// Nothing to commit (every value too short) is not a commit.
	if err := r.AddGlobal("alice", "cred", time.Now(), []byte("short")); err != nil || b.globals != 1 {
		t.Errorf("a value under MinLen reached the backend: %d commits, err %v", b.globals, err)
	}
}

// Fresh is the consumer's proof of currency: true with no backend, and the
// backend's answer with one.
func TestBackend_FreshIsTheBackendsAnswer(t *testing.T) {
	var nilReg *Registry
	if !nilReg.Fresh(time.Now()) || !NewRegistry().Fresh(time.Now()) {
		t.Fatal("a registry with no backend is not its own corpus")
	}
	b := &fakeBackend{}
	r := NewRegistry()
	r.SetBackend(b)
	if !r.Fresh(time.Now()) {
		t.Fatal("a healthy backend is not fresh")
	}
	b.stale = true
	if r.Fresh(time.Now()) {
		t.Fatal("a backend that cannot prove currency is fresh")
	}
}

// ApplyGlobal and the drops put what another replica committed into the cache
// without touching the backend, and a current value retired by a tombstone stays
// masked until it is swept.
func TestBackend_ApplyingAnotherReplicasCommit(t *testing.T) {
	b := &fakeBackend{}
	r := NewRegistry()
	r.SetBackend(b)
	run := uuid.New()
	v := []byte("a-value-another-replica-committed")
	r.ApplyGlobal("alice", "cred", v, time.Time{}, time.Time{})
	r.AddLocal(run, []byte("a-run-value-another-replica-committed"))
	if !masksValue(r, run, string(v)) || !masksValue(r, run, "a-run-value-another-replica-committed") {
		t.Fatal("applied values are not masked")
	}
	if b.runs+b.globals != 0 {
		t.Fatal("applying a commit wrote it back to the backend")
	}
	r.RetireGlobalValue("alice", "cred", v, time.Now())
	if !masksValue(r, run, string(v)) {
		t.Error("a retired value stopped being masked before its sweep")
	}
	if n := r.SweepGlobals(time.Now().Add(time.Second)); n != 1 || masksValue(r, run, string(v)) {
		t.Errorf("the sweep dropped %d values and masked=%v", n, masksValue(r, run, string(v)))
	}
	r.DropRunValue(run, []byte("a-run-value-another-replica-committed"))
	if masksValue(r, run, "a-run-value-another-replica-committed") {
		t.Error("a dropped run value is still masked")
	}
	r.ApplyGlobal("alice", "cred", v, time.Time{}, time.Time{})
	r.DropGlobalValue("alice", "cred", v)
	if masksValue(r, run, string(v)) {
		t.Error("a dropped credential value is still masked")
	}
}
