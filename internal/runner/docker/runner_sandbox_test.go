// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/api/types/container"

	"github.com/cjohnstoniv/wardyn/internal/runner"
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
			if (err == nil) != tc.ok {
				t.Fatalf("proof = %v, want ok = %v", err, tc.ok)
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
			}
			f.listItems = items
			d := newWithClient(f, Config{ProxyImage: "wardyn-proxy:dev", RunnerID: tc.runnerID})
			n, err := d.SweepOrphanedSandboxes(t.Context(), 10*time.Minute, func(uuid.UUID) bool { return true })
			if err != nil || n != 1 {
				t.Fatalf("swept %d: %v", n, err)
			}
			for _, id := range []uuid.UUID{mine, theirs, unlabelled} {
				if c := f.containers[agentContainerName(id)]; (c != nil && c.removed) != (id == tc.swept) {
					t.Fatalf("run %s removed = %v", id, c != nil && c.removed)
				}
			}
		})
	}
}
