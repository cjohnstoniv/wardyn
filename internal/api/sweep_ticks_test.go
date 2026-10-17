// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/sweephealth"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The sweeps that live in this package return an error, so a tick that did not
// finish cleanly is an attempt without a success.

// failListStore fails the one read SweepRunSecrets makes.
type failListStore struct{ store.Store }

func (failListStore) ListRuns(context.Context) ([]types.AgentRun, error) {
	return nil, errors.New("store down")
}

func TestSweepRunSecrets_ReturnsTheRunListingError(t *testing.T) {
	h := newHarness(t)
	reg := secretmask.NewRegistry()
	reg.Add(uuid.New(), []byte("held-secret"))
	cfg := baseTestConfig(h, failListStore{})
	cfg.MaskRegistry = reg
	srv := New(cfg)

	if n, err := srv.SweepRunSecrets(context.Background()); err == nil || n != 0 {
		t.Fatalf("SweepRunSecrets = %d, %v, want 0 and the listing error", n, err)
	}
}

func TestSweepExpiredCredentials_ReturnsTheStoreError(t *testing.T) {
	h := newHarness(t)
	boom := errors.New("pg secretstore: expired select: connection reset")
	h.srv.cfg.Secrets = &sweepSecrets{memSecrets: &memSecrets{m: map[string][]byte{}}, err: boom}
	if _, err := h.srv.SweepExpiredCredentials(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("SweepExpiredCredentials error = %v, want the store's", err)
	}

	// A store that cannot sweep at all is a deployment fact the sweep announces
	// once at Error; it is not a failed tick.
	h.srv.cfg.Secrets = &sweepSecrets{memSecrets: &memSecrets{m: map[string][]byte{}}, err: secretstore.ErrNoExpirySweep}
	if n, err := h.srv.SweepExpiredCredentials(context.Background()); err != nil || n != 0 {
		t.Fatalf("a store with no expiry sweep: %d, %v, want 0 and no error", n, err)
	}
}

// leaseFailStore fails the run watcher's claim, the first sub-pass of the tick.
type leaseFailStore struct{ store.Store }

func (leaseFailStore) ClaimStaleRunWatchers(context.Context, string, time.Duration) ([]types.AgentRun, error) {
	return nil, errors.New("claim failed")
}
func (leaseFailStore) HeartbeatRunWatcher(context.Context, uuid.UUID, string) error { return nil }
func (leaseFailStore) RunWatcherFresh(context.Context, uuid.UUID, time.Duration) (bool, error) {
	return false, nil
}

// The run watcher tick returns the first sub-pass error and still runs the
// rest, and its health record keeps the success where it was.
func TestRunWatcherTick_ReturnsTheFirstSubPassError(t *testing.T) {
	ticks := sweephealth.NewMemStore()
	tracker := sweephealth.New(ticks, "r1", nil)
	srv := &Server{cfg: Config{Store: leaseFailStore{}, Runner: &fakeRunner{}, SweepHealth: tracker, Now: time.Now}}
	recent := time.Now() // inside the undispatched pass's slow cadence: skip it

	err := tracker.Tick(context.Background(), sweephealth.RunWatcher, func(ctx context.Context) error {
		return srv.runWatcherTick(ctx, &recent)
	})
	if err == nil || err.Error() != "claim failed" {
		t.Fatalf("runWatcherTick = %v, want the claim error", err)
	}
	got, _ := ticks.Ticks(context.Background())
	if tk := got[sweephealth.RunWatcher]; tk.AttemptedAt.IsZero() || !tk.SucceededAt.IsZero() {
		t.Fatalf("health record %+v, want an attempt and no success", tk)
	}
}

// failingSweepBuilder is a sweepableImageBuilder whose sweep errors.
type failingSweepBuilder struct{ sweepableImageBuilder }

func (*failingSweepBuilder) SweepOrphanedBuilds(context.Context) error {
	return errors.New("docker unreachable")
}

// The orphaned build sweeper records every tick, and an erroring one is an
// attempt without a success.
func TestOrphanedBuildSweeper_RecordsAnErroringTickAsAnAttempt(t *testing.T) {
	ticks := sweephealth.NewMemStore()
	srv := &Server{cfg: Config{ImageBuilder: &failingSweepBuilder{}, SweepHealth: sweephealth.New(ticks, "r1", nil)}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); srv.orphanedBuildSweeper(ctx, 5*time.Millisecond) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		got, _ := ticks.Ticks(context.Background())
		if !got[sweephealth.OrphanedBuild].AttemptedAt.IsZero() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the sweeper never recorded an attempt")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	got, _ := ticks.Ticks(context.Background())
	if tk := got[sweephealth.OrphanedBuild]; !tk.SucceededAt.IsZero() {
		t.Fatalf("an erroring sweep recorded a success at %s", tk.SucceededAt)
	}
}
