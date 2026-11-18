// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

// errNoBinding is the transient error the fake store answers a workload report with: a frame the rate admits goes on
// as far as the binding lookup and fails there, which is not a refusal.
var errNoBinding = errors.New("binding lookup unavailable")

// workloadRateStore is a store whose only workload method is the binding lookup, which fails transiently, and which
// records every RefuseRunnerEvent (refuseStoreFake).
type workloadRateStore struct {
	*refuseStoreFake
	store.WorkloadStore
}

func (workloadRateStore) BindingForRunner(context.Context, uuid.UUID, string) (store.WorkloadBinding, error) {
	return store.WorkloadBinding{}, errNoBinding
}

func workloadFrame(t *testing.T, data []byte) (context.Context, runnerwire.Event) {
	t.Helper()
	return remote.WithEventSession(t.Context(), "session-1"), runnerwire.Event{Kind: runnerwire.EventWorkload, Data: data}
}

func validWorkloadFrame(t *testing.T) (context.Context, runnerwire.Event) {
	t.Helper()
	return workloadFrame(t, mustJSON(runnerwire.WorkloadReport{Op: runnerwire.WorkloadOpSync, Namespace: "team-a"}))
}

// passes sends n valid workload frames from runnerID and fails unless each one gets past the rate.
func passes(t *testing.T, srv *Server, runnerID uuid.UUID, n int) {
	t.Helper()
	ctx, ev := validWorkloadFrame(t)
	for i := range n {
		err := srv.eventFromRunner(ctx, runnerID, "", ev)
		var refusal *runnerwire.EventRefusal
		if !errors.Is(err, errNoBinding) || errors.As(err, &refusal) {
			t.Fatalf("frame %d = %v, want the binding lookup's error: the rate should have admitted it", i+1, err)
		}
	}
}

// TestWorkloadEventRate_RefusesPastBurst: a runner may send a full burst at once; the next frame is refused
// rate_limited, recorded as a row, and acknowledged; a second later 10 more pass.
func TestWorkloadEventRate_RefusesPastBurst(t *testing.T) {
	fake := &refuseStoreFake{}
	now := time.Now()
	srv := refuseTestServer(workloadRateStore{refuseStoreFake: fake}, &now)
	runnerID := uuid.New()
	passes(t, srv, runnerID, int(workloadEventBurst))
	if got := fake.recorded(); len(got) != 0 {
		t.Fatalf("%d refusals recorded within the burst, want none", len(got))
	}
	ctx, ev := validWorkloadFrame(t)
	var refusal *runnerwire.EventRefusal
	if err := srv.eventFromRunner(ctx, runnerID, "", ev); !errors.As(err, &refusal) || refusal.Code != runnerwire.RefusalRateLimited {
		t.Fatalf("the frame past the burst = %v, want an EventRefusal %q", err, runnerwire.RefusalRateLimited)
	}
	got := fake.recorded()
	if len(got) != 1 || got[0].Code != runnerwire.RefusalRateLimited || got[0].Kind != runnerwire.EventWorkload || !got[0].Row {
		t.Fatalf("RefuseRunnerEvent calls = %+v, want one rate_limited workload refusal with its own row", got)
	}
	now = now.Add(time.Second)
	passes(t, srv, runnerID, int(workloadEventRatePerSec))
}

// TestWorkloadEventRate_PerRunner: one runner's drained bucket leaves another runner's full.
func TestWorkloadEventRate_PerRunner(t *testing.T) {
	now := time.Now()
	srv := refuseTestServer(workloadRateStore{refuseStoreFake: &refuseStoreFake{}}, &now)
	first, second := uuid.New(), uuid.New()
	passes(t, srv, first, int(workloadEventBurst))
	ctx, ev := validWorkloadFrame(t)
	var refusal *runnerwire.EventRefusal
	if err := srv.eventFromRunner(ctx, first, "", ev); !errors.As(err, &refusal) || refusal.Code != runnerwire.RefusalRateLimited {
		t.Fatalf("the drained runner's next frame = %v, want %q", err, runnerwire.RefusalRateLimited)
	}
	passes(t, srv, second, int(workloadEventBurst))
}

// TestWorkloadEventRate_ShapeFirst: a frame of the wrong shape is refused shape on a drained runner, and on a fresh
// one it spends nothing from the burst.
func TestWorkloadEventRate_ShapeFirst(t *testing.T) {
	fake := &refuseStoreFake{}
	now := time.Now()
	srv := refuseTestServer(workloadRateStore{refuseStoreFake: fake}, &now)
	drained, fresh := uuid.New(), uuid.New()
	passes(t, srv, drained, int(workloadEventBurst))
	ctx, bad := workloadFrame(t, []byte(`{`))
	var refusal *runnerwire.EventRefusal
	if err := srv.eventFromRunner(ctx, drained, "", bad); !errors.As(err, &refusal) || refusal.Code != runnerwire.RefusalShape {
		t.Fatalf("a malformed frame on a drained runner = %v, want %q", err, runnerwire.RefusalShape)
	}
	for range 3 {
		if err := srv.eventFromRunner(ctx, fresh, "", bad); !errors.As(err, &refusal) || refusal.Code != runnerwire.RefusalShape {
			t.Fatalf("a malformed frame on a fresh runner = %v, want %q", err, runnerwire.RefusalShape)
		}
	}
	passes(t, srv, fresh, int(workloadEventBurst))
}
