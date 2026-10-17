// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// queueListStore answers the four list reads the queue sweep makes; one of them can fail.
type queueListStore struct {
	store.Store
	queueStore
	failing string
}

func (q queueListStore) read(name string) error {
	if q.failing == name {
		return errors.New("read " + name + " failed")
	}
	return nil
}

func (q queueListStore) ExpiredQueueEntries(context.Context, time.Time, int) ([]store.RunQueueEntry, error) {
	return nil, q.read("expired")
}

func (q queueListStore) QueueEntriesOfRevokedRunners(context.Context, int) ([]store.RunQueueEntry, error) {
	return nil, q.read("revoked")
}

func (q queueListStore) StalePromotedRuns(context.Context, time.Duration) ([]types.AgentRun, error) {
	return nil, q.read("stale")
}

func (q queueListStore) QueuePoolIDs(context.Context) ([]uuid.UUID, error) {
	return nil, q.read("pools")
}

// A list read the queue sweep cannot make is returned, so the sweeper's health shows it instead of a
// tick that looks clean; a sweep whose reads all work returns nil.
func TestSweepRunQueueReturnsAListReadFailure(t *testing.T) {
	for _, failing := range []string{"", "expired", "revoked", "stale", "pools"} {
		t.Run("failing "+failing, func(t *testing.T) {
			h := newHarness(t)
			h.srv.cfg.Store = queueListStore{failing: failing}
			err := h.srv.SweepRunQueue(context.Background())
			if failing == "" {
				if err != nil {
					t.Fatalf("SweepRunQueue = %v, want nil when every read works", err)
				}
				return
			}
			want := "read " + failing + " failed"
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("SweepRunQueue = %v, want an error carrying %q", err, want)
			}
		})
	}
}
