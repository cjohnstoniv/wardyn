// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// failingKillRunner fails the sandbox kill with driver text a caller must never see.
type failingKillRunner struct{ *fakeRunner }

func (failingKillRunner) KillSandbox(context.Context, string) error {
	return errors.New("docker: dial tcp 10.9.8.7:2375: connection refused")
}

// A kill whose teardown fails answers 500 with the names of the steps that failed (an array), a registered
// reason, and none of the driver's text; the text is in the run.revoke row.
func TestKillRun_TeardownFailureBodyNamesStepsOnly(t *testing.T) {
	h := newHarness(t)
	runID := uuid.New()
	st := &killRekillStore{run: types.AgentRun{ID: runID, State: types.RunKilled, SandboxRef: "sbx-1"}}
	cfg := baseTestConfig(h, st)
	cfg.Runner = failingKillRunner{&fakeRunner{}}
	srv := New(cfg)

	w := do(t, srv, http.MethodPost, "/api/v1/runs/"+runID.String()+"/kill", adminToken, "")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("kill = %d %s, want 500", w.Code, w.Body.String())
	}
	var body struct {
		Errors []string `json:"errors"`
		Reason string   `json:"reason"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("body %s: %v", w.Body.String(), err)
	}
	if len(body.Errors) != 1 || body.Errors[0] != "runner_error" || body.Reason != "internal_error" {
		t.Errorf("errors %v, reason %q; want [runner_error] and the literal internal_error", body.Errors, body.Reason)
	}
	if strings.Contains(w.Body.String(), "10.9.8.7") {
		t.Errorf("body %s carries the driver's text", w.Body.String())
	}
}

// watcherReadStore fails the first GetRun and serves a runner-bound local run after that, stamping each read.
type watcherReadStore struct {
	store.Store
	mu    sync.Mutex
	reads []time.Time
}

func (s *watcherReadStore) GetRun(context.Context, uuid.UUID) (types.AgentRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads = append(s.reads, time.Now())
	if len(s.reads) == 1 {
		return types.AgentRun{}, errors.New("pool exhausted")
	}
	rid := uuid.New()
	return types.AgentRun{Placement: types.PlacementLocal, RunnerID: &rid}, nil
}

// The completion watcher's second read of its run waits a short bounded time after the first fails.
func TestStartCompletionWatcher_SecondReadWaitsAfterAFailedFirst(t *testing.T) {
	st := &watcherReadStore{}
	cfg := baseTestConfig(newHarness(t), st)
	cfg.Runner = &fakeRunner{}
	srv := New(cfg)
	srv.startCompletionWatcher(uuid.New(), "sbx", "")
	deadline := time.Now().Add(5 * time.Second)
	for {
		st.mu.Lock()
		n := len(st.reads)
		st.mu.Unlock()
		if n >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.reads) != 2 {
		t.Fatalf("reads = %d, want 2", len(st.reads))
	}
	if gap := st.reads[1].Sub(st.reads[0]); gap < completionWatcherReadRetryDelay-50*time.Millisecond || gap > 2*time.Second {
		t.Errorf("gap between the reads = %v, want about %v", gap, completionWatcherReadRetryDelay)
	}
}

// liveRunStore serves one run and counts reads of it.
type liveRunStore struct {
	store.Store
	run   types.AgentRun
	mu    sync.Mutex
	reads int
}

func (s *liveRunStore) GetRun(context.Context, uuid.UUID) (types.AgentRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	return s.run, nil
}

// A recording part reads its run once, in internalAuth's liveness gate, and the upload's audit row still
// carries the run's evidence label from that read.
func TestUploadRecordingPart_ReadsTheRunOnceAndKeepsTheEvidenceLabel(t *testing.T) {
	runID := uuid.New()
	for name, src := range map[string]types.RunEvidenceSource{
		"a runner-asserted run": types.RunEvidenceRunnerAsserted,
		"a substrate run":       types.RunEvidenceSubstrate,
	} {
		t.Run(name, func(t *testing.T) {
			h := newRecordingHarness(t, &fakeRecordingStore{}, nil)
			st := &liveRunStore{run: types.AgentRun{ID: runID, State: types.RunRunning, EvidenceSource: src}}
			cfg := h.srv.cfg
			cfg.Store = st
			h.srv = New(cfg)
			tok := h.mintRunToken(t, runID)
			w := do(t, h.srv, http.MethodPut, "/api/v1/internal/recordings/"+runID.String()+"/parts/2", tok, partsHeader)
			if w.Code != http.StatusNoContent {
				t.Fatalf("part upload = %d %s, want 204", w.Code, w.Body.String())
			}
			if st.reads != 1 {
				t.Errorf("run reads = %d, want 1 (the liveness gate's)", st.reads)
			}
			var label any
			for _, ev := range h.audit.events {
				if ev.Action == "recording.upload" && ev.Outcome == "success" {
					var d map[string]any
					_ = json.Unmarshal(ev.Data, &d)
					label = d["evidence_source"]
				}
			}
			if want := src == types.RunEvidenceRunnerAsserted; (label == string(types.RunEvidenceRunnerAsserted)) != want {
				t.Errorf("evidence_source = %v, want the runner-asserted label present = %v", label, want)
			}
		})
	}
}
