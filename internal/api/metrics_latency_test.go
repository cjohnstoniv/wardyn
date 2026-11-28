// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// TestLatencyHistWritesCumulativeBuckets: buckets count every value at or under their bound, a negative duration
// (clocks that disagree) counts as 0, and a closed label set is written with zeros before anything is seen.
func TestLatencyHistWritesCumulativeBuckets(t *testing.T) {
	spec := histogramSpec{name: "x_seconds", help: "h", label: "k", buckets: []float64{1, 5}, values: []string{"a", "b"}}
	var h latencyHist
	h.observe(&spec, "a", 500*time.Millisecond)
	h.observe(&spec, "a", 3*time.Second)
	h.observe(&spec, "a", -time.Second)
	var buf bytes.Buffer
	h.write(&buf, &spec)
	want := `# HELP x_seconds h
# TYPE x_seconds histogram
x_seconds_bucket{k="a",le="1"} 2
x_seconds_bucket{k="a",le="5"} 3
x_seconds_bucket{k="a",le="+Inf"} 3
x_seconds_sum{k="a"} 3.5
x_seconds_count{k="a"} 3
x_seconds_bucket{k="b",le="1"} 0
x_seconds_bucket{k="b",le="5"} 0
x_seconds_bucket{k="b",le="+Inf"} 0
x_seconds_sum{k="b"} 0
x_seconds_count{k="b"} 0
`
	if got := buf.String(); got != want {
		t.Fatalf("exposition:\n%s\nwant:\n%s", got, want)
	}
}

// TestRunStartedRecordsEachPhase: the four phases split the start at dispatch's stamps and total spans all of it.
func TestRunStartedRecordsEachPhase(t *testing.T) {
	var m metrics
	t0 := time.Unix(1_700_000_000, 0)
	m.runStarted(t0, t0.Add(300*time.Millisecond), t0.Add(700*time.Millisecond), t0.Add(3*time.Second), t0.Add(3500*time.Millisecond))
	var buf bytes.Buffer
	m.writeLatency(&buf)
	for _, want := range []string{
		`wardyn_run_start_phase_seconds_bucket{phase="before_dispatch",le="0.5"} 1`,
		`wardyn_run_start_phase_seconds_bucket{phase="prepare",le="0.25"} 0`,
		`wardyn_run_start_phase_seconds_bucket{phase="prepare",le="0.5"} 1`,
		`wardyn_run_start_phase_seconds_bucket{phase="create",le="2"} 0`,
		`wardyn_run_start_phase_seconds_bucket{phase="create",le="4"} 1`,
		`wardyn_run_start_phase_seconds_bucket{phase="confirm",le="0.5"} 1`,
		`wardyn_run_start_phase_seconds_sum{phase="total"} 3.5`,
		`wardyn_run_start_phase_seconds_count{phase="total"} 1`,
	} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("missing %q in:\n%s", want, buf.String())
		}
	}
}

// TestMetricsLatencyHistograms: a decision posted 1.5 s after its proxy took the request lands in the decision
// histogram's 5-second bucket and not its 1-second one, the post is counted under its route pattern, a path no
// route matches is counted as unmatched, and the start phases are on /metrics before any run has started.
func TestMetricsLatencyHistograms(t *testing.T) {
	h := newHarness(t)
	body, _ := json.Marshal(egress.DecisionLog{
		Request:  egress.Request{Host: "allowed.test", Port: 443, Method: http.MethodConnect, Time: time.Now().Add(-1500 * time.Millisecond)},
		Decision: egress.Allow, RuleSource: "policy:allowed",
	})
	if w := do(t, h.srv, http.MethodPost, "/api/v1/internal/decisions", h.mintRunToken(t, uuid.New()), string(body)); w.Code >= 300 {
		t.Fatalf("post decision: %d %s", w.Code, w.Body.String())
	}
	_ = do(t, h.srv, http.MethodGet, "/no/such/path", "", "")
	w := do(t, h.srv, http.MethodGet, "/metrics", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("/metrics = %d, want 200", w.Code)
	}
	for _, want := range []string{
		"# TYPE wardyn_egress_decision_record_seconds histogram",
		`wardyn_egress_decision_record_seconds_bucket{decision="allow",le="1"} 0`,
		`wardyn_egress_decision_record_seconds_bucket{decision="allow",le="5"} 1`,
		`wardyn_egress_decision_record_seconds_count{decision="deny"} 0`,
		"# TYPE wardyn_http_request_seconds histogram",
		`wardyn_http_request_seconds_count{route="/api/v1/internal/decisions"} 1`,
		`wardyn_http_request_seconds_count{route="unmatched"} 1`,
		"# TYPE wardyn_run_start_phase_seconds histogram",
		`wardyn_run_start_phase_seconds_count{phase="total"} 0`,
	} {
		if !strings.Contains(w.Body.String(), want) {
			t.Errorf("/metrics missing %q", want)
		}
	}
}
