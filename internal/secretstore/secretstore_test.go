// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package secretstore

import (
	"context"
	"errors"
	"sort"
	"testing"
)

// secretstore declares the at-rest Store contract (Name/Put/Get/Delete/List).
// There is no concrete logic in this package, so these tests assert the
// CONTRACT every provider must honor using a hand-rolled in-memory Store (no
// real Postgres / OpenBao). They exist so a future provider can be exercised
// against the same expectations:
//
//   - Put then Get returns the stored plaintext verbatim;
//   - Get of an unknown name reports an error (not an empty success);
//   - Put overwrites an existing name (last write wins);
//   - Delete removes a name (subsequent Get fails);
//   - List enumerates exactly the live names.

// ---- fake (in-memory, no real backend) -----------------------------------

var errNotFound = errors.New("secret not found")

type memStore struct {
	name string
	data map[string][]byte
}

func newMemStore() *memStore {
	return &memStore{name: "mem", data: map[string][]byte{}}
}

func (s *memStore) Name() string { return s.name }

func (s *memStore) Put(_ context.Context, name string, value []byte) error {
	// Copy to model an encrypt-at-rest backend that does not alias the caller's
	// buffer.
	cp := make([]byte, len(value))
	copy(cp, value)
	s.data[name] = cp
	return nil
}

func (s *memStore) Get(_ context.Context, name string) ([]byte, error) {
	v, ok := s.data[name]
	if !ok {
		return nil, errNotFound
	}
	cp := make([]byte, len(v))
	copy(cp, v)
	return cp, nil
}

func (s *memStore) Delete(_ context.Context, name string) error {
	delete(s.data, name)
	return nil
}

func (s *memStore) List(_ context.Context) ([]string, error) {
	out := make([]string, 0, len(s.data))
	for k := range s.data {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

// Compile-time assertion that the fake satisfies the contract under test.
var _ Store = (*memStore)(nil)

func TestStorePutGetRoundTrip(t *testing.T) {
	s := newMemStore()
	ctx := context.Background()
	want := []byte("hunter2")

	if err := s.Put(ctx, "gh_token", want); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := s.Get(ctx, "gh_token")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("Get = %q, want %q", got, want)
	}
}

// TestStoreGetMissingErrors verifies a missing secret is an explicit error, not
// a silent empty value the caller might inject as a credential.
func TestStoreGetMissingErrors(t *testing.T) {
	s := newMemStore()
	got, err := s.Get(context.Background(), "absent")
	if err == nil {
		t.Fatalf("Get(absent) returned no error; got value %q", got)
	}
	if got != nil {
		t.Errorf("Get(absent) value = %q, want nil", got)
	}
}

// TestStorePutOverwrites verifies last-write-wins semantics on the same name
// (key rotation overwrites the old value).
func TestStorePutOverwrites(t *testing.T) {
	s := newMemStore()
	ctx := context.Background()

	if err := s.Put(ctx, "k", []byte("old")); err != nil {
		t.Fatalf("Put old: %v", err)
	}
	if err := s.Put(ctx, "k", []byte("new")); err != nil {
		t.Fatalf("Put new: %v", err)
	}
	got, err := s.Get(ctx, "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != "new" {
		t.Errorf("Get after overwrite = %q, want %q", got, "new")
	}
}

// TestStoreDeleteRemoves verifies Delete makes a name unresolvable afterward.
func TestStoreDeleteRemoves(t *testing.T) {
	s := newMemStore()
	ctx := context.Background()

	if err := s.Put(ctx, "k", []byte("v")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.Delete(ctx, "k"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(ctx, "k"); err == nil {
		t.Errorf("Get after Delete succeeded; want error")
	}
}

// TestStoreListEnumeratesLiveNames verifies List returns exactly the names that
// are currently stored, and reflects deletions.
func TestStoreListEnumeratesLiveNames(t *testing.T) {
	s := newMemStore()
	ctx := context.Background()

	for _, n := range []string{"a", "b", "c"} {
		if err := s.Put(ctx, n, []byte("v")); err != nil {
			t.Fatalf("Put %s: %v", n, err)
		}
	}
	if err := s.Delete(ctx, "b"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	got, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []string{"a", "c"}
	if len(got) != len(want) {
		t.Fatalf("List = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("List[%d] = %q, want %q (got %v)", i, got[i], want[i], got)
		}
	}
}

// TestStorePutDoesNotAliasCaller verifies a provider must not retain the
// caller's backing array: mutating the caller's slice after Put must not change
// what Get returns. (Real providers encrypt-at-rest; an aliasing bug would let a
// later mutation silently corrupt a stored secret.)
func TestStorePutDoesNotAliasCaller(t *testing.T) {
	s := newMemStore()
	ctx := context.Background()

	val := []byte("secret")
	if err := s.Put(ctx, "k", val); err != nil {
		t.Fatalf("Put: %v", err)
	}
	val[0] = 'X' // mutate caller's buffer after handing it off

	got, err := s.Get(ctx, "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != "secret" {
		t.Errorf("Get = %q, want %q (Put aliased caller buffer)", got, "secret")
	}
}
