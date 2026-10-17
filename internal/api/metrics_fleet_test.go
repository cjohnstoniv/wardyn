// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

type fleetCapStore struct {
	pingStore
	calls int
	opts  []store.RunCapacityOpts
	cap   store.RunCapacity
	err   error
}

func (c *fleetCapStore) RunCapacity(_ context.Context, o store.RunCapacityOpts) (store.RunCapacity, error) {
	c.calls++
	c.opts = append(c.opts, o)
	return c.cap, c.err
}

func scrapeBody(t *testing.T, srv *Server) string {
	t.Helper()
	w := do(t, srv, http.MethodGet, "/metrics", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("/metrics = %d", w.Code)
	}
	return w.Body.String()
}

func TestMetricsFleetGauges(t *testing.T) {
	cs := &fleetCapStore{cap: store.RunCapacity{
		States:             map[string]int{"RUNNING": 3, "STARTING": 2},
		UnschedulableTotal: 1,
		ByRunner: map[string]store.RunCapacityRunner{
			"k8s":    {Basis: "requests", RunCapacitySums: store.RunCapacitySums{HeldCPUMillis: 1500, HeldMemoryMiB: 3072}},
			"docker": {Basis: "caps", RunCapacitySums: store.RunCapacitySums{Holding: 1, Unknown: 1}},
		},
		OldestActiveSeconds: 4200,
	}}
	srv := New(baseTestConfig(newHarness(t), cs))
	body := scrapeBody(t, srv)
	for _, want := range []string{
		`wardyn_runs_active{state="RUNNING"} 3`,
		`wardyn_runs_active{state="STARTING"} 2`,
		"wardyn_runs_unschedulable 1",
		`wardyn_runs_cpu_millis_held{runner="k8s"} 1500`,
		`wardyn_runs_cpu_millis_held{runner="docker"} 0`,
		`wardyn_runs_memory_mib_held{runner="k8s"} 3072`,
		`wardyn_runs_memory_mib_held{runner="docker"} 0`,
		"wardyn_runs_oldest_active_seconds 4200",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q:\n%s", want, body)
		}
	}
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "wardyn_runs_") && strings.Contains(line, "owner") {
			t.Errorf("owner label on %q", line)
		}
	}
	scrapeBody(t, srv)
	if cs.calls != 1 {
		t.Errorf("two scrapes issued %d aggregate queries, want 1", cs.calls)
	}
}

func TestMetricsFleetFailureOmitsFamilies(t *testing.T) {
	cs := &fleetCapStore{err: errors.New("boom")}
	cs.pingStore.err = errors.New("dial tcp: refused")
	body := scrapeBody(t, New(baseTestConfig(newHarness(t), cs)))
	if !strings.Contains(body, "wardyn_store_up 0") {
		t.Errorf("want wardyn_store_up 0:\n%s", body)
	}
	if strings.Contains(body, "wardyn_runs_active") || strings.Contains(body, "wardyn_runs_unschedulable") || strings.Contains(body, "_held") || strings.Contains(body, "oldest_active") {
		t.Errorf("capacity families present on a failed refresh:\n%s", body)
	}
}

// The capacity handler and the metrics snapshot both tell the store which executors are this deployment's
// own, so a run bound outside it is not counted in either.
func TestRunCapacityReadsPassTheDeploymentExecutors(t *testing.T) {
	cs := &fleetCapStore{}
	cfg := baseTestConfig(newHarness(t), cs)
	cfg.Runner = multiExecutorRunner{names: []string{"docker", "k8s"}}
	srv := New(cfg)
	if w := do(t, srv, http.MethodGet, "/api/v1/admin/runs/capacity", adminToken, ""); w.Code != http.StatusOK {
		t.Fatalf("capacity = %d %s", w.Code, w.Body.String())
	}
	scrapeBody(t, srv)
	if len(cs.opts) != 2 {
		t.Fatalf("store reads = %d, want the handler's and the snapshot's", len(cs.opts))
	}
	for i, o := range cs.opts {
		if o.LocalExecutor == nil || !o.LocalExecutor("k8s") || o.LocalExecutor("elsewhere") {
			t.Errorf("read %d: LocalExecutor not wired to the deployment's executors", i)
		}
	}
}
