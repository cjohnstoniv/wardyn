// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/substrate"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

type localSub struct {
	substrate.Substrate
	mu           sync.Mutex
	stops, kills int
	stopErr      error
	status       runner.Status
	agent        runner.Status
}

func (s *localSub) Name() string { return "docker" }
func (s *localSub) StopProxy(context.Context, string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stops++
	return s.stopErr
}
func (s *localSub) KillSandbox(context.Context, string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.kills++
	return nil
}
func (s *localSub) Status(context.Context, string) (runner.Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status, nil
}
func (s *localSub) AgentStatus(context.Context, string, string) (runner.Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.agent, nil
}

func managerFixture(t *testing.T) (*Manager, *localSub, *time.Duration, time.Time) {
	t.Helper()
	sub := &localSub{status: runner.Status{State: types.RunRunning}, agent: runner.Status{State: types.RunRunning}}
	m, err := New(t.TempDir(), uuid.New(), sub)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	base := time.Now()
	elapsed := time.Duration(0)
	m.now = func() time.Time { return base.Add(elapsed) }
	m.since = func(at time.Time) time.Duration { return base.Add(elapsed).Sub(at) }
	m.clock = NewClock(m.since)
	org := time.Date(2040, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := m.Clock(runnerwire.Hello{ServerTime: org}, base); err != nil {
		t.Fatal(err)
	}
	if err := m.BeforeReady(context.Background(), &runnerwire.Peer{}, false); err != nil {
		t.Fatal(err)
	}
	return m, sub, &elapsed, org
}
func createLocal(t *testing.T, m *Manager, org time.Time) uuid.UUID {
	t.Helper()
	id := uuid.New()
	ctx := context.Background()
	if err := m.Lease(ctx, runnerwire.Lease{RunID: id, TokenRenewedAt: org, EndsAt: org.Add(3 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	spec := runner.SandboxSpec{RunID: id}
	if err := m.BeforeCreate(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if err := m.Created(ctx, spec, id.String()); err != nil {
		t.Fatal(err)
	}
	return id
}
func TestOfflineKeepsAgentUntilMonotonicProxyLapse(t *testing.T) {
	m, sub, elapsed, org := managerFixture(t)
	id := createLocal(t, m, org)
	ctx := context.Background()
	m.Offline(ctx)
	*elapsed = identity.RunTokenLapseAfter - time.Nanosecond
	if err := m.CheckLapses(ctx); err != nil {
		t.Fatal(err)
	}
	if sub.stops != 0 || sub.kills != 0 {
		t.Fatal("link loss destroyed live sandbox")
	}
	// A reconnect's absurd wall-clock mapping and repeated old LEASE cannot
	// reset a deadline that was already anchored to elapsed monotonic time.
	if err := m.Clock(runnerwire.Hello{ServerTime: org.Add(-24 * time.Hour)}, m.now()); err != nil {
		t.Fatal(err)
	}
	if err := m.BeforeReady(ctx, &runnerwire.Peer{}, true); err != nil {
		t.Fatal(err)
	}
	if err := m.Lease(ctx, runnerwire.Lease{RunID: id, TokenRenewedAt: org, EndsAt: org.Add(3 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	*elapsed = identity.RunTokenLapseAfter
	if err := m.CheckLapses(ctx); err != nil {
		t.Fatal(err)
	}
	if sub.stops != 1 || sub.kills != 0 {
		t.Fatalf("lapse containment: stops=%d kills=%d", sub.stops, sub.kills)
	}
	rows, err := m.State(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ProxyStoppedAt == nil || !rows[0].SandboxPresent {
		t.Fatalf("STATE lost agent or containment: %+v", rows)
	}
	if err := m.BeforeRefCall(ctx, runnerwire.MethodReplaceProxy, id.String()); !errors.Is(err, runner.ErrRunnerOffline) {
		t.Fatalf("expired revive accepted: %v", err)
	}
}
func TestCreateRequiresInitialLeaseAndRejectsAmbiguousRetry(t *testing.T) {
	m, _, _, org := managerFixture(t)
	ctx := context.Background()
	id := uuid.New()
	if err := m.BeforeCreate(ctx, runner.SandboxSpec{RunID: id}); err == nil {
		t.Fatal("created before initial lease")
	}
	if len(m.state.Runs) != 0 {
		t.Fatal("failed admission wrote intent")
	}
	id = createLocal(t, m, org)
	if err := m.BeforeCreate(ctx, runner.SandboxSpec{RunID: id}); err == nil {
		t.Fatal("ambiguous retry accepted")
	}
	m.Offline(ctx)
	if err := m.BeforeCreate(ctx, runner.SandboxSpec{RunID: uuid.New()}); !errors.Is(err, runner.ErrRunnerOffline) {
		t.Fatalf("offline create %v", err)
	}
}
func TestRestartContainsUnknownDeadlineAndRetainsState(t *testing.T) {
	m, sub, _, org := managerFixture(t)
	id := createLocal(t, m, org)
	restarted, err := New(m.dir, m.runnerID, sub)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if err := restarted.CheckLapses(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sub.stops != 1 || sub.kills != 0 || restarted.state.Runs[id].Ref != id.String() {
		t.Fatal("restart widened unknown lease or lost binding")
	}
}
func TestRevocationPersistsRefusalAndRetriesContainment(t *testing.T) {
	m, sub, _, org := managerFixture(t)
	createLocal(t, m, org)
	sub.stopErr = errors.New("daemon unavailable")
	if err := m.Revoked(context.Background()); err == nil {
		t.Fatal("failed containment hidden")
	}
	if err := m.BeforeReady(context.Background(), &runnerwire.Peer{}, false); !errors.Is(err, runnerwire.ErrRevoked) {
		t.Fatal(err)
	}
	sub.stopErr = nil
	if err := m.CheckLapses(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sub.stops != 2 || sub.kills != 0 {
		t.Fatalf("revocation must stop only proxy: %d %d", sub.stops, sub.kills)
	}
}
func TestAgentExitUsesPersistedExecAndPendingActionsAreStable(t *testing.T) {
	m, sub, _, org := managerFixture(t)
	id := createLocal(t, m, org)
	ctx := context.Background()
	if err := m.AfterRefCall(ctx, runnerwire.MethodExec, id.String(), "exec-1"); err != nil {
		t.Fatal(err)
	}
	code := 7
	sub.agent = runner.Status{State: types.RunStopped, ExitCode: &code}
	if _, err := m.State(ctx); err != nil {
		t.Fatal(err)
	}
	if got := m.state.Runs[id].Exit; got == nil || got.Code != 7 {
		t.Fatalf("exit not durable: %+v", got)
	}
	a := runnerwire.PendingAction{ID: uuid.New(), RunID: id, Ref: "runner:" + m.runnerID.String() + "/" + id.String(), Kind: types.RunnerActionKill}
	first, err := m.apply(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.apply(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || sub.kills != 1 {
		t.Fatal("repeated pending action was reapplied")
	}
	a.Kind = types.RunnerActionEnd
	if _, err := m.apply(ctx, a); err == nil {
		t.Fatal("action identity substitution accepted")
	}
}
