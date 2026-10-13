// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/api/types/container"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestProveRunnerSandboxAcceptsOnlyThisRunnersWardynContainers(t *testing.T) {
	runnerID, run := uuid.NewString(), uuid.New()
	agent, proxy := agentContainerName(run), proxyContainerName(run)
	mine := wardynLabels(run, componentAgent, map[string]string{runner.LabelRunnerID: runnerID})
	theirs := wardynLabels(run, componentAgent, map[string]string{runner.LabelRunnerID: uuid.NewString()})
	otherRun := wardynLabels(uuid.New(), componentAgent, map[string]string{runner.LabelRunnerID: runnerID})
	cases := []struct {
		name       string
		ref        string
		containers map[string]map[string]string
		ok         bool
	}{
		{name: "unrelated name", ref: "my-postgres", containers: map[string]map[string]string{"my-postgres": nil}},
		{name: "agent name without labels", ref: agent, containers: map[string]map[string]string{agent: nil}},
		{name: "another runner's agent", ref: agent, containers: map[string]map[string]string{agent: theirs}},
		{name: "agent labelled for another run", ref: agent, containers: map[string]map[string]string{agent: otherRun}},
		{name: "unmanaged agent", ref: agent, containers: map[string]map[string]string{agent: {runner.LabelRunnerID: runnerID, labelRun: run.String()}}},
		{name: "agent gone, another runner's proxy", ref: agent, containers: map[string]map[string]string{proxy: theirs}},
		{name: "this runner's agent beside a foreign proxy", ref: agent, containers: map[string]map[string]string{agent: mine, proxy: nil}},
		{name: "non-canonical run id", ref: agentNamePrefix + strings.ReplaceAll(run.String(), "-", ""), containers: map[string]map[string]string{agentNamePrefix + strings.ReplaceAll(run.String(), "-", ""): mine}},
		{name: "this runner's agent", ref: agent, containers: map[string]map[string]string{agent: mine, proxy: mine}, ok: true},
		{name: "agent gone, this runner's proxy", ref: agent, containers: map[string]map[string]string{proxy: mine}, ok: true},
		{name: "nothing left", ref: agent, ok: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeDocker()
			for name, labels := range tc.containers {
				f.containers[name] = &createdContainer{name: name, cfg: &container.Config{Labels: labels}, state: &container.State{Status: "running", Running: true}}
			}
			err := newTestDriver(f).ProveRunnerSandbox(t.Context(), tc.ref, runnerID)
			if (err == nil) != tc.ok || (err != nil && !errors.Is(err, runner.ErrNotRunnerSandbox)) {
				t.Fatalf("proof = %v, want ok = %v, a refusal being ErrNotRunnerSandbox", err, tc.ok)
			}
		})
	}
}

func TestSweepOrphanedSandboxesRemovesOnlyThisRunnersSandboxes(t *testing.T) {
	runnerID := uuid.NewString()
	mine, theirs, unlabelled := uuid.New(), uuid.New(), uuid.New()
	old := time.Now().Add(-time.Hour).Unix()
	items := []container.Summary{
		{ID: agentContainerName(mine), Created: old, Labels: wardynLabels(mine, componentAgent, map[string]string{runner.LabelRunnerID: runnerID})},
		{ID: agentContainerName(theirs), Created: old, Labels: wardynLabels(theirs, componentAgent, map[string]string{runner.LabelRunnerID: uuid.NewString()})},
		{ID: agentContainerName(unlabelled), Created: old, Labels: wardynLabels(unlabelled, componentAgent, nil)},
		{ID: driveSeedName(mine), Created: old, Labels: wardynLabels(mine, componentDriveSeed, map[string]string{runner.LabelRunnerID: runnerID})},
		{ID: driveSeedName(theirs), Created: old, Labels: wardynLabels(theirs, componentDriveSeed, map[string]string{runner.LabelRunnerID: uuid.NewString()})},
		{ID: driveSeedName(unlabelled), Created: old, Labels: wardynLabels(unlabelled, componentDriveSeed, nil)},
	}
	for _, tc := range []struct {
		name     string
		runnerID string
		swept    uuid.UUID
	}{
		{name: "runner", runnerID: runnerID, swept: mine},
		{name: "local install", runnerID: "", swept: unlabelled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeDocker()
			for _, id := range []uuid.UUID{mine, theirs, unlabelled} {
				seedSandbox(f, id)
				f.containers[driveSeedName(id)] = &createdContainer{name: driveSeedName(id)}
			}
			f.listItems = items
			d := newWithClient(f, Config{ProxyImage: "wardyn-proxy:dev", RunnerID: tc.runnerID})
			// The org's listing pass may ask for any age; it still reaches no other runner's seed.
			if _, err := d.SweepOrphanedSandboxes(t.Context(), 0, func(uuid.UUID) bool { return false }); err != nil {
				t.Fatal(err)
			}
			// The fake lists every item to both halves, so what was removed, not the count, is the answer.
			if _, err := d.SweepOrphanedSandboxes(t.Context(), 10*time.Minute, func(uuid.UUID) bool { return true }); err != nil {
				t.Fatal(err)
			}
			for _, id := range []uuid.UUID{mine, theirs, unlabelled} {
				for _, name := range []string{agentContainerName(id), driveSeedName(id)} {
					if c := f.containers[name]; (c != nil && c.removed) != (id == tc.swept) {
						t.Fatalf("%s removed = %v", name, c != nil && c.removed)
					}
				}
			}
		})
	}
}

func TestDriveSeedCarriesItsRunnersLabel(t *testing.T) {
	for _, runnerID := range []string{uuid.NewString(), ""} {
		f := newFakeDocker()
		run := uuid.New()
		d := newWithClient(f, Config{ProxyImage: "wardyn-proxy:dev", RunnerID: runnerID})
		if err := d.seedDriveVolume(t.Context(), run, "agent:dev", &types.DriveMount{DriveID: uuid.New(), ObjectName: "wardyn-drive-x"}); err != nil {
			t.Fatal(err)
		}
		c := f.containers[driveSeedName(run)]
		if c == nil || c.cfg == nil {
			t.Fatal("no seed was created")
		}
		if got, ok := c.cfg.Labels[runner.LabelRunnerID]; got != runnerID || ok != (runnerID != "") {
			t.Fatalf("seed runner label %q (set %v), want %q", got, ok, runnerID)
		}
	}
}
