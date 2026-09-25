// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/api"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The control plane reaches the orphan sweep by type assertion on the Runner it
// holds, so a signature drift here would make the docker sweep a silent no-op
// rather than a build failure. This line makes it a build failure.
var _ api.SandboxOrphanSweeper = (*Driver)(nil)

// seedSandbox registers an agent container (plus its sibling proxy and per-run
// network) in the fake, so a teardown this sweep drives removes the real three
// objects rather than short-circuiting on a not-found agent.
func seedSandbox(f *fakeDocker, runID uuid.UUID) {
	name := agentContainerName(runID)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.containers[name] = &createdContainer{
		name: name,
		cfg:  &container.Config{Labels: wardynLabels(runID, componentAgent, nil)},
	}
	f.containers[proxyContainerName(runID)] = &createdContainer{name: proxyContainerName(runID)}
	f.networks[internalNetName(runID)] = client.NetworkCreateOptions{Internal: true}
}

// TestSweepOrphanedSandboxes_TearsDownOnlyOrphansPastMinAge is test-2/C1.
//
// The sweep had 0.0% coverage: inverting `if !isOrphan(runID)` — so it tore down
// every LIVE run's sandbox and spared the orphans, the worst outcome this
// function has — left the package green. The only test naming the symbol was an
// internal/api FAKE, which exercises the control plane's isOrphan predicate and
// nothing of the driver.
//
// Three skips and one teardown, all four driven through the real
// containerListerAPI seam:
//   - an orphan older than minAge is torn down (agent + proxy + network);
//   - a LIVE run's sandbox, same age, is left alone;
//   - a container younger than minAge is left alone even though it IS an orphan
//     (another replica's dispatch may not have written its ref yet);
//   - a container whose run-id label is not a UUID is left alone.
func TestSweepOrphanedSandboxes_TearsDownOnlyOrphansPastMinAge(t *testing.T) {
	orphan := uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000001")
	live := uuid.MustParse("bbbbbbbb-0000-0000-0000-000000000002")
	young := uuid.MustParse("cccccccc-0000-0000-0000-000000000003")

	f := newFakeDocker()
	for _, id := range []uuid.UUID{orphan, live, young} {
		seedSandbox(f, id)
	}
	old := time.Now().Add(-time.Hour).Unix()
	f.listItems = []container.Summary{
		{ID: agentContainerName(orphan), Created: old, Labels: map[string]string{labelRun: orphan.String()}},
		{ID: agentContainerName(live), Created: old, Labels: map[string]string{labelRun: live.String()}},
		{ID: agentContainerName(young), Created: time.Now().Unix(), Labels: map[string]string{labelRun: young.String()}},
		{ID: "not-ours", Created: old, Labels: map[string]string{labelRun: "not-a-uuid"}},
	}

	d := newTestDriver(f)
	swept, err := d.SweepOrphanedSandboxes(context.Background(), 10*time.Minute,
		func(id uuid.UUID) bool { return id != live })
	if err != nil {
		t.Fatalf("SweepOrphanedSandboxes: %v", err)
	}
	if swept != 1 {
		t.Fatalf("swept = %d, want exactly 1 (the aged orphan)", swept)
	}

	// A crashed sandbox's agent may be EXITED, not running, so the scan must be
	// All:true and label-scoped to the agent component.
	if !f.lastListAll {
		t.Error("the orphan scan must list ALL containers — a crashed agent is exited, not running")
	}
	if !f.lastListFilters["label"][labelComponent+"="+componentAgent] {
		t.Errorf("the orphan scan must filter on the agent component label, got %v", f.lastListFilters)
	}

	removed := func(name string) bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		c := f.containers[name]
		return c != nil && c.removed
	}
	if !removed(agentContainerName(orphan)) {
		t.Error("the aged orphan's agent container was not torn down")
	}
	// The whole sandbox, not just the agent: the proxy sidecar holds the run's
	// credentials and the network is the run's only route.
	if !removed(proxyContainerName(orphan)) {
		t.Error("the orphan's proxy sidecar survived — it holds the run's credentials")
	}
	if _, ok := f.networks[internalNetName(orphan)]; ok {
		t.Error("the orphan's per-run network survived")
	}
	for _, spared := range []struct {
		name string
		id   uuid.UUID
	}{{"a live run", live}, {"a container younger than minAge", young}} {
		if removed(agentContainerName(spared.id)) {
			t.Errorf("%s was torn down; the sweep must only reap aged orphans", spared.name)
		}
	}
}

// TestAgentStatus_MapsExecLiveness is test-2/C2.
//
// AgentStatus sat at 15.4% — the `agentExecID == "" || mainProcessExecID` early
// return only. Inverting `if insp.Running`, so a live agent reports Stopped and a
// dead one Running, left the package green: every internal/api caller uses a
// fake, and the boot reconciler finalizes on exactly this answer, so the
// inversion kills healthy runs and strands dead ones with nothing red.
func TestAgentStatus_MapsExecLiveness(t *testing.T) {
	t.Run("a running exec is RUNNING, with no exit code", func(t *testing.T) {
		f := newFakeDocker()
		f.execExited = false
		st, err := newTestDriver(f).AgentStatus(context.Background(), "wardyn-agent-x", "exec-1")
		if err != nil {
			t.Fatalf("AgentStatus: %v", err)
		}
		if st.State != types.RunRunning {
			t.Errorf("state = %q, want %q for a live exec", st.State, types.RunRunning)
		}
		if st.ExitCode != nil {
			t.Errorf("exit code = %d on a live agent, want none", *st.ExitCode)
		}
	})

	t.Run("an exited exec is STOPPED, carrying its real exit code", func(t *testing.T) {
		f := newFakeDocker()
		f.execExited, f.execExitCode = true, 7
		st, err := newTestDriver(f).AgentStatus(context.Background(), "wardyn-agent-x", "exec-1")
		if err != nil {
			t.Fatalf("AgentStatus: %v", err)
		}
		if st.State != types.RunStopped {
			t.Errorf("state = %q, want %q for an exited exec", st.State, types.RunStopped)
		}
		// The CODE is what decides COMPLETED vs FAILED upstream, so "stopped" alone
		// is not enough: a dropped code finalizes every run as a success.
		if st.ExitCode == nil || *st.ExitCode != 7 {
			t.Errorf("exit code = %v, want 7 — the agent's real exit decides completed vs failed", st.ExitCode)
		}
	})
}
