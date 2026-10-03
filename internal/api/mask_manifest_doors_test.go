// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/maskmanifest"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// noKeys is the key service of a manifest store that must never seal: every
// door here is decided before a value would be.
type noKeys struct{}

func (noKeys) Current(context.Context, string, string) (int, []byte, error) {
	return 0, nil, context.Canceled
}

func (noKeys) Key(context.Context, string, string, int) ([]byte, error) {
	return nil, context.Canceled
}

// maskDoorServer is a Server whose masking manifests sit on a Postgres that
// refuses every connection (pgxpool dials lazily), which is "this process
// cannot prove the run's corpus whole": every door must refuse.
func maskDoorServer(t *testing.T) (*Server, *memAudit, *secretmask.Registry) {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://wardyn@127.0.0.1:1/wardyn?connect_timeout=1&sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	reg := secretmask.NewRegistry()
	rec := &memAudit{}
	return &Server{
		cfg:      Config{Audit: rec, Now: time.Now, MaskRegistry: reg, MaskManifests: maskmanifest.New(pool, noKeys{}, reg)},
		maskBeat: 10 * time.Millisecond,
	}, rec, reg
}

func TestMaskCoverageIsNotGatedWhenNoManifestsAreKept(t *testing.T) {
	s := &Server{cfg: Config{Audit: &memAudit{}, Now: time.Now, MaskRegistry: secretmask.NewRegistry()}}
	run := uuid.New()

	if !s.maskCovered(context.Background(), run) {
		t.Error("a deployment keeping no manifests must gate nothing")
	}
	if s.maskGuard(run) != nil {
		t.Error("maskGuard returned a check for a deployment keeping no manifests")
	}
	if s.holdMaskFence(context.Background(), run, func() { t.Error("end called") })() {
		t.Error("holdMaskFence reported a fence with no manifests")
	}
	w := httptest.NewRecorder()
	if s.refuseUncovered(w, httptest.NewRequest(http.MethodGet, "/", nil), run, "door") || w.Code != http.StatusOK {
		t.Errorf("refuseUncovered refused with no manifests: %d", w.Code)
	}
	if !s.beginMaskManifest(context.Background(), types.AgentRun{ID: run}) || !s.completeMaskManifest(context.Background(), types.AgentRun{ID: run}) {
		t.Error("begin/complete failed the run with no manifests")
	}
	if err := s.loadMaskManifests(context.Background()); err != nil {
		t.Errorf("loadMaskManifests = %v", err)
	}
	s.forgetMaskManifest(run) // a no-op, not a nil dereference
}

func TestMaskCheckPeriod(t *testing.T) {
	if got := (&Server{}).maskCheckPeriod(); got != maskCheckEvery {
		t.Errorf("default period = %v, want %v", got, maskCheckEvery)
	}
	if got := (&Server{maskBeat: time.Second}).maskCheckPeriod(); got != time.Second {
		t.Errorf("overridden period = %v", got)
	}
}

func TestMaskRefusalNamesTheRunDoorAndGlobalsOnlyScope(t *testing.T) {
	run := uuid.New()
	d := maskRefusal(run, "attach")
	if d.Reason != authz.ReasonMaskStateUnavailable || d.Target != "attach" || d.RunID == nil || *d.RunID != run {
		t.Fatalf("decision = %+v", d)
	}
	if d.Detail["mask_scope"] != maskScopeGlobalsOnly {
		t.Fatalf("detail = %v, want mask_scope %q", d.Detail, maskScopeGlobalsOnly)
	}
}

func TestAnUncoveredRunRefusesTheDoorWith503AndADeniedRow(t *testing.T) {
	s, rec, _ := maskDoorServer(t)
	run := uuid.New()

	w := httptest.NewRecorder()
	if !s.refuseUncovered(w, httptest.NewRequest(http.MethodGet, "/", nil), run, "run.attach") {
		t.Fatal("an uncovered run was admitted")
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	var body errorBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Reason != string(authz.ReasonMaskStateUnavailable) {
		t.Fatalf("body = %s (%v), want reason %s", w.Body.String(), err, authz.ReasonMaskStateUnavailable)
	}
	rows := rec.find(authz.AuditAction)
	if len(rows) != 1 || rows[0].Outcome != "denied" || rows[0].Target != "run.attach" {
		t.Fatalf("audit rows = %+v, want one denied row for run.attach", rows)
	}
	var data map[string]any
	_ = json.Unmarshal(rows[0].Data, &data)
	if data["mask_scope"] != maskScopeGlobalsOnly {
		t.Errorf("row data = %v, want mask_scope %q", data, maskScopeGlobalsOnly)
	}
}

func TestAnUncoveredRunRefusesTheRunTokenDoorAsTheAgent(t *testing.T) {
	s, rec, _ := maskDoorServer(t)
	claims := &identity.Claims{RunID: uuid.New(), SPIFFEID: "spiffe://wardyn/agent-run/x"}

	w := httptest.NewRecorder()
	if !s.refuseUncoveredAgent(w, httptest.NewRequest(http.MethodPost, "/", nil), claims, "internal.upload") {
		t.Fatal("an uncovered run's token was admitted")
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	rows := rec.find(authz.AuditAction)
	if len(rows) != 1 || rows[0].ActorType != types.ActorAgent || rows[0].Actor != claims.SPIFFEID {
		t.Fatalf("audit rows = %+v, want one row by the run token's SPIFFE id", rows)
	}
}

func TestAuditUncoveredRecordsTheAskingActorWithoutAResponse(t *testing.T) {
	s, rec, _ := maskDoorServer(t)
	run := uuid.New()
	s.auditUncovered(context.Background(), run, types.ActorHuman, "alice@example.com", "ssh.shell")

	rows := rec.find(authz.AuditAction)
	if len(rows) != 1 || rows[0].Actor != "alice@example.com" || rows[0].ActorType != types.ActorHuman || rows[0].Target != "ssh.shell" {
		t.Fatalf("audit rows = %+v", rows)
	}
}

func TestHoldMaskFenceEndsTheConsumerOnTheFirstMiss(t *testing.T) {
	s, _, _ := maskDoorServer(t)
	ended := make(chan struct{})
	fenced := s.holdMaskFence(context.Background(), uuid.New(), func() { close(ended) })

	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the consumer was not ended although the run is uncovered")
	}
	deadline := time.Now().Add(time.Second)
	for !fenced() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !fenced() {
		t.Fatal("holdMaskFence did not report it ended the consumer")
	}
}

func TestHoldMaskFenceStopsQuietlyWhenTheConsumerEndsFirst(t *testing.T) {
	s, _, _ := maskDoorServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fenced := s.holdMaskFence(ctx, uuid.New(), func() { t.Error("end called for a consumer that already ended") })
	time.Sleep(50 * time.Millisecond)
	if fenced() {
		t.Fatal("a consumer that ended on its own was reported fenced")
	}
}

func TestMaskDispatchValueRegistersBeforeCommittingAndFailsClosed(t *testing.T) {
	s, _, reg := maskDoorServer(t)
	run := uuid.New()
	value := []byte("0123456789abcdef")

	if err := s.maskDispatchValue(context.Background(), run, value); err == nil {
		t.Fatal("a value that could not be committed was reported committed")
	}
	if got := reg.Snapshot(run); len(got) != 1 {
		t.Errorf("registry holds %d values; the value masks as soon as it is resolved, committed or not", len(got))
	}
	if err := s.maskMintedValue(context.Background(), run, value); err == nil {
		t.Error("a minted value that could not be committed was reported committed")
	}

	bare := &Server{cfg: Config{MaskRegistry: secretmask.NewRegistry()}}
	other := uuid.New()
	if err := bare.maskMintedValue(context.Background(), other, value); err != nil {
		t.Errorf("with no manifests a minted value = %v, want nil", err)
	}
	if len(bare.cfg.MaskRegistry.Snapshot(other)) != 1 {
		t.Error("with no manifests the value still has to mask")
	}
}

// failedRunsStore records the CAS failAndRevoke lands and serves a fixed run
// list; the CAS reports a concurrent winner so no cascade runs.
type failedRunsStore struct {
	store.Store
	runs []types.AgentRun
	cas  []types.RunState // from, to of each CAS
	err  error
}

func (f *failedRunsStore) UpdateRunStateIf(_ context.Context, _ uuid.UUID, from, to types.RunState) (bool, error) {
	f.cas = append(f.cas, from, to)
	return false, nil
}

func (f *failedRunsStore) ListRuns(context.Context) ([]types.AgentRun, error) { return f.runs, f.err }

// A run whose manifest cannot be committed is failed, never launched: its
// secrets could not be masked after a restart.
func TestARunWhoseManifestCannotBeCommittedIsFailedNotLaunched(t *testing.T) {
	for name, step := range map[string]func(*Server, context.Context, types.AgentRun) bool{
		"start":    (*Server).beginMaskManifest,
		"complete": (*Server).completeMaskManifest,
	} {
		t.Run(name, func(t *testing.T) {
			s, rec, _ := maskDoorServer(t)
			st := &failedRunsStore{}
			s.cfg.Store = st
			run := types.AgentRun{ID: uuid.New(), CreatedBy: "alice@example.com"}

			if step(s, context.Background(), run) {
				t.Fatal("the dispatch went on although the manifest was not committed")
			}
			if want := []types.RunState{types.RunStarting, types.RunFailed}; len(st.cas) != 2 || st.cas[0] != want[0] || st.cas[1] != want[1] {
				t.Fatalf("run state CAS = %v, want STARTING -> FAILED", st.cas)
			}
			rows := rec.find("run.dispatch")
			if len(rows) != 1 || rows[0].Outcome != "failure" || rows[0].Target != run.ID.String() {
				t.Fatalf("audit rows = %+v, want one run.dispatch failure for the run", rows)
			}
			if !strings.Contains(string(rows[0].Data), "("+name+")") {
				t.Errorf("the row does not say which step failed: %s", rows[0].Data)
			}
		})
	}
}

func TestLoadMaskManifestsAsksOnlyForLiveRuns(t *testing.T) {
	s, _, _ := maskDoorServer(t)
	s.cfg.Store = &failedRunsStore{runs: []types.AgentRun{
		{ID: uuid.New(), State: types.RunRunning},
		{ID: uuid.New(), State: types.RunFailed},
	}}
	if err := s.loadMaskManifests(context.Background()); err != nil {
		t.Fatalf("loadMaskManifests = %v; an unprovable run is left uncovered, not an error", err)
	}

	s.cfg.Store = &failedRunsStore{err: errors.New("list failed")}
	if err := s.loadMaskManifests(context.Background()); err == nil {
		t.Fatal("a failed run listing was swallowed")
	}
}

func TestEndSSHOnMaskFenceTellsTheShellThenCancelsIt(t *testing.T) {
	s, _, _ := maskDoorServer(t)
	ch := newFakeSSHChannel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelled := make(chan struct{})
	fenced := s.endSSHOnMaskFence(ctx, uuid.New(), ch, func() { close(cancelled) })

	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("the shell of an uncovered run was not cancelled")
	}
	if got := ch.stderrString(); !strings.Contains(got, "can no longer prove this run's secrets are masked") {
		t.Errorf("stderr = %q, want the courtesy line", got)
	}
	deadline := time.Now().Add(time.Second)
	for !fenced() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !fenced() {
		t.Error("endSSHOnMaskFence did not report it ended the shell")
	}
}
