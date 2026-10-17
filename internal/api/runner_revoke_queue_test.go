// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// revokeQueueStore is a queue store whose owner list is fixed and whose GetRun fails for the runs in failRead.
type revokeQueueStore struct {
	store.Store
	queueStore
	entries  []store.RunQueueEntry
	failRead map[uuid.UUID]bool
	mu       sync.Mutex
	read     []uuid.UUID
}

func (s *revokeQueueStore) QueueOwnerEntries(context.Context, string) ([]store.RunQueueEntry, error) {
	return s.entries, nil
}
func (s *revokeQueueStore) QueueEntriesOfRevokedRunners(context.Context, int) ([]store.RunQueueEntry, error) {
	return s.entries, nil
}
func (s *revokeQueueStore) QueuePoolIDs(context.Context) ([]uuid.UUID, error) { return nil, nil }
func (s *revokeQueueStore) GetRun(_ context.Context, id uuid.UUID) (types.AgentRun, error) {
	s.mu.Lock()
	s.read = append(s.read, id)
	s.mu.Unlock()
	if s.failRead[id] {
		return types.AgentRun{}, errors.New("read failed")
	}
	return types.AgentRun{ID: id, State: types.RunKilled}, nil
}

// One waiting run that cannot be read does not keep the owner's other pinned runs waiting: the sweep goes
// on to read the next, counts nothing it did not end, and returns the read's error. A launch that names
// the runner in upper case is pinned to it too, as the queue sweep's lower() compare has it.
func TestEndQueuedRunsPinnedTo_AFailedReadDoesNotStopTheRest(t *testing.T) {
	runner := uuid.New()
	entry := func(runnerID string) store.RunQueueEntry {
		req, _ := json.Marshal(createRunRequest{RunnerID: runnerID})
		launch, _ := json.Marshal(queuedLaunch{Request: req})
		return store.RunQueueEntry{RunID: uuid.New(), Launch: launch}
	}
	unreadable, other, later := entry(runner.String()), entry(uuid.NewString()), entry(strings.ToUpper(runner.String()))
	st := &revokeQueueStore{entries: []store.RunQueueEntry{unreadable, other, later}, failRead: map[uuid.UUID]bool{unreadable.RunID: true}}
	srv := New(Config{Store: st, Audit: &recRecorder{}})

	n, err := srv.endQueuedRunsPinnedTo(context.Background(), types.Runner{Owner: "owner"}, runner)
	if err == nil || n != 0 {
		t.Fatalf("ended %d with error %v, want 0 and the unreadable run's error", n, err)
	}
	if len(st.read) != 2 || st.read[0] != unreadable.RunID || st.read[1] != later.RunID {
		t.Errorf("runs read = %v, want the unreadable run, then the later pinned one, and not the other runner's", st.read)
	}
}

// A waiting run the queue sweep cannot end is not tried on every tick: the first retry waits
// revokedEndFirstWait, and each failure after it doubles the wait.
func TestEndQueuedRunsOfRevokedRunners_BacksOffARunItCannotEnd(t *testing.T) {
	run := uuid.New()
	st := &revokeQueueStore{entries: []store.RunQueueEntry{{RunID: run}}, failRead: map[uuid.UUID]bool{run: true}}
	now := time.Unix(1_000_000, 0)
	srv := New(Config{Store: st, Audit: &recRecorder{}, Now: func() time.Time { return now }})
	for i, step := range []struct {
		after time.Duration
		tries int
	}{
		{0, 1},                // the first try
		{10 * time.Second, 0}, // 10 s into the 30 s wait
		{20 * time.Second, 1}, // the wait is over; it fails again
		{30 * time.Second, 0}, // 30 s into a 60 s wait
		{30 * time.Second, 1}, // the 60 s wait is over
	} {
		now = now.Add(step.after)
		before := len(st.read)
		_ = srv.endQueuedRunsOfRevokedRunners(context.Background())
		if got := len(st.read) - before; got != step.tries {
			t.Fatalf("tick %d (+%s): tried %d times, want %d", i, step.after, got, step.tries)
		}
	}
}
