// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// gpuStartRunner is the revive runner that can also start a stopped agent, counting the starts.
type gpuStartRunner struct {
	*reviveRunner
	mu     sync.Mutex
	starts int
}

func (r *gpuStartRunner) StartSandbox(context.Context, string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.starts++
	return nil
}

// gpuOwedStore is the revive store that answers whether a live run's agent start is still owed.
type gpuOwedStore struct {
	*reviveStore
	owed bool
}

func (s *gpuOwedStore) RunAgentStopped(context.Context, uuid.UUID) (bool, error) { return s.owed, nil }

// gpuReviveFixture is the revive fixture with a runner that can start an agent, and its run (a copy)
// made a GPU run. supported calls reviveSupported on the copy, as a revive door does.
type gpuReviveFixture struct {
	*reviveFixture
	starter *gpuStartRunner
	run     types.AgentRun
}

func newGPUReviveFixture(t *testing.T) *gpuReviveFixture {
	t.Helper()
	f := &gpuReviveFixture{reviveFixture: newReviveFixture(t)}
	f.starter = &gpuStartRunner{reviveRunner: f.rr}
	f.srv.cfg.Runner = f.starter
	f.run = f.reviveFixture.run
	f.run.GPU = types.GPUNvidia
	return f
}

func (f *gpuReviveFixture) supported(t *testing.T, startAgent bool) (rebooted bool, rerr *reviveError) {
	t.Helper()
	_, _, starter, rebooted, rerr := f.srv.reviveSupported(context.Background(), f.run, startAgent)
	if rerr != nil && starter != nil {
		t.Error("a refused revive still returned a starter")
	}
	return rebooted, rerr
}

func (f *gpuReviveFixture) wantGPURefusal(t *testing.T, startAgent bool) {
	t.Helper()
	_, rerr := f.supported(t, startAgent)
	if rerr == nil || rerr.status != http.StatusConflict || rerr.reason != reasonReviveGPUAgentStart {
		t.Fatalf("revive = %+v, want 409 %s", rerr, reasonReviveGPUAgentStart)
	}
	if strings.Contains(rerr.msg, "in this release") {
		t.Errorf("refusal %q promises a later release", rerr.msg)
	}
	f.starter.mu.Lock()
	defer f.starter.mu.Unlock()
	if f.starter.starts != 0 {
		t.Errorf("the agent was started %d times by a refused revive", f.starter.starts)
	}
}

func TestReviveSupported_GPURunAgentStartRefused(t *testing.T) {
	f := newGPUReviveFixture(t)
	f.run.LostReason = types.LostReboot
	f.wantGPURefusal(t, true)
}

// The positive control: a live GPU run with no owed agent start revives its proxy only, as today. It must fail
// if the GPU check is widened to every GPU revive.
func TestReviveSupported_GPURunProxyOnlyAllowed(t *testing.T) {
	f := newGPUReviveFixture(t)
	f.run.LostAt, f.run.LostReason = nil, ""
	rebooted, rerr := f.supported(t, true)
	if rerr != nil || rebooted {
		t.Fatalf("a live GPU run's proxy-only revive = rebooted %v, %+v; want allowed and no agent start", rebooted, rerr)
	}
}

func TestReviveSupported_NoGPURebootUnchanged(t *testing.T) {
	f := newGPUReviveFixture(t)
	f.run.GPU = types.GPUNone
	f.run.LostReason = types.LostReboot
	rebooted, rerr := f.supported(t, true)
	if rerr != nil || !rebooted {
		t.Fatalf("a no-GPU reboot revive = rebooted %v, %+v; want allowed, starting its agent", rebooted, rerr)
	}
}

func TestReviveSupported_GPUEndedRunRefused(t *testing.T) {
	f := newGPUReviveFixture(t)
	f.run.Interactive, f.run.LostReason = true, types.LostEnded
	f.wantGPURefusal(t, true)
}

func TestReviveSupported_GPUOutageAgentStoppedRefused(t *testing.T) {
	f := newGPUReviveFixture(t)
	f.rr.status = types.RunCompleted // the outage run's agent is stopped
	f.wantGPURefusal(t, true)
	// A bulk restart (startAgent false) meets the GPU answer, not the bulk one: it fails if the check sits below
	// the bulk refusal.
	f.wantGPURefusal(t, false)
}

func TestReviveSupported_GPUOutageAgentRunningAllowed(t *testing.T) {
	f := newGPUReviveFixture(t) // status "" reads as running: the agent is up, so the revive is proxy-only
	rebooted, rerr := f.supported(t, true)
	if rerr != nil || rebooted {
		t.Fatalf("an outage GPU run whose agent runs = rebooted %v, %+v; want a proxy-only revive", rebooted, rerr)
	}
}

func TestReviveSupported_GPUOwedStartRefused(t *testing.T) {
	f := newGPUReviveFixture(t)
	f.srv.cfg.Store = &gpuOwedStore{reviveStore: f.rs, owed: true}
	f.run.LostAt, f.run.LostReason = nil, ""
	f.wantGPURefusal(t, true)
}

// A GPU run past its end is answered with the GPU refusal, never revive_past_end: extending it would not make it
// revivable. It fails if the early refusal is moved below reviveEligible.
func TestReviveSupported_GPUPastEndAnswersGPU(t *testing.T) {
	for _, reason := range []types.LostReason{types.LostReboot, types.LostEnded} {
		f := newGPUReviveFixture(t)
		end := f.now.Add(-time.Minute)
		f.run.LostReason, f.run.EndsAt, f.run.Interactive = reason, &end, true
		f.wantGPURefusal(t, true)
	}
}

func TestReviveSupported_GPUBulkAnswersGPU(t *testing.T) {
	f := newGPUReviveFixture(t)
	f.run.LostReason = types.LostReboot
	f.wantGPURefusal(t, false)
}
