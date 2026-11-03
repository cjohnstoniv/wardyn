// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package maskmanifest

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/audit/sinks"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
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

// Covered answering false because Postgres failed says so in the log (#2808), with the run and never a
// value; the answer itself is unchanged.
func TestCoveredLogsWhyItIsNotCovered(t *testing.T) {
	var logs strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	m, _ := newManifests(t, downPool(t))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	run := uuid.New()
	if m.Covered(ctx, run) {
		t.Fatal("Covered answered true with Postgres down")
	}
	got := logs.String()
	if !strings.Contains(got, "reading a run's manifest row failed") || !strings.Contains(got, run.String()) || strings.Contains(got, string(secretValue)) {
		t.Fatalf("log = %q, want the failed read named with the run id and no value", got)
	}
}

// A value another replica removed leaves the registry only after the manifests stop vouching for the run it
// belonged to, and a process-wide value only after they stop vouching for any run: Held is false by the time
// the masker is short of it, with no DropFenced in between.
func TestARegistryDropUnholdsBeforeTheValueGoes(t *testing.T) {
	m, reg := newManifests(t, nil)
	run, other := uuid.New(), uuid.New()
	m.note(run, 1, true, nil)
	m.note(other, 1, true, nil)
	reg.Add(run, secretValue)

	var heldWhenGone bool
	reg.SetDropGuard(func(id uuid.UUID) func() { // wraps the manifests' guard to see the state at the moment the drop begins
		heldWhenGone = m.Held(run)
		return m.beginDrop(id)
	})
	reg.DropRunValue(run, secretValue)
	if !heldWhenGone {
		t.Fatal("the guard did not run before the value was removed")
	}
	if m.Held(run) || !m.Held(other) || len(reg.Snapshot(run)) != 0 {
		t.Fatalf("after a run value drop: run held %v, other held %v, values %d", m.Held(run), m.Held(other), len(reg.Snapshot(run)))
	}
}

func TestADroppedProcessWideValueUnholdsEveryRunOnlyWhileItGoes(t *testing.T) {
	m, reg := newManifests(t, nil)
	a, b := uuid.New(), uuid.New()
	m.note(a, 1, true, nil)
	m.note(b, 1, true, nil)
	id := uuid.New()
	reg.ApplyGlobal(id, "alice", "sso", secretValue, time.Time{}, time.Time{})

	var heldDuring bool
	reg.SetDropGuard(func(rid uuid.UUID) func() { // the manifests' guard, observed while the removal is under way
		end := m.beginDrop(rid)
		heldDuring = m.Held(a) || m.Held(b)
		return end
	})
	reg.DropGlobalValue(id)
	if heldDuring {
		t.Error("a run was held while a process-wide value was being dropped")
	}
	if !m.Held(a) || !m.Held(b) {
		t.Fatalf("a dropped process-wide value left runs un-held after it: %v %v; every row would be labelled globals_only", m.Held(a), m.Held(b))
	}
}

// The sweeper's late eviction takes a held run's values out of the registry; it stops the manifests vouching
// for the run first, and the audit sinks stop adding the run's correlation id from then on.
func TestAnEvictionUnholdsTheRunBeforeItsValuesGoAndTheSinkStopsAddingTheID(t *testing.T) {
	const secret = "zq-throwaway-evict-13"
	m, reg := newManifests(t, nil)
	run := uuid.New()
	m.note(run, 1, true, nil)
	if err := reg.Add(run, []byte(secret)); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var got []types.AuditEvent
	fan := sinks.NewFanout(sinkFunc(func(ev types.AuditEvent) { mu.Lock(); got = append(got, ev); mu.Unlock() }))
	fan.EnrichRuns(func(context.Context, uuid.UUID) (string, bool, error) { return secret, true, nil },
		sinks.Witnessed(func(id uuid.UUID, b []byte) []byte { return reg.JSONVariantMasker(id).Mask(b) }, m.Held, reg.Generation), m.Held)
	emit := func() {
		ev := types.AuditEvent{ID: uuid.New(), RunID: &run, Action: "run.kill", Outcome: "success", Data: json.RawMessage(`{"k":1}`)}
		if err := fan.Emit(context.Background(), ev); err != nil {
			t.Fatal(err)
		}
	}

	emit() // held: the id goes out masked
	if !m.Held(run) {
		t.Fatal("the run is not held before the eviction")
	}
	reg.Evict(run)
	if m.Held(run) {
		t.Fatal("the manifests still vouch for the run after its values were evicted")
	}
	emit() // the id is cached on the sink; coverage is checked before the cache
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("%d events reached the sink, want 2", len(got))
	}
	if !strings.Contains(string(got[0].Data), sinks.CorrelationKey) || strings.Contains(string(got[0].Data), secret) {
		t.Errorf("held event: %s", got[0].Data)
	}
	if strings.Contains(string(got[1].Data), secret) || strings.Contains(string(got[1].Data), sinks.CorrelationKey) {
		t.Errorf("event after the eviction: %s", got[1].Data)
	}
}

type sinkFunc func(types.AuditEvent)

func (sinkFunc) Name() string { return "func" }
func (f sinkFunc) Emit(_ context.Context, ev types.AuditEvent) error {
	f(ev)
	return nil
}

// The cold sweep's Evict lands between the enricher's coverage check and its masking, and a reload holds the
// run again before the masking returns: Held is true at both ends and the masker read an empty corpus. The
// registry's generation is the witness that catches it.
func TestAnEvictionAndAReloadAroundTheMaskingLeaveTheIDOff(t *testing.T) {
	const secret = "zq-throwaway-sweep-16"
	m, reg := newManifests(t, nil)
	run := uuid.New()
	m.note(run, 1, true, nil)
	if err := reg.Add(run, []byte(secret)); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var got []types.AuditEvent
	inLookup, releaseLookup := make(chan struct{}), make(chan struct{})
	inMask, releaseMask := make(chan struct{}), make(chan struct{})
	fan := sinks.NewFanout(sinkFunc(func(ev types.AuditEvent) { mu.Lock(); got = append(got, ev); mu.Unlock() }))
	fan.EnrichRuns(func(context.Context, uuid.UUID) (string, bool, error) {
		close(inLookup)
		<-releaseLookup
		return secret, true, nil
	}, sinks.Witnessed(func(id uuid.UUID, b []byte) []byte {
		out := reg.JSONVariantMasker(id).Mask(b)
		close(inMask)
		<-releaseMask
		return out
	}, m.Held, reg.Generation), m.Held)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ev := types.AuditEvent{ID: uuid.New(), RunID: &run, Action: "run.kill", Outcome: "success", Data: json.RawMessage(`{"k":1}`)}
		_ = fan.Emit(context.Background(), ev)
	}()
	<-inLookup
	reg.Evict(run) // the sweeper
	close(releaseLookup)
	<-inMask
	reg.AddLocal(run, []byte(secret)) // the reload registers, then publishes
	m.note(run, 1, true, nil)
	if !m.Held(run) {
		t.Fatal("the reload did not hold the run again")
	}
	close(releaseMask)
	<-done
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || strings.Contains(string(got[0].Data), secret) || strings.Contains(string(got[0].Data), sinks.CorrelationKey) {
		t.Errorf("the sink got %v", got)
	}
}
