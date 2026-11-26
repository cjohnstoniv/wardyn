// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// mcpRowsMem is the run_mcp_sandboxes store in memory with the database's conditions: each write the store
// joins on agent_runs asks runOf for the run, so a test that kills or loses the run sees the write refused as
// the database refuses it. listErr fails the read and stopErr the stop write.
type mcpRowsMem struct {
	mu      sync.Mutex
	rows    []store.RunMCPSandbox
	clock   time.Time
	listErr error
	stopErr error
	// runOf reads the run a joined write is checked against; nil reads every run RUNNING and not lost.
	runOf func(uuid.UUID) (types.AgentRun, error)
}

func newMCPRowsMem(runID uuid.UUID, keys ...string) *mcpRowsMem {
	m := &mcpRowsMem{clock: time.Now().UTC().Truncate(time.Second)}
	for i, k := range keys {
		m.rows = append(m.rows, store.RunMCPSandbox{RunID: runID, Ordinal: i, ServerKey: k, State: store.MCPSandboxReady,
			SandboxRef: "ref-" + k, CPTokenJTI: "jti-" + k, CreatedAt: m.clock, UpdatedAt: m.clock})
	}
	return m
}

// tick is the next updated_at, so a write the revive's reopen compares against moves it.
func (m *mcpRowsMem) tick() time.Time {
	m.clock = m.clock.Add(time.Millisecond)
	return m.clock
}

func (m *mcpRowsMem) runLive(runID uuid.UUID, state types.RunState) bool {
	if m.runOf == nil {
		return state == types.RunRunning
	}
	run, err := m.runOf(runID)
	return err == nil && run.State == state && (state != types.RunRunning || run.LostAt == nil)
}

func (m *mcpRowsMem) row(runID uuid.UUID, ordinal int) *store.RunMCPSandbox {
	for i := range m.rows {
		if m.rows[i].RunID == runID && m.rows[i].Ordinal == ordinal {
			return &m.rows[i]
		}
	}
	return nil
}

// get is one row as the store holds it.
func (m *mcpRowsMem) get(ordinal int) store.RunMCPSandbox {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rows[slices.IndexFunc(m.rows, func(r store.RunMCPSandbox) bool { return r.Ordinal == ordinal })]
}

func (m *mcpRowsMem) all() []store.RunMCPSandbox {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.rows)
}

func (m *mcpRowsMem) InsertRunMCPSandbox(_ context.Context, row store.RunMCPSandbox) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.row(row.RunID, row.Ordinal) != nil {
		return errors.New("duplicate key")
	}
	row.CreatedAt, row.UpdatedAt = m.tick(), m.clock
	m.rows = append(m.rows, row)
	return nil
}

func (m *mcpRowsMem) ListRunMCPSandboxes(_ context.Context, runID uuid.UUID) ([]store.RunMCPSandbox, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.listErr != nil {
		return nil, m.listErr
	}
	var out []store.RunMCPSandbox
	for _, r := range m.rows {
		if r.RunID == runID {
			out = append(out, r)
		}
	}
	return out, nil
}

// write applies f to the row when ok holds for it, as one UPDATE ... WHERE.
func (m *mcpRowsMem) write(runID uuid.UUID, ordinal int, ok func(r *store.RunMCPSandbox) bool, f func(r *store.RunMCPSandbox)) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.row(runID, ordinal)
	if r == nil || !ok(r) {
		return false, nil
	}
	f(r)
	r.UpdatedAt = m.tick()
	return true, nil
}

func (m *mcpRowsMem) MarkMCPSandboxStartedIfStarting(_ context.Context, runID uuid.UUID, ordinal int, ref string) (bool, error) {
	return m.write(runID, ordinal, func(r *store.RunMCPSandbox) bool {
		return r.State == store.MCPSandboxStarting && m.runLive(runID, types.RunStarting)
	}, func(r *store.RunMCPSandbox) { r.State, r.SandboxRef = store.MCPSandboxReady, ref })
}

func (m *mcpRowsMem) FailMCPSandboxIfUp(_ context.Context, runID uuid.UUID, ordinal int, cause string) (bool, error) {
	return m.write(runID, ordinal, func(r *store.RunMCPSandbox) bool {
		return r.State == store.MCPSandboxStarting || r.State == store.MCPSandboxReady
	}, func(r *store.RunMCPSandbox) { r.State, r.LastCause = store.MCPSandboxFailed, cause })
}

func (m *mcpRowsMem) MarkMCPSandboxReadyIfLive(_ context.Context, runID uuid.UUID, ordinal int, ref string) (bool, error) {
	return m.write(runID, ordinal, func(r *store.RunMCPSandbox) bool {
		return r.State != store.MCPSandboxStopped && m.runLive(runID, types.RunRunning)
	}, func(r *store.RunMCPSandbox) { r.State, r.SandboxRef = store.MCPSandboxReady, ref })
}

func (m *mcpRowsMem) ReopenMCPSandboxIfLive(_ context.Context, runID uuid.UUID, ordinal int, readAt time.Time, jti string, prevUntil time.Time) (bool, error) {
	return m.write(runID, ordinal, func(r *store.RunMCPSandbox) bool {
		return r.UpdatedAt.Equal(readAt) && m.runLive(runID, types.RunRunning)
	}, func(r *store.RunMCPSandbox) {
		until := prevUntil
		r.State, r.SandboxRef, r.PrevCPTokenJTI, r.PrevCPTokenUntil, r.CPTokenJTI = store.MCPSandboxStarting, "", r.CPTokenJTI, &until, jti
		r.Restarts, r.RestartWindowStart, r.LastCause, r.StoppedAt = 0, m.clock, "", nil
	})
}

func (m *mcpRowsMem) KeepMCPSandboxStartingIfLive(_ context.Context, runID uuid.UUID, ordinal int, ref string) (bool, error) {
	return m.write(runID, ordinal, func(r *store.RunMCPSandbox) bool {
		return r.State == store.MCPSandboxStarting && m.runLive(runID, types.RunRunning)
	}, func(r *store.RunMCPSandbox) { r.SandboxRef = ref })
}

func (m *mcpRowsMem) FailMCPSandboxIfStarting(_ context.Context, runID uuid.UUID, ordinal int, cause string) (bool, error) {
	return m.write(runID, ordinal, func(r *store.RunMCPSandbox) bool { return r.State == store.MCPSandboxStarting },
		func(r *store.RunMCPSandbox) { r.State, r.LastCause = store.MCPSandboxFailed, cause })
}

func (m *mcpRowsMem) StopMCPSandboxRow(_ context.Context, runID uuid.UUID, ordinal int, cause string) (bool, error) {
	if m.stopErr != nil {
		return false, m.stopErr
	}
	return m.write(runID, ordinal, func(r *store.RunMCPSandbox) bool { return r.State != store.MCPSandboxStopped },
		func(r *store.RunMCPSandbox) {
			at := m.clock
			r.State, r.LastCause, r.StoppedAt = store.MCPSandboxStopped, cause, &at
		})
}

func (m *mcpRowsMem) RotateMCPSandboxToken(_ context.Context, runID uuid.UUID, ordinal int, oldJTI, newJTI string, oldUntil time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.row(runID, ordinal)
	if r == nil || r.CPTokenJTI != oldJTI || r.State == store.MCPSandboxStopped {
		return false, nil
	}
	until := oldUntil
	r.PrevCPTokenJTI, r.PrevCPTokenUntil, r.CPTokenJTI = oldJTI, &until, newJTI
	return true, nil
}

func (m *mcpRowsMem) RecordMCPSandboxCreated(_ context.Context, runID uuid.UUID, ordinal int, c store.MCPSandboxCreated) (bool, error) {
	return m.write(runID, ordinal, func(r *store.RunMCPSandbox) bool {
		return r.State == store.MCPSandboxStarting || r.State == store.MCPSandboxRestarting
	}, func(r *store.RunMCPSandbox) {
		r.RelayAddr, r.PolicyDigest, r.ProxyConfigSealed = c.RelayAddr, c.PolicyDigest, c.ProxyConfigSealed
	})
}

func (m *mcpRowsMem) RestartMCPSandboxIfLive(_ context.Context, runID uuid.UUID, ordinal int, rs store.MCPSandboxRestart) (bool, error) {
	return m.write(runID, ordinal, func(r *store.RunMCPSandbox) bool {
		return r.State != store.MCPSandboxStarting && r.UpdatedAt.Equal(rs.ReadAt) && m.runLive(runID, types.RunRunning)
	}, func(r *store.RunMCPSandbox) {
		until := rs.PrevUntil
		r.State, r.SandboxRef, r.PrevCPTokenJTI, r.PrevCPTokenUntil, r.CPTokenJTI = store.MCPSandboxRestarting, "", r.CPTokenJTI, &until, rs.JTI
		r.Restarts, r.RestartWindowStart, r.LastCause, r.StoppedAt = rs.Restarts, rs.WindowStart, "", nil
	})
}

var _ store.RunMCPSandboxStore = (*mcpRowsMem)(nil)

// mcpSandboxerFake is the runner's MCP sandboxes in memory: each created sandbox is held by ordinal until it
// is killed, and live is whether it was started and not killed. The hooks fail or block a call.
type mcpSandboxerFake struct {
	mu       sync.Mutex
	held     map[int]bool // created and not killed
	live     map[int]bool // started and not killed
	specs    []runner.MCPSandboxSpec
	calls    []string
	killAll  int
	createFn func(ctx context.Context, spec runner.MCPSandboxSpec) error
	startErr error
	killErr  error
	// statusDead reads every sandbox dead whatever its state, as a crashed server reads.
	statusDead bool
}

func newMCPSandboxerFake() *mcpSandboxerFake {
	return &mcpSandboxerFake{held: map[int]bool{}, live: map[int]bool{}}
}

func (f *mcpSandboxerFake) record(call string, ord int) {
	f.calls = append(f.calls, call+":"+strconv.Itoa(ord))
}

func (f *mcpSandboxerFake) CreateMCPSandbox(ctx context.Context, spec runner.MCPSandboxSpec) (runner.MCPSandbox, error) {
	if f.createFn != nil {
		if err := f.createFn(ctx, spec); err != nil {
			return runner.MCPSandbox{}, err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("create", spec.ID.Ordinal)
	f.specs = append(f.specs, spec)
	f.held[spec.ID.Ordinal] = true
	return runner.MCPSandbox{ID: spec.ID, RelayAddr: "172.30.0." + strconv.Itoa(2+spec.ID.Ordinal) + ":3130"}, nil
}

func (f *mcpSandboxerFake) StartMCPSandbox(_ context.Context, id runner.MCPSandboxID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("start", id.Ordinal)
	if f.startErr != nil {
		return f.startErr
	}
	if !f.held[id.Ordinal] {
		return runner.ErrMCPSandboxNotCreatedHere
	}
	f.live[id.Ordinal] = true
	return nil
}

func (f *mcpSandboxerFake) KillRunMCPSandboxes(context.Context, uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.killAll++
	f.held, f.live = map[int]bool{}, map[int]bool{}
	return nil
}

func (f *mcpSandboxerFake) KillMCPSandbox(_ context.Context, id runner.MCPSandboxID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record("kill", id.Ordinal)
	if f.killErr != nil {
		return f.killErr
	}
	delete(f.held, id.Ordinal)
	delete(f.live, id.Ordinal)
	return nil
}

func (f *mcpSandboxerFake) MCPSandboxStatus(_ context.Context, id runner.MCPSandboxID) (runner.MCPSandbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return runner.MCPSandbox{ID: id, Live: f.live[id.Ordinal] && !f.statusDead}, nil
}

// count is how many calls of kind the fake took.
func (f *mcpSandboxerFake) count(kind string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, kind+":") {
			n++
		}
	}
	return n
}

// left is how many sandboxes are still held: created and not killed.
func (f *mcpSandboxerFake) left() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.held)
}

// startAll marks every ordinal of rows created and started, as a dispatch that brought them up leaves them.
func (f *mcpSandboxerFake) startAll(rows []store.RunMCPSandbox) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range rows {
		f.held[r.Ordinal], f.live[r.Ordinal] = true, true
	}
}

var _ runner.MCPSandboxer = (*mcpSandboxerFake)(nil)
