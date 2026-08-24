// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// storePingTimeout bounds the Postgres reachability check shared by the /readyz
// readiness probe (handleReadyz) and the wardyn_store_up gauge below, so the two
// cannot drift into disagreeing about whether the store is up.
const storePingTimeout = 3 * time.Second

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
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	s.metrics.write(w)
	s.writeHealthGauges(r, w)
}

// writeHealthGauges appends the two LIVE gauges — sampled at scrape time rather
// than counted as events happen, because both describe a standing condition, not
// a rate.
//
// They exist because a store outage makes every counter above simply stop
// moving, which a scrape cannot tell apart from a quiet cluster: wardyn_store_up
// is the same Postgres ping /readyz makes (0 = the pod is out of the Service's
// endpoints and runs are failing their durable writes), and
// wardyn_audit_spool_lines is the backlog those failed writes are spooling to
// disk. A spool that grows and never drains back to 0 is the C1 fallback doing
// its job while the drain loop is not doing its own — invisible until now
// outside a log line.
func (s *Server) writeHealthGauges(r *http.Request, w io.Writer) {
	// Nil Store (tests, and only tests) matches /readyz: nothing to be down.
	up := 1
	if s.cfg.Store != nil {
		ctx, cancel := context.WithTimeout(r.Context(), storePingTimeout)
		defer cancel()
		if err := s.cfg.Store.Ping(ctx); err != nil {
			up = 0
		}
	}
	fmt.Fprintf(w, "# HELP wardyn_store_up 1 when the control-plane Postgres answers a ping, 0 when it does not (the same check /readyz makes).\n"+
		"# TYPE wardyn_store_up gauge\nwardyn_store_up %d\n", up)
	fmt.Fprintf(w, "# HELP wardyn_audit_spool_lines Audit events in the durable fallback spool, waiting to drain back into the store.\n"+
		"# TYPE wardyn_audit_spool_lines gauge\nwardyn_audit_spool_lines %d\n", s.cfg.AuditSpool.Lines())
	fmt.Fprintf(w, "# HELP wardyn_audit_spool_torn_total Spool lines dropped as unparseable (a torn tail from an ENOSPC/partial write).\n"+
		"# TYPE wardyn_audit_spool_torn_total counter\nwardyn_audit_spool_torn_total %d\n", s.cfg.AuditSpool.TornDrops())
	s.writeSinkDrops(w)
}

// writeSinkDrops emits the per-SIEM-sink delivery-drop counter (D2): events a
// webhook/syslog sink shed past its buffer or after retry exhaustion, which the
// audit Fanout counts but nothing exposed to a scrape target. Omitted entirely
// when no sinks are configured (AuditSinkDrops nil), so a deployment without SIEM
// carries no dead series. Sink names come from the sink types (webhook/syslog/
// file) — never operator input — but %q-quoted anyway.
func (s *Server) writeSinkDrops(w io.Writer) {
	if s.cfg.AuditSinkDrops == nil {
		return
	}
	drops := s.cfg.AuditSinkDrops()
	if len(drops) == 0 {
		return
	}
	names := make([]string, 0, len(drops))
	for name := range drops {
		names = append(names, name)
	}
	sort.Strings(names) // deterministic scrape output
	fmt.Fprint(w, "# HELP wardyn_audit_sink_drops_total Audit events dropped by a SIEM sink (buffer overflow or retry exhaustion), by sink.\n"+
		"# TYPE wardyn_audit_sink_drops_total counter\n")
	for _, name := range names {
		fmt.Fprintf(w, "wardyn_audit_sink_drops_total{sink=%q} %d\n", name, drops[name])
	}
}
