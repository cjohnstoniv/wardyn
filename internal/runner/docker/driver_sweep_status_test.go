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
		{ID: agentContainerName(orphan), Created: old, Labels: map[string]string{labelComponent: componentAgent, labelRun: orphan.String()}},
		{ID: agentContainerName(live), Created: old, Labels: map[string]string{labelComponent: componentAgent, labelRun: live.String()}},
		{ID: agentContainerName(young), Created: time.Now().Unix(), Labels: map[string]string{labelComponent: componentAgent, labelRun: young.String()}},
		{ID: "not-ours", Created: old, Labels: map[string]string{labelComponent: componentAgent, labelRun: "not-a-uuid"}},
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
	// All:true and label-scoped to Wardyn's sandbox components.
	if !f.lastListAll {
		t.Error("the orphan scan must list ALL containers — a crashed agent is exited, not running")
	}
	if !f.lastListFilters["label"][labelComponent] {
		t.Errorf("the orphan scan must filter on the component label, got %v", f.lastListFilters)
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

// An exec record is gone once its container stops. A container a daemon restart, a reboot or an
// operator stopped exits 143 (or was OOM killed): that is as definitive as a clean stop, so the agent
// status is the container's own, exit code included, never the "ambiguous" error that holds the run
// RUNNING until the 30 minute ceiling. A running container with no exec record stays ambiguous.
func TestAgentStatus_ExecGoneOnAFailedContainerIsDefinitive(t *testing.T) {
	for _, c := range []struct {
		name  string
		state container.State
		code  int
	}{
		{"exited 143", container.State{Status: "exited", ExitCode: 143}, 143},
		{"OOM killed", container.State{Status: "exited", ExitCode: 137, OOMKilled: true}, 0}, // reported as OOM killed, no code
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeDocker()
			f.execGone = "exec-1"
			f.containers["wardyn-agent-x"] = &createdContainer{name: "wardyn-agent-x", cfg: &container.Config{}, state: &c.state}
			st, err := newTestDriver(f).AgentStatus(context.Background(), "wardyn-agent-x", "exec-1")
			if err != nil {
				t.Fatalf("AgentStatus: %v", err)
			}
			if st.State != types.RunFailed || (c.code != 0 && (st.ExitCode == nil || *st.ExitCode != c.code)) {
				t.Fatalf("status = %+v, want FAILED (exit code %d)", st, c.code)
			}
		})
	}
	f := newFakeDocker()
	f.execGone = "exec-1"
	f.containers["wardyn-agent-x"] = &createdContainer{name: "wardyn-agent-x", cfg: &container.Config{}, state: &container.State{Status: "running", Running: true}}
	if _, err := newTestDriver(f).AgentStatus(context.Background(), "wardyn-agent-x", "exec-1"); err == nil {
		t.Fatal("exec gone on a running container answered definitively; want the ambiguous error")
	}
}

// Two deployments can share one Docker daemon (concurrent CI jobs, a desktop stack beside a dev
// one): an aged sandbox another deployment labelled is never this one's orphan, whatever this
// one's database says. This deployment's own, and an unlabelled one from before the label, are
// swept as before; a create labels every object with this deployment.
// A create that failed after the proxy started and before the agent existed, with the rollback's
// proxy removal refused, leaves a proxy (holding the run's config) and the per-run network and no agent:
// the sweep reaps them from the proxy. A run whose agent and proxy are both listed is torn down once,
// and a container of another component (a drive seed's own pass aside) keys nothing.
func TestSweepOrphanedSandboxes_ReapsAProxyWithNoAgent(t *testing.T) {
	alone, both, other := uuid.New(), uuid.New(), uuid.New()
	f := newFakeDocker()
	seedSandbox(f, both)
	seedSandbox(f, other)
	f.mu.Lock()
	f.containers[proxyContainerName(alone)] = &createdContainer{name: proxyContainerName(alone)}
	f.networks[internalNetName(alone)] = client.NetworkCreateOptions{Internal: true}
	f.mu.Unlock()
	old := time.Now().Add(-time.Hour).Unix()
	f.listItems = []container.Summary{
		{ID: proxyContainerName(alone), Created: old, Labels: wardynLabels(alone, componentProxy, nil)},
		{ID: agentContainerName(both), Created: old, Labels: wardynLabels(both, componentAgent, nil)},
		{ID: proxyContainerName(both), Created: old, Labels: wardynLabels(both, componentProxy, nil)},
		{ID: "other", Created: old, Labels: wardynLabels(other, "network", nil)},
	}
	swept, err := newTestDriver(f).SweepOrphanedSandboxes(context.Background(), 10*time.Minute, func(uuid.UUID) bool { return true })
	if err != nil || swept != 2 {
		t.Fatalf("SweepOrphanedSandboxes = %d, %v; want 2 (the lone proxy's run and the whole sandbox, once)", swept, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.containers[proxyContainerName(alone)]; !c.removed {
		t.Error("the proxy with no agent survived; it holds the run's config")
	}
	if _, ok := f.networks[internalNetName(alone)]; ok {
		t.Error("the lone proxy's per-run network survived")
	}
	if f.containers[agentContainerName(other)].removed {
		t.Error("a container of another component keyed a teardown")
	}
}

func TestSweepOrphanedSandboxes_KeepsToItsOwnDeployment(t *testing.T) {
	ours := uuid.MustParse("aaaaaaaa-0000-0000-0000-000000000001")
	theirs := uuid.MustParse("bbbbbbbb-0000-0000-0000-000000000002")
	unlabelled := uuid.MustParse("cccccccc-0000-0000-0000-000000000003")
	f := newFakeDocker()
	for _, id := range []uuid.UUID{ours, theirs, unlabelled} {
		seedSandbox(f, id)
	}
	old := time.Now().Add(-time.Hour).Unix()
	f.listItems = []container.Summary{
		{ID: agentContainerName(ours), Created: old, Labels: map[string]string{labelComponent: componentAgent, labelRun: ours.String(), labelDeployment: "wardyn-internal"}},
		{ID: agentContainerName(theirs), Created: old, Labels: map[string]string{labelComponent: componentAgent, labelRun: theirs.String(), labelDeployment: "ci-job-b-internal"}},
		{ID: agentContainerName(unlabelled), Created: old, Labels: map[string]string{labelComponent: componentAgent, labelRun: unlabelled.String()}},
	}
	d := newTestDriver(f)
	swept, err := d.SweepOrphanedSandboxes(context.Background(), 10*time.Minute, func(uuid.UUID) bool { return true })
	if err != nil || swept != 2 {
		t.Fatalf("SweepOrphanedSandboxes = %d, %v; want 2 (ours and the unlabelled one)", swept, err)
	}
	f.mu.Lock()
	gone := f.containers[agentContainerName(theirs)].removed
	f.mu.Unlock()
	if gone {
		t.Fatal("swept another deployment's sandbox")
	}
	if l := d.labels(ours, componentAgent, map[string]string{labelDeployment: "spoofed"}); l[labelDeployment] != "wardyn-internal" {
		t.Fatalf("labels = %v, want this deployment's %s stamped over any placement label", l, labelDeployment)
	}
}
