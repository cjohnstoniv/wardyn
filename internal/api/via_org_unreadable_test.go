// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// flakyRunStore answers GetRun from fail: true is a failed read, false the run live on its runner.
type flakyRunStore struct {
	store.Store
	runner uuid.UUID
	reads  atomic.Int32
	fail   func(read int32) bool
}

func (f *flakyRunStore) GetRun(_ context.Context, id uuid.UUID) (types.AgentRun, error) {
	if f.fail(f.reads.Add(1)) {
		return types.AgentRun{}, errors.New("connection reset")
	}
	return types.AgentRun{ID: id, State: types.RunRunning, RunnerID: &f.runner}, nil
}

// A failed read of the run is a blip the streams survive; the second in a row ends them.
func TestViaOrg_OneFailedRunReadKeepsTheStreams(t *testing.T) {
	for _, tc := range []struct {
		name       string
		fail       func(read int32) bool
		wantClosed bool
	}{
		{"one failed read", func(read int32) bool { return read == 1 }, false},
		{"failures that are not in a row", func(read int32) bool { return read%2 == 1 }, false},
		{"two failed reads in a row", func(read int32) bool { return read <= 2 }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := uuid.New()
			st := &flakyRunStore{runner: runner, fail: tc.fail}
			ctx, stop := context.WithCancel(context.Background())
			defer stop()
			s := &Server{cfg: Config{Store: st, BaseCtx: ctx}}
			run := &viaOrgRun{id: uuid.New(), runner: runner, bytes: new(atomic.Int64), done: make(chan struct{}),
				streams: map[*viaOrgStream]struct{}{}, idleSince: time.Now(), idle: time.Hour, period: 2 * time.Millisecond}
			sctx, cancel := context.WithCancel(ctx)
			stream := &viaOrgStream{ctx: sctx, cancel: cancel, run: run}
			stream.last.Store(time.Now().UnixNano())
			run.streams[stream] = struct{}{}
			go s.watchViaOrgRun(run)
			defer func() { s.releaseViaOrgRun(run, true, false) }()

			waitFor(t, "the run read several times", func() bool { return st.reads.Load() >= 6 })
			if closed := sctx.Err() != nil; closed != tc.wantClosed {
				t.Errorf("stream closed = %v after %d reads, want %v", closed, st.reads.Load(), tc.wantClosed)
			}
		})
	}
}
