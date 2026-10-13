// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

type sizingStore struct {
	store.Store
	n   int
	err error
}

func (s *sizingStore) SetRunSizing(context.Context, uuid.UUID, store.RunSizing) error {
	s.n++
	return s.err
}

// The record is what each substrate applies: k8s requests come after the ratio, a
// policy-set 3000m/6144Mi beats the deployment default, and Docker records its hard cap.
// Two different ratio and proxy settings give two different records.
func TestRunSizingParity(t *testing.T) {
	t.Cleanup(func() {
		_ = runner.SetRequestRatio(0)
		runner.SetDefaultLimits(0, 0)
		runner.SetProxyLimits(0, 0)
	})
	runner.SetDefaultLimits(1000, 2048)
	policy3000 := runner.Resources{CPUMillis: 3000, MemoryMiB: 6144}
	for _, tc := range []struct {
		name                                   string
		kind                                   string
		ratio                                  float64
		proxyCPU                               int64
		res                                    runner.Resources
		cpuReq, memReq, cpuLim, memLim, pxyCPU int64
	}{
		{"k8s policy 3000m/6144Mi, ratio 0.5", "k8s", 0.5, 500, policy3000, 1500, 3072, 3000, 6144, 500},
		{"k8s policy 3000m/6144Mi, no ratio, bigger proxy", "k8s", 0, 750, policy3000, 3000, 6144, 3000, 6144, 750},
		{"k8s defaults, ratio 0.25", "k8s", 0.25, 500, runner.Resources{}, 250, 512, 1000, 2048, 500},
		{"docker policy 3000m/6144Mi ignores the ratio", "docker", 0.5, 500, policy3000, 3000, 6144, 3000, 6144, 500},
	} {
		if err := runner.SetRequestRatio(tc.ratio); err != nil {
			t.Fatal(err)
		}
		runner.SetProxyLimits(tc.proxyCPU, 256)
		z := runSizing(tc.kind, tc.res, false)
		if z.RunnerKind != tc.kind || z.AgentCPURequestMillis != tc.cpuReq || z.AgentMemoryRequestMiB != tc.memReq ||
			z.AgentCPULimitMillis != tc.cpuLim || z.AgentMemoryLimitMiB != tc.memLim ||
			z.ProxyCPUMillis == nil || *z.ProxyCPUMillis != tc.pxyCPU || z.ProxyMemoryMiB != 256 {
			t.Errorf("%s: recorded %+v", tc.name, z)
		}
	}
}

// A failed write is logged and dispatch proceeds; a store without the capability is skipped.
func TestRecordRunSizingWriteFailureIsBestEffort(t *testing.T) {
	st := &sizingStore{err: errors.New("db down")}
	s := &Server{cfg: Config{Store: st, RunnerTarget: "docker"}}
	s.recordRunSizing(context.Background(), uuid.New(), runner.Resources{}, false)
	if st.n != 1 {
		t.Errorf("SetRunSizing calls = %d, want 1", st.n)
	}
	s = &Server{cfg: Config{Store: struct{ store.Store }{}}}
	s.recordRunSizing(context.Background(), uuid.New(), runner.Resources{}, false)
}

// Only a run bound to a placement of this deployment has its sizing recorded: not one on a registered
// runner (stamped or not), not one on an executor the deployment does not run, and a multi-executor
// deployment's executor is matched by name, never against the aggregate driver's own name.
func TestRunOnDeployment(t *testing.T) {
	rid := uuid.New()
	s := &Server{cfg: Config{Runner: multiExecutorRunner{names: []string{"docker", "k8s"}}}}
	for _, tc := range []struct {
		name string
		run  types.AgentRun
		want bool
	}{
		{"unplaced run", types.AgentRun{}, true},
		{"deployment executor", types.AgentRun{Placement: types.PlacementRemote, RunnerPoolExecutor: "k8s"}, true},
		{"registered runner", types.AgentRun{Placement: types.PlacementLocal, RunnerID: &rid}, false},
		{"registered runner since erased", types.AgentRun{Placement: types.PlacementLocal}, false},
		{"executor of another deployment", types.AgentRun{Placement: types.PlacementRemote, RunnerPoolExecutor: "elsewhere"}, false},
	} {
		if got := s.runOnDeployment(tc.run); got != tc.want {
			t.Errorf("%s: runOnDeployment = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// multiExecutorRunner is an aggregate driver: it names itself "orchestrator" and lists its executors.
type multiExecutorRunner struct {
	runner.Runner
	names []string
}

func (multiExecutorRunner) Name() string          { return "orchestrator" }
func (m multiExecutorRunner) Executors() []string { return m.names }

// A run with an Azure route gate records the 400 MiB proxy envelope the drivers apply, not the
// 256 MiB default; any other run keeps the default, and a configured value above 400 wins.
func TestRunSizingAzureProxyEnvelope(t *testing.T) {
	t.Cleanup(func() { runner.SetProxyLimits(0, 0) })
	runner.SetProxyLimits(500, 256)
	if z := runSizing("docker", runner.Resources{}, true); z.ProxyMemoryMiB != runner.AzureProxyMemoryMiB {
		t.Errorf("azure run proxy memory = %d, want %d", z.ProxyMemoryMiB, runner.AzureProxyMemoryMiB)
	}
	if z := runSizing("docker", runner.Resources{}, false); z.ProxyMemoryMiB != 256 {
		t.Errorf("non-azure run proxy memory = %d, want 256", z.ProxyMemoryMiB)
	}
	runner.SetProxyLimits(500, 600)
	if z := runSizing("k8s", runner.Resources{}, true); z.ProxyMemoryMiB != 600 {
		t.Errorf("azure run with a configured 600 = %d, want 600", z.ProxyMemoryMiB)
	}
}
