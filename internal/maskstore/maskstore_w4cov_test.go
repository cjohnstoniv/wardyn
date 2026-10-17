// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package maskstore

import (
	"bytes"
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey"
)

// w4CovKeys is an in-memory Keys: one 32-byte key per (owner, version), with an
// injectable error per owner, and a record of every Key lookup.
type w4CovKeys struct {
	mu       sync.Mutex
	keys     map[w4CovKeyID][]byte
	errs     map[string]error
	current  int
	lookups  []w4CovKeyID
	currents []string
	handed   [][]byte
}

type w4CovKeyID struct {
	owner   string
	version int
}

func w4CovNewKeys() *w4CovKeys {
	return &w4CovKeys{keys: map[w4CovKeyID][]byte{}, errs: map[string]error{}, current: 1}
}

func (k *w4CovKeys) put(owner string, version int, fill byte) []byte {
	key := bytes.Repeat([]byte{fill}, kek.DEKSize)
	k.keys[w4CovKeyID{owner, version}] = key
	return key
}

func (k *w4CovKeys) Current(_ context.Context, owner, purpose string) (int, []byte, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.currents = append(k.currents, owner)
	if purpose != subjectkey.PurposeCred {
		return 0, nil, errors.New("w4cov: unexpected purpose " + purpose)
	}
	if err := k.errs[owner]; err != nil {
		return 0, nil, err
	}
	key, ok := k.keys[w4CovKeyID{owner, k.current}]
	if !ok {
		return 0, nil, errors.New("w4cov: no current key")
	}
	return k.current, bytes.Clone(key), nil
}

func (k *w4CovKeys) Key(_ context.Context, owner, purpose string, version int) ([]byte, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if purpose != subjectkey.PurposeCred {
		return nil, errors.New("w4cov: unexpected purpose " + purpose)
	}
	k.lookups = append(k.lookups, w4CovKeyID{owner, version})
	if err := k.errs[owner]; err != nil {
		return nil, err
	}
	key, ok := k.keys[w4CovKeyID{owner, version}]
	if !ok {
		return nil, errors.New("w4cov: no such key")
	}
	out := bytes.Clone(key)
	k.handed = append(k.handed, out)
	return out, nil
}

func w4CovContains(set [][]byte, v string) bool {
	for _, s := range set {
		if string(s) == v {
			return true
		}
	}
	return false
}

func TestW4CovNewAttachesTheStoreAsTheRegistryBackend(t *testing.T) {
	reg := secretmask.NewRegistry()
	if reg.Persisted() {
		t.Fatal("a fresh registry reports a backend")
	}
	s := New(nil, w4CovNewKeys(), reg)
	if !reg.Persisted() {
		t.Fatal("New did not attach the store to the registry")
	}
	if s.sync.rows == nil {
		t.Fatal("New left the ref map nil: note would panic")
	}
}

func TestW4CovDigestOfIsKeyedAndSeparatesEveryField(t *testing.T) {
	key := bytes.Repeat([]byte{1}, 32)
	base := digestOf(key, bucketGlobal, "alice", "cred", 1, []byte("value-one"))
	if len(base) != 32 {
		t.Fatalf("digest is %d bytes, want 32", len(base))
	}
	if again := digestOf(key, bucketGlobal, "alice", "cred", 1, []byte("value-one")); !bytes.Equal(base, again) {
		t.Fatal("the digest is not deterministic: two replicas would write two rows")
	}
	for name, other := range map[string][]byte{
		"key":     digestOf(bytes.Repeat([]byte{2}, 32), bucketGlobal, "alice", "cred", 1, []byte("value-one")),
		"bucket":  digestOf(key, bucketRun, "alice", "cred", 1, []byte("value-one")),
		"owner":   digestOf(key, bucketGlobal, "bob", "cred", 1, []byte("value-one")),
		"scope":   digestOf(key, bucketGlobal, "alice", "other", 1, []byte("value-one")),
		"version": digestOf(key, bucketGlobal, "alice", "cred", 2, []byte("value-one")),
		"value":   digestOf(key, bucketGlobal, "alice", "cred", 1, []byte("value-two")),
	} {
		if bytes.Equal(base, other) {
			t.Errorf("changing the %s did not change the digest", name)
		}
	}
	if bytes.Contains(base, []byte("value-one")) {
		t.Error("the digest carries the value")
	}
}

func TestW4CovAADBindsTheRowIdentity(t *testing.T) {
	id := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	base := aad(bucketRun, id, "alice", "scope", 3)
	if !bytes.Equal(base, aad(bucketRun, id, "alice", "scope", 3)) {
		t.Fatal("aad is not deterministic")
	}
	others := map[string][]byte{
		"bucket":  aad(bucketGlobal, id, "alice", "scope", 3),
		"id":      aad(bucketRun, uuid.MustParse("22222222-2222-2222-2222-222222222222"), "alice", "scope", 3),
		"owner":   aad(bucketRun, id, "bob", "scope", 3),
		"scope":   aad(bucketRun, id, "alice", "elsewhere", 3),
		"version": aad(bucketRun, id, "alice", "scope", 4),
	}
	for name, o := range others {
		if bytes.Equal(base, o) {
			t.Errorf("changing the %s did not change the aad", name)
		}
	}
}

func TestW4CovSealOpensOnlyUnderItsOwnRow(t *testing.T) {
	key := bytes.Repeat([]byte{7}, kek.DEKSize)
	put := secretmask.GlobalPut{Value: []byte("a-credential-value")}
	row, err := seal(key, 2, bucketGlobal, "alice", "cred", put)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if row.id == uuid.Nil {
		t.Fatal("seal minted no row id")
	}
	if !bytes.Equal(row.digest, digestOf(key, bucketGlobal, "alice", "cred", 2, put.Value)) {
		t.Error("the sealed row's digest is not the value's digest")
	}
	if bytes.Contains(row.blob, put.Value) {
		t.Error("the sealed blob holds the plaintext")
	}
	got, err := kek.Open(key, row.blob, aad(bucketGlobal, row.id, "alice", "cred", 2))
	if err != nil || string(got) != string(put.Value) {
		t.Fatalf("the blob does not open under its own aad: %q, %v", got, err)
	}
	if _, err := kek.Open(key, row.blob, aad(bucketGlobal, row.id, "bob", "cred", 2)); err == nil {
		t.Error("the blob opened under another owner's aad")
	}

	if _, err := seal(key[:16], 2, bucketGlobal, "alice", "cred", put); err == nil ||
		!strings.Contains(err.Error(), "maskstore: seal a value") {
		t.Errorf("a short key sealed or was refused without context: %v", err)
	}
}

func TestW4CovExpiryHelpers(t *testing.T) {
	early := time.Now().UTC().Truncate(time.Second)
	late := early.Add(time.Hour)
	for _, c := range []struct {
		name string
		a, b *time.Time
		want *time.Time
	}{
		{"both nil", nil, nil, nil},
		{"a nil means no expiry", nil, &late, nil},
		{"b nil means no expiry", &early, nil, nil},
		{"a later", &late, &early, &late},
		{"b later", &early, &late, &late},
	} {
		got := laterExpiry(c.a, c.b)
		if (got == nil) != (c.want == nil) || (got != nil && !got.Equal(*c.want)) {
			t.Errorf("%s: laterExpiry = %v, want %v", c.name, got, c.want)
		}
	}
	same := early
	for _, c := range []struct {
		name string
		a, b *time.Time
		want bool
	}{
		{"both nil", nil, nil, true},
		{"one nil", &early, nil, false},
		{"other nil", nil, &early, false},
		{"equal instants", &early, &same, true},
		{"different instants", &early, &late, false},
	} {
		if got := sameTime(c.a, c.b); got != c.want {
			t.Errorf("%s: sameTime = %v, want %v", c.name, got, c.want)
		}
	}
	inOtherZone := early.In(time.FixedZone("x", 3600))
	if !sameTime(&early, &inOtherZone) {
		t.Error("the same instant in another zone is not the same time")
	}
}

func TestW4CovDueCondNumbersItsParameters(t *testing.T) {
	re := regexp.MustCompile(`\$\d+`)
	for _, c := range []struct{ bucket, cutoff int }{{1, 2}, {2, 3}} {
		got := re.FindAllString(dueCond(c.bucket, c.cutoff), -1)
		allowed := map[string]bool{"$" + string(rune('0'+c.bucket)): true, "$" + string(rune('0'+c.cutoff)): true}
		seen := map[string]bool{}
		for _, p := range got {
			if !allowed[p] {
				t.Errorf("dueCond(%d, %d) references %s, which is not one of its parameters", c.bucket, c.cutoff, p)
			}
			seen[p] = true
		}
		if len(seen) != 2 {
			t.Errorf("dueCond(%d, %d) references %v, want both parameters", c.bucket, c.cutoff, seen)
		}
	}
}

func TestW4CovWritersThatNeedNoPoolRefuseOrNoOp(t *testing.T) {
	keys := w4CovNewKeys()
	s := New(nil, keys, secretmask.NewRegistry())
	now := time.Now().UTC().Truncate(time.Second)
	puts := []secretmask.GlobalPut{{Value: []byte("a-credential-value")}}

	if err := s.PutGlobal(0, "", "cred", puts, false, now); err != nil {
		t.Errorf("the operator namespace is process-local: PutGlobal = %v", err)
	}
	if err := s.EvictGlobal("", "cred", now); err != nil {
		t.Errorf("the operator namespace is process-local: EvictGlobal = %v", err)
	}
	if err := s.PurgeRuns(context.Background(), nil); err != nil {
		t.Errorf("purging no runs is a no-op: %v", err)
	}
	n, err := s.EraseOwner(context.Background(), "")
	if err == nil || n != 0 || !strings.Contains(err.Error(), "operator namespace") {
		t.Errorf("EraseOwner(\"\") = %d, %v; want a refusal naming the operator namespace", n, err)
	}
	if len(keys.lookups) != 0 || len(keys.currents) != 0 {
		t.Errorf("a no-op writer asked for a key: Key %v, Current %v", keys.lookups, keys.currents)
	}
}

func TestW4CovPutGlobalFailsClosedOnItsKey(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	puts := []secretmask.GlobalPut{{Value: []byte("a-credential-value")}}

	t.Run("the owner's key is unavailable", func(t *testing.T) {
		keys := w4CovNewKeys()
		injected := errors.New("key service down")
		keys.errs["alice"] = injected
		err := New(nil, keys, secretmask.NewRegistry()).PutGlobal(0, "alice", "cred", puts, false, now)
		if !errors.Is(err, injected) {
			t.Fatalf("PutGlobal = %v, want the injected key error wrapped", err)
		}
		if !strings.Contains(err.Error(), "the owner's key") {
			t.Errorf("the key failure carries no context: %v", err)
		}
	})

	t.Run("the key cannot seal", func(t *testing.T) {
		keys := w4CovNewKeys()
		keys.keys[w4CovKeyID{"alice", 1}] = []byte("too-short")
		err := New(nil, keys, secretmask.NewRegistry()).PutGlobal(0, "alice", "cred", puts, false, now)
		if err == nil || !strings.Contains(err.Error(), "maskstore: seal a value") {
			t.Fatalf("PutGlobal with a bad key = %v, want the seal failure", err)
		}
	})
}
