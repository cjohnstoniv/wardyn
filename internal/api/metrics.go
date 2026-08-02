// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// metrics holds the control plane's scrape counters, served as Prometheus text
// exposition by GET /metrics (admin-gated — see routes()).
//
// Why this exists: the audit fanout (file/syslog/webhook, file on by default in
// compose) already carries run/approval/deny/mint counts to a SIEM, but nothing
// exposed them to a scrape target, and sandbox launch latency was measured
// nowhere at all.
//
// Why it is hand-rolled: the Prometheus text exposition format is a dozen
// Fprintf lines, while prometheus/client_golang would drag protobuf + ~5 more
// modules through this repo's licenses / SBOM / govulncheck / tidy gates — in a
// security product — to emit five counters. Same scrape, no supply chain.
//
// ponytail: ONE mutex for the whole set (a handful of increments per run is not
// a hot path) and no histogram — launch latency ships as a sum/count average.
// Add buckets the day someone actually needs a p99.
type metrics struct {
	mu           sync.Mutex
	runs         map[types.RunState]int64 // runs that reached a terminal state
	approvals    map[string]int64         // "approved" / "denied"
	egressDenies int64
	mints        int64
	launchSum    float64 // seconds, run creation -> RUNNING
	launchCount  int64
}

func (m *metrics) runTerminal(st types.RunState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.runs == nil {
		m.runs = map[types.RunState]int64{}
	}
	m.runs[st]++
}

func (m *metrics) approvalDecided(approve bool) {
	decision := "denied"
	if approve {
		decision = "approved"
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.approvals == nil {
		m.approvals = map[string]int64{}
	}
	m.approvals[decision]++
}

func (m *metrics) egressDenied() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.egressDenies++
}

func (m *metrics) credentialMinted() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mints++
}

func (m *metrics) sandboxLaunched(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.launchSum += d.Seconds()
	m.launchCount++
}

// write emits the counters in Prometheus text exposition format. Label VALUES
// are Wardyn enums (run states, approved/denied) — never operator input — so no
// escaping is needed here.
func (m *metrics) write(w io.Writer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	fmt.Fprint(w, "# HELP wardyn_runs_total Runs that reached a terminal state, by state.\n"+
		"# TYPE wardyn_runs_total counter\n")
	for st, n := range m.runs {
		fmt.Fprintf(w, "wardyn_runs_total{state=%q} %d\n", string(st), n)
	}
	fmt.Fprint(w, "# HELP wardyn_approval_decisions_total Approval decisions, by outcome.\n"+
		"# TYPE wardyn_approval_decisions_total counter\n")
	for decision, n := range m.approvals {
		fmt.Fprintf(w, "wardyn_approval_decisions_total{decision=%q} %d\n", decision, n)
	}
	fmt.Fprintf(w, "# HELP wardyn_egress_denies_total Egress requests denied by policy (proxy decision ingest).\n"+
		"# TYPE wardyn_egress_denies_total counter\nwardyn_egress_denies_total %d\n", m.egressDenies)
	fmt.Fprintf(w, "# HELP wardyn_credential_mints_total Credentials minted by the broker.\n"+
		"# TYPE wardyn_credential_mints_total counter\nwardyn_credential_mints_total %d\n", m.mints)
	// A summary with no quantiles: sum/count only, i.e. an average launch time.
	fmt.Fprintf(w, "# HELP wardyn_sandbox_launch_seconds Time from run creation to RUNNING.\n"+
		"# TYPE wardyn_sandbox_launch_seconds summary\n"+
		"wardyn_sandbox_launch_seconds_sum %g\nwardyn_sandbox_launch_seconds_count %d\n",
		m.launchSum, m.launchCount)
}

// handleMetrics serves the scrape surface. It is mounted INSIDE the
// humanOrAdminAuth group on purpose: wardynd fails the public API closed when no
// admin token is configured (cmd/wardynd) and refuses capability disclosure on
// the anonymous /healthz, so an open /metrics would contradict that posture. A
// Prometheus scrape_config authenticates with two lines of `authorization:`.
func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	s.metrics.write(w)
}
