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
		z := runSizing(tc.kind, tc.res)
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
	s.recordRunSizing(context.Background(), uuid.New(), runner.Resources{})
	if st.n != 1 {
		t.Errorf("SetRunSizing calls = %d, want 1", st.n)
	}
	s = &Server{cfg: Config{Store: struct{ store.Store }{}}}
	s.recordRunSizing(context.Background(), uuid.New(), runner.Resources{})
}
