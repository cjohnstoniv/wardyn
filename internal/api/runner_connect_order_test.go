// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// slowRunnerRead is a RunnerStore whose GetRunner of one runner waits for release.
type slowRunnerRead struct {
	store.RunnerStore
	slow    uuid.UUID
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *slowRunnerRead) GetRunner(ctx context.Context, id uuid.UUID) (types.Runner, error) {
	if id == s.slow {
		s.once.Do(func() { close(s.entered) })
		<-s.release
	}
	return s.RunnerStore.GetRunner(ctx, id)
}

// blockingConnectAudit holds the runner.connect row of one runner inside Record until release.
type blockingConnectAudit struct {
	*recRecorder
	runner  uuid.UUID
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (a *blockingConnectAudit) Record(ctx context.Context, ev types.AuditEvent) error {
	if ev.Action == placement.ActionRunnerConnect && ev.Target == a.runner.String() {
		a.once.Do(func() { close(a.entered) })
		<-a.release
	}
	return a.recRecorder.Record(ctx, ev)
}

func runnerAuditRows(h *harness, action string, id uuid.UUID) []types.AuditEvent {
	var out []types.AuditEvent
	for _, ev := range h.audit.snapshot() {
		if ev.Action == action && ev.Target == id.String() {
			out = append(out, ev)
		}
	}
	return out
}

func awaitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// One runner's slow read, while it writes its owed runner.connect row, holds no other runner's connect.
func TestWriteOwedRunnerConnect_ASlowReadHoldsNoOtherRunner(t *testing.T) {
	srv, f, h := newRunnerRegistrationServer(t)
	a := f.addRunner(srv, "alice", "laptop-a", types.RunnerClaimed, time.Hour)
	b := f.addRunner(srv, "bob", "laptop-b", types.RunnerClaimed, time.Hour)
	srv.runnerStreams.connectMu.Lock()
	srv.runnerStreams.connectOwed = map[uuid.UUID]string{a.ID: "sa", b.ID: "sb"}
	srv.runnerStreams.connectMu.Unlock()
	st := &slowRunnerRead{RunnerStore: f, slow: a.ID, entered: make(chan struct{}), release: make(chan struct{})}
	ctx := context.Background()

	aDone := make(chan struct{})
	go func() {
		defer close(aDone)
		srv.writeOwedRunnerConnect(ctx, st, a.ID, "sa", "")
	}()
	awaitClosed(t, st.entered, "A's runner read")

	bDone := make(chan struct{})
	go func() {
		defer close(bDone)
		srv.writeOwedRunnerConnect(ctx, st, b.ID, "sb", "")
	}()
	awaitClosed(t, bDone, "B's runner.connect while A's read is held")
	if got := runnerAuditRows(h, placement.ActionRunnerConnect, b.ID); len(got) != 1 {
		t.Fatalf("B's runner.connect rows = %d, want 1", len(got))
	}
	if got := runnerAuditRows(h, placement.ActionRunnerConnect, a.ID); len(got) != 0 {
		t.Fatalf("A's runner.connect was written before its read returned: %d rows", len(got))
	}

	close(st.release)
	awaitClosed(t, aDone, "A's runner.connect after its read")
	if got := runnerAuditRows(h, placement.ActionRunnerConnect, a.ID); len(got) != 1 {
		t.Fatalf("A's runner.connect rows = %d, want 1", len(got))
	}
}

// A runner.disconnect waits for the runner.connect the session owed that another path is still writing, so every
// disconnect follows its connect.
func TestRunnerDisconnectedFollowsTheConnectRowCapsIsWriting(t *testing.T) {
	srv, f, h := newRunnerRegistrationServer(t)
	a := f.addRunner(srv, "alice", "laptop", types.RunnerClaimed, time.Hour)
	srv.runnerStreams.connectMu.Lock()
	srv.runnerStreams.connectOwed = map[uuid.UUID]string{a.ID: "sa"}
	srv.runnerStreams.connectMu.Unlock()
	audit := &blockingConnectAudit{recRecorder: h.audit, runner: a.ID, entered: make(chan struct{}), release: make(chan struct{})}
	srv.cfg.Audit = audit
	ctx := context.Background()

	connectDone := make(chan struct{})
	go func() {
		defer close(connectDone)
		srv.writeOwedRunnerConnect(ctx, f, a.ID, "sa", "1.2.3")
	}()
	awaitClosed(t, audit.entered, "the runner.connect write")

	disconnectDone := make(chan struct{})
	go func() {
		defer close(disconnectDone)
		_ = srv.runnerDisconnected(ctx, f, a.ID)
	}()
	time.Sleep(200 * time.Millisecond)
	if got := runnerAuditRows(h, placement.ActionRunnerDisconnect, a.ID); len(got) != 0 {
		t.Fatalf("runner.disconnect was written while runner.connect was still being written: %d rows", len(got))
	}

	close(audit.release)
	awaitClosed(t, connectDone, "runner.connect")
	awaitClosed(t, disconnectDone, "runner.disconnect")
	var order []string
	var connectData string
	for _, ev := range h.audit.snapshot() {
		if ev.Target != a.ID.String() {
			continue
		}
		switch ev.Action {
		case placement.ActionRunnerConnect:
			order = append(order, "connect")
			connectData = string(ev.Data)
		case placement.ActionRunnerDisconnect:
			order = append(order, "disconnect")
		}
	}
	if len(order) != 2 || order[0] != "connect" || order[1] != "disconnect" {
		t.Fatalf("rows = %v, want [connect disconnect]", order)
	}
	if want := `{"resumed":false,"version":"1.2.3"}`; connectData != want {
		t.Fatalf("runner.connect data = %s, want %s", connectData, want)
	}
}
