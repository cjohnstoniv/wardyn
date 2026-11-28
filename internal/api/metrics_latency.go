// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// histogramSpec names one single-label latency histogram on /metrics and its buckets (upper bounds, in seconds).
// values is the closed label set written on every scrape, zeros included; nil writes only the values seen.
type histogramSpec struct {
	name, help, label string
	buckets           []float64
	values            []string
}

// The start's phases, in order (metrics.runStarted).
const (
	startPhaseBeforeDispatch = "before_dispatch"
	startPhasePrepare        = "prepare"
	startPhaseCreate         = "create"
	startPhaseConfirm        = "confirm"
	startPhaseTotal          = "total"
)

var (
	routeSeconds = histogramSpec{
		name: "wardyn_http_request_seconds",
		help: "Time wardynd took to answer a request, by the route pattern it matched (unmatched when none did). " +
			"An attach or stream counts its whole session.",
		label:   "route",
		buckets: []float64{0.005, 0.025, 0.1, 0.5, 2.5, 10},
	}
	startPhaseSeconds = histogramSpec{
		name: "wardyn_run_start_phase_seconds",
		help: "Time a run that reached RUNNING spent in each phase of its start: before_dispatch (creation to dispatch, " +
			"any pool queue wait included), prepare (dispatch to the sandbox create), create (the runner's create), " +
			"confirm (the proxy's confirmation and the move to RUNNING) and total (creation to RUNNING).",
		label:   "phase",
		buckets: []float64{0.25, 0.5, 1, 2, 4, 8, 15, 30, 60, 300},
		values:  []string{startPhaseBeforeDispatch, startPhasePrepare, startPhaseCreate, startPhaseConfirm, startPhaseTotal},
	}
	decisionRecordSeconds = histogramSpec{
		name: "wardyn_egress_decision_record_seconds",
		help: "Time from a run's proxy taking a request to wardynd recording its egress decision, by decision, for " +
			"proxies that post to wardynd directly (a runner's spooled rows are not counted). A held request's wait " +
			"for its approval is included. Clocks that disagree read as 0.",
		label:   "decision",
		buckets: []float64{0.01, 0.05, 0.25, 1, 5, 30, 120},
		values:  []string{string(egress.Allow), string(egress.Deny), string(egress.Pending)},
	}
)

// latencyHist is one histogramSpec's counts. The zero value is ready. It has its own lock, not metrics.mu: the
// route series is recorded on every request.
type latencyHist struct {
	mu     sync.Mutex
	counts map[string][]int64 // per label value, one slot per bucket
	sum    map[string]float64
	n      map[string]int64
}

// latencyHists are the three histograms on metrics.
type latencyHists struct{ route, startPhase, decisionRecord latencyHist }

func (h *latencyHist) observe(spec *histogramSpec, value string, d time.Duration) {
	sec := max(d.Seconds(), 0)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.counts == nil {
		h.counts, h.sum, h.n = map[string][]int64{}, map[string]float64{}, map[string]int64{}
	}
	c := h.counts[value]
	if c == nil {
		c = make([]int64, len(spec.buckets))
		h.counts[value] = c
	}
	for i, le := range spec.buckets {
		if sec <= le {
			c[i]++
		}
	}
	h.sum[value] += sec
	h.n[value]++
}

// write appends the histogram. Label values are route patterns from this package's router and the closed sets
// above, never request input.
func (h *latencyHist) write(w io.Writer, spec *histogramSpec) {
	h.mu.Lock()
	defer h.mu.Unlock()
	fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s histogram\n", spec.name, spec.help, spec.name)
	values := spec.values
	if values == nil {
		values = slices.Sorted(maps.Keys(h.n))
	}
	for _, v := range values {
		for i, le := range spec.buckets {
			var c int64
			if h.counts[v] != nil {
				c = h.counts[v][i]
			}
			fmt.Fprintf(w, "%s_bucket{%s=%q,le=\"%g\"} %d\n", spec.name, spec.label, v, le, c)
		}
		fmt.Fprintf(w, "%s_bucket{%s=%q,le=\"+Inf\"} %d\n", spec.name, spec.label, v, h.n[v])
		fmt.Fprintf(w, "%s_sum{%s=%q} %g\n", spec.name, spec.label, v, h.sum[v])
		fmt.Fprintf(w, "%s_count{%s=%q} %d\n", spec.name, spec.label, v, h.n[v])
	}
}

// runStarted records a run's start phases when it reaches RUNNING, from the times dispatch stamped.
func (m *metrics) runStarted(created, dispatched, createStart, createEnd, running time.Time) {
	h, s := &m.latency.startPhase, &startPhaseSeconds
	h.observe(s, startPhaseBeforeDispatch, dispatched.Sub(created))
	h.observe(s, startPhasePrepare, createStart.Sub(dispatched))
	h.observe(s, startPhaseCreate, createEnd.Sub(createStart))
	h.observe(s, startPhaseConfirm, running.Sub(createEnd))
	h.observe(s, startPhaseTotal, running.Sub(created))
}

// decisionRecorded records one egress decision row's time from the proxy's request stamp to its record. A row with
// no stamp, or a decision outside the closed set, is not counted.
func (m *metrics) decisionRecorded(d egress.Decision, taken, recorded time.Time) {
	if taken.IsZero() || !slices.Contains(decisionRecordSeconds.values, string(d)) {
		return
	}
	m.latency.decisionRecord.observe(&decisionRecordSeconds, string(d), recorded.Sub(taken))
}

// writeLatency appends the three histograms.
func (m *metrics) writeLatency(w io.Writer) {
	m.latency.route.write(w, &routeSeconds)
	m.latency.startPhase.write(w, &startPhaseSeconds)
	m.latency.decisionRecord.write(w, &decisionRecordSeconds)
}

// timeRoute records each request's time in wardyn_http_request_seconds under the route pattern chi matched, read
// once the handler has returned (the pattern is complete only then). A path no route matched is "unmatched", so a
// scan of random paths adds one series, not one per path.
func (s *Server) timeRoute(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		route := "unmatched"
		if rctx := chi.RouteContext(r.Context()); rctx != nil && rctx.RoutePattern() != "" {
			route = rctx.RoutePattern()
		}
		s.metrics.latency.route.observe(&routeSeconds, route, time.Since(start))
	})
}
