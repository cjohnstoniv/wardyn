// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package maskmanifest

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/secretmask"
)

// noKeys is a Keys that must never be asked: every path these tests drive
// refuses or finishes before a value is sealed.
type noKeys struct{ t *testing.T }

func (k noKeys) Current(context.Context, string, string) (int, []byte, error) {
	k.t.Helper()
	k.t.Error("the key service was asked")
	return 0, nil, errors.New("unexpected")
}

func (k noKeys) Key(context.Context, string, string, int) ([]byte, error) {
	k.t.Helper()
	k.t.Error("the key service was asked")
	return nil, errors.New("unexpected")
}

// downPool is a pool whose Postgres refuses every connection: pgxpool dials
// lazily, so it builds, and each query then fails.
func downPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://wardyn@127.0.0.1:1/wardyn?connect_timeout=1&sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func newManifests(t *testing.T, pool *pgxpool.Pool) (*Manifests, *secretmask.Registry) {
	t.Helper()
	reg := secretmask.NewRegistry()
	return New(pool, noKeys{t}, reg), reg
}

var secretValue = []byte("0123456789abcdef")

func TestAADBindsRunOrdinalAndKeyVersion(t *testing.T) {
	run := uuid.New()
	base := string(aad(run, 1, 1))
	for name, other := range map[string][]byte{
		"another run":     aad(uuid.New(), 1, 1),
		"another ordinal": aad(run, 2, 1),
		"another version": aad(run, 1, 2),
	} {
		if string(other) == base {
			t.Errorf("%s produced the same AAD", name)
		}
	}
	if !strings.Contains(base, aadLabel) {
		t.Errorf("AAD %q does not carry the purpose label", base)
	}
}

func TestRefusalsThatNeedNoPostgres(t *testing.T) {
	m, _ := newManifests(t, nil)
	ctx := context.Background()
	if err := m.Start(ctx, uuid.New(), ""); !errors.Is(err, ErrNoOwner) {
		t.Errorf("Start with no owner = %v, want ErrNoOwner", err)
	}
	if _, err := m.FenceSubject(ctx, ""); !errors.Is(err, ErrNoOwner) {
		t.Errorf("FenceSubject with no owner = %v, want ErrNoOwner", err)
	}
}

func TestAppendDropsShortAndAlreadyHeldValuesBeforeAskingPostgres(t *testing.T) {
	m, reg := newManifests(t, nil) // a nil pool panics on any query
	run := uuid.New()
	m.cache[run] = &entry{rev: 1, have: map[[sha256.Size]byte]struct{}{sha256.Sum256(secretValue): {}}}

	if err := m.Append(context.Background(), run, []byte("short"), secretValue); err != nil {
		t.Fatalf("Append of only short or held values = %v, want nil", err)
	}
	if got := reg.Snapshot(run); len(got) != 0 {
		t.Fatalf("a value already on record was registered again: %q", got)
	}
}

func TestNoteKeepsThisProcessCopyExactOrDropsIt(t *testing.T) {
	m, _ := newManifests(t, nil)
	run := uuid.New()

	m.note(run, 5, false, nil)
	if _, ok := m.cache[run]; ok {
		t.Fatal("a revision written before this process held a copy created an entry")
	}

	m.note(run, 1, false, [][]byte{secretValue})
	e := m.cache[run]
	if e == nil || e.rev != 1 || e.complete {
		t.Fatalf("first write = %+v, want rev 1, incomplete", e)
	}
	if _, ok := e.have[sha256.Sum256(secretValue)]; !ok {
		t.Fatal("the appended value's digest was not recorded")
	}
	if m.Held(run) {
		t.Fatal("an incomplete manifest reports Held")
	}

	m.note(run, 2, true, nil)
	if !m.Held(run) {
		t.Fatal("Complete did not make the manifest Held")
	}
	m.note(run, 3, false, nil)
	if !m.cache[run].complete {
		t.Fatal("a later write cleared the complete flag")
	}

	m.note(run, 9, false, nil) // another replica wrote in between
	if _, ok := m.cache[run]; ok || m.Held(run) {
		t.Fatal("a revision gap kept the copy; the next admission must reload")
	}
}

func TestForgetDropsTheCopyAndTheRegistryValuesButForgetCacheKeepsThem(t *testing.T) {
	m, reg := newManifests(t, nil)
	run := uuid.New()
	m.note(run, 1, true, [][]byte{secretValue})
	reg.Add(run, secretValue)

	m.ForgetCache(run)
	if m.Held(run) {
		t.Fatal("ForgetCache kept the copy")
	}
	if len(reg.Snapshot(run)) != 1 {
		t.Fatal("ForgetCache touched the registry's values")
	}

	m.note(run, 1, true, nil)
	m.Forget(run)
	if m.Held(run) || len(reg.Snapshot(run)) != 0 {
		t.Fatal("Forget left the copy or the registry's values")
	}
}

func TestGoneEvictsOnlyWhatThisProcessLoaded(t *testing.T) {
	m, reg := newManifests(t, nil)
	loaded, never := uuid.New(), uuid.New()
	m.note(loaded, 1, true, nil)
	reg.Add(loaded, secretValue)
	reg.Add(never, secretValue) // injection's value for a run that never had a manifest

	m.gone(loaded)
	m.gone(never)

	if len(reg.Snapshot(loaded)) != 0 {
		t.Error("a vanished manifest's values were left masking")
	}
	if len(reg.Snapshot(never)) != 1 {
		t.Error("a run that never had a manifest lost the values injection registered")
	}
}

func TestPostgresDownFailsClosed(t *testing.T) {
	m, reg := newManifests(t, downPool(t))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	run := uuid.New()

	if err := m.Start(ctx, run, "alice"); err == nil || !strings.Contains(err.Error(), "maskmanifest: start") {
		t.Errorf("Start = %v, want a start failure", err)
	}
	err := m.Append(ctx, run, secretValue)
	if err == nil || errors.Is(err, ErrNoManifest) || errors.Is(err, ErrFenced) {
		t.Errorf("Append = %v, want a plain read failure, not a manifest verdict", err)
	}
	if len(reg.Snapshot(run)) != 0 {
		t.Error("a value was registered although its rows were not committed")
	}
	if err := m.Complete(ctx, run); err == nil || !strings.Contains(err.Error(), "maskmanifest: complete") {
		t.Errorf("Complete = %v, want a complete failure", err)
	}
	if m.Covered(ctx, run) {
		t.Error("Covered answered true with Postgres down")
	}
	if runs, err := m.FenceSubject(ctx, "alice"); err == nil || runs != nil {
		t.Errorf("FenceSubject = %v, %v, want an error and no runs", runs, err)
	}
	if m.load(ctx, run, "alice") {
		t.Error("load succeeded with Postgres down")
	}
}

func TestWatchAnswersFromTheLastCheckWithinItsInterval(t *testing.T) {
	m, _ := newManifests(t, downPool(t))
	watch := m.Watch(uuid.New(), time.Hour)
	if watch() {
		t.Fatal("Watch answered true with Postgres down")
	}
	// Within the interval the answer is the cached one, not a second round trip:
	// with no pool at all, a re-query would panic.
	m.pool = nil
	if watch() {
		t.Fatal("Watch changed its answer inside the interval")
	}
}
