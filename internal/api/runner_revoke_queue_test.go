// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

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
// on to read the next, counts nothing it did not end, and returns the read's error.
func TestEndQueuedRunsPinnedTo_AFailedReadDoesNotStopTheRest(t *testing.T) {
	runner := uuid.New()
	entry := func(runnerID string) store.RunQueueEntry {
		req, _ := json.Marshal(createRunRequest{RunnerID: runnerID})
		launch, _ := json.Marshal(queuedLaunch{Request: req})
		return store.RunQueueEntry{RunID: uuid.New(), Launch: launch}
	}
	unreadable, other, later := entry(runner.String()), entry(uuid.NewString()), entry(runner.String())
	st := &revokeQueueStore{entries: []store.RunQueueEntry{unreadable, other, later}, failRead: map[uuid.UUID]bool{unreadable.RunID: true}}
	srv := New(Config{Store: st, Audit: &recRecorder{}})

	n, err := srv.endQueuedRunsPinnedTo(context.Background(), "owner", runner)
	if err == nil || n != 0 {
		t.Fatalf("ended %d with error %v, want 0 and the unreadable run's error", n, err)
	}
	if len(st.read) != 2 || st.read[0] != unreadable.RunID || st.read[1] != later.RunID {
		t.Errorf("runs read = %v, want the unreadable run, then the later pinned one, and not the other runner's", st.read)
	}
}
