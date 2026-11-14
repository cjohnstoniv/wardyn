// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package sinks_test

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/audit/sinks"
)

// exposition holds each type the /metrics body writes, a summary and a histogram
// with two label sets each, a counter read from state that outlives a restart,
// and a NaN gauge and an unreadable line, which are both skipped.
const exposition = `# HELP wardyn_runs_total Runs that reached a terminal state, by state.
# TYPE wardyn_runs_total counter
wardyn_runs_total{state="succeeded"} 3
# TYPE wardyn_audit_spool_quarantined_total counter
wardyn_audit_spool_quarantined_total 2
# HELP wardyn_store_up 1 when Postgres answers.
# TYPE wardyn_store_up gauge
wardyn_store_up 1
# TYPE wardyn_nan_gauge gauge
wardyn_nan_gauge NaN
# HELP wardyn_run_start_wait_seconds Time spent waiting, by reason.
# TYPE wardyn_run_start_wait_seconds summary
wardyn_run_start_wait_seconds_sum{reason="image_pull"} 12.5
wardyn_run_start_wait_seconds_count{reason="image_pull"} 2
wardyn_run_start_wait_seconds_sum{reason="other"} 7
wardyn_run_start_wait_seconds_count{reason="other"} 5
# HELP wardyn_runner_pool_queue_wait_seconds How long runs waited.
# TYPE wardyn_runner_pool_queue_wait_seconds histogram
wardyn_runner_pool_queue_wait_seconds_bucket{outcome="started",le="1"} 1
wardyn_runner_pool_queue_wait_seconds_bucket{outcome="started",le="5"} 3
wardyn_runner_pool_queue_wait_seconds_bucket{outcome="started",le="+Inf"} 4
wardyn_runner_pool_queue_wait_seconds_sum{outcome="started"} 40
wardyn_runner_pool_queue_wait_seconds_count{outcome="started"} 4
wardyn_runner_pool_queue_wait_seconds_bucket{outcome="timeout",le="1"} 0
wardyn_runner_pool_queue_wait_seconds_bucket{outcome="timeout",le="5"} 0
wardyn_runner_pool_queue_wait_seconds_bucket{outcome="timeout",le="+Inf"} 2
wardyn_runner_pool_queue_wait_seconds_sum{outcome="timeout"} 600
wardyn_runner_pool_queue_wait_seconds_count{outcome="timeout"} 2
not a sample {
`

// metricPoint is one decoded data point; the fields a type does not carry stay zero.
type metricPoint struct {
	Attributes []struct {
		Key   string
		Value struct{ StringValue string }
	}
	StartTimeUnixNano string
	TimeUnixNano      string
	AsDouble          float64
	Count             string
	Sum               float64
	BucketCounts      []string
	ExplicitBounds    []float64
}

// attr is the point's string attribute key, "" when absent.
func (p metricPoint) attr(key string) string {
	for _, a := range p.Attributes {
		if a.Key == key {
			return a.Value.StringValue
		}
	}
	return ""
}

type metricSeries struct {
	AggregationTemporality int
	IsMonotonic            bool
	DataPoints             []metricPoint
}

type decodedMetric struct {
	Name, Description              string
	Sum, Gauge, Summary, Histogram *metricSeries
}

type decodedPush struct {
	ResourceMetrics []struct {
		Resource struct {
			Attributes []struct {
				Key   string
				Value struct{ StringValue string }
			}
		}
		ScopeMetrics []struct{ Metrics []decodedMetric }
	}
}

// newMetrics builds an exporter for an otlp_metrics block on rcv with extra JSON fields appended.
func newMetrics(t *testing.T, rcv *otlpReceiver, extra string) *sinks.OTLPMetrics {
	t.Helper()
	m, err := sinks.NewOTLPMetrics([]byte(`{"otlp_metrics":{"endpoint":"` + rcv.srv.URL + `"` + extra + `}}`))
	if err != nil || m == nil {
		t.Fatalf("NewOTLPMetrics = %v, %v; want an exporter", m, err)
	}
	return m
}

// pushOnce sends one export of text to rcv and returns the request, its
// resource's service.instance.id and its metrics by name.
func pushOnce(t *testing.T, rcv *otlpReceiver, extra, text string) (otlpRequest, string, map[string]decodedMetric) {
	t.Helper()
	m := newMetrics(t, rcv, extra)
	sinks.PushOTLPMetrics(context.Background(), m, func(context.Context) []byte { return []byte(text) })
	if rcv.count() != 1 {
		t.Fatalf("requests = %d, want 1", rcv.count())
	}
	req := rcv.get(0)
	var body decodedPush
	if err := json.Unmarshal(req.body, &body); err != nil {
		t.Fatalf("push is not JSON: %v", err)
	}
	instance := ""
	for _, a := range body.ResourceMetrics[0].Resource.Attributes {
		if a.Key == "service.instance.id" {
			instance = a.Value.StringValue
		}
	}
	byName := map[string]decodedMetric{}
	for _, dm := range body.ResourceMetrics[0].ScopeMetrics[0].Metrics {
		byName[dm.Name] = dm
	}
	return req, instance, byName
}

// byAttr indexes points by one attribute's value.
func byAttr(t *testing.T, s *metricSeries, key string) map[string]metricPoint {
	t.Helper()
	if s == nil {
		t.Fatal("series missing")
	}
	out := map[string]metricPoint{}
	for _, p := range s.DataPoints {
		out[p.attr(key)] = p
	}
	return out
}

// The push goes to /v1/metrics with the block's headers and carries each family
// under its own name, as the OTLP type its # TYPE line names, one point per
// label set, with labels as attributes.
func TestOTLPMetrics_PushesTheExpositionToV1Metrics(t *testing.T) {
	rcv := newOTLPReceiver(t, nil)
	req, _, got := pushOnce(t, rcv, `,"headers":{"X-Scope-OrgID":"tenant-a"}`, exposition)
	if req.path != "/v1/metrics" || req.header.Get("Content-Type") != "application/json" {
		t.Fatalf("push went to %q as %q, want /v1/metrics as application/json", req.path, req.header.Get("Content-Type"))
	}
	if h := req.header.Get("X-Scope-OrgID"); h != "tenant-a" {
		t.Errorf("X-Scope-OrgID = %q, want the configured header", h)
	}
	if len(got) != 5 {
		t.Errorf("metrics = %d, want 5 (the NaN gauge and the unreadable line skipped)", len(got))
	}

	runs := got["wardyn_runs_total"]
	if runs.Description != "Runs that reached a terminal state, by state." {
		t.Errorf("counter description = %q, want its HELP text", runs.Description)
	}
	if runs.Sum == nil || runs.Sum.AggregationTemporality != 2 || !runs.Sum.IsMonotonic || len(runs.Sum.DataPoints) != 1 {
		t.Fatalf("counter = %+v, want one cumulative monotonic sum point", runs.Sum)
	}
	p := runs.Sum.DataPoints[0]
	if p.AsDouble != 3 || p.attr("state") != "succeeded" {
		t.Errorf("counter point = %+v, want 3 with state=succeeded", p)
	}
	start, errS := strconv.ParseInt(p.StartTimeUnixNano, 10, 64)
	now, errT := strconv.ParseInt(p.TimeUnixNano, 10, 64)
	if errS != nil || errT != nil || start <= 0 || start >= now {
		t.Errorf("counter start %q, time %q: want a start before the point's time", p.StartTimeUnixNano, p.TimeUnixNano)
	}
	if q := got["wardyn_audit_spool_quarantined_total"].Sum; q == nil || len(q.DataPoints) != 1 || q.DataPoints[0].StartTimeUnixNano != "" {
		t.Errorf("carried counter = %+v, want one point with no start time", q)
	}

	if g := got["wardyn_store_up"].Gauge; g == nil || len(g.DataPoints) != 1 || g.DataPoints[0].AsDouble != 1 || g.DataPoints[0].StartTimeUnixNano != "" {
		t.Errorf("gauge = %+v, want asDouble 1 and no start time", g)
	}

	waits := byAttr(t, got["wardyn_run_start_wait_seconds"].Summary, "reason")
	if len(waits) != 2 || waits["image_pull"].Count != "2" || waits["image_pull"].Sum != 12.5 ||
		waits["other"].Count != "5" || waits["other"].Sum != 7 {
		t.Errorf("summary points = %+v, want image_pull 2/12.5 and other 5/7", waits)
	}

	h := got["wardyn_runner_pool_queue_wait_seconds"].Histogram
	if h == nil || h.AggregationTemporality != 2 {
		t.Fatalf("histogram = %+v, want cumulative", h)
	}
	queue := byAttr(t, h, "outcome")
	want := map[string]struct {
		count, buckets string
		sum            float64
	}{"started": {"4", `["1","2","1"]`, 40}, "timeout": {"2", `["0","0","2"]`, 600}}
	for outcome, w := range want {
		pt, ok := queue[outcome]
		b, _ := json.Marshal(pt.BucketCounts)
		if !ok || pt.Count != w.count || pt.Sum != w.sum || string(b) != w.buckets || len(pt.ExplicitBounds) != 2 ||
			pt.ExplicitBounds[0] != 1 || pt.ExplicitBounds[1] != 5 || pt.attr("le") != "" || pt.StartTimeUnixNano == "" {
			t.Errorf("histogram %s point = %+v, want count %s, sum %g, buckets %s, bounds [1 5], no le", outcome, pt, w.count, w.sum, w.buckets)
		}
	}
}

// The push is off unless an otlp_metrics block asks for it; the otlp block
// neither turns it on nor takes a metrics field; a bad block refuses boot
// through ParseSinks as well as here.
func TestOTLPMetrics_OwnBlockAndChecks(t *testing.T) {
	for _, cfg := range []string{"", "  ", `{"syslog":{"network":"udp","addr":"localhost:514"}}`, `{"otlp":{"endpoint":"http://o","traces":true}}`} {
		if m, err := sinks.NewOTLPMetrics([]byte(cfg)); m != nil || err != nil {
			t.Errorf("NewOTLPMetrics(%q) = %v, %v; want nil, nil", cfg, m, err)
		}
	}
	if _, err := sinks.ParseSinks([]byte(`{"otlp":{"endpoint":"http://o","metrics":true}}`)); err == nil {
		t.Error(`the otlp block accepted "metrics"; it must stay as it is`)
	}
	// Metrics alone build no audit sink, so no audit row goes to the metrics collector.
	if s, err := sinks.ParseSinks([]byte(`{"otlp_metrics":{"endpoint":"http://o"}}`)); err != nil || len(s) != 0 {
		t.Errorf("ParseSinks(otlp_metrics only) = %d sinks, %v; want 0, nil", len(s), err)
	}
	for _, block := range []string{
		`{"endpoint":"http://o","interval":"500ms"}`,
		`{"endpoint":"http://o","interval":"0s"}`,
		`{"endpoint":"http://o","interval":"soon"}`,
		`{"interval":"60s"}`,
		`{"endpoint":"http://o","bearer_token":"t"}`,
		`{"endpoint":"http://o","timeout":"1m"}`,
		`{"endpoint":"http://o","batch_size":10}`,
	} {
		cfg := `{"otlp_metrics":` + block + `}`
		if _, err := sinks.NewOTLPMetrics([]byte(cfg)); err == nil {
			t.Errorf("NewOTLPMetrics accepted %s", block)
		}
		if _, err := sinks.ParseSinks([]byte(cfg)); err == nil {
			t.Errorf("ParseSinks booted with %s", block)
		}
	}
	if m, err := sinks.NewOTLPMetrics([]byte(`{"otlp_metrics":{"endpoint":"http://o","interval":"1s"}}`)); m == nil || err != nil {
		t.Errorf("interval 1s = %v, %v; want an exporter (the floor is inclusive)", m, err)
	}
}

// Each replica pushes under its own service.instance.id: the host name, or
// "<WARDYN_AUDIT_SOURCE>/<host name>" when a source is set, since one source is
// often shared by every replica. Not t.Parallel(): sinks.Source is process-global.
func TestOTLPMetrics_ServiceInstanceID(t *testing.T) {
	orig := sinks.Source
	t.Cleanup(func() { sinks.Source = orig })
	host, err := os.Hostname()
	if err != nil || host == "" {
		t.Skip("no host name on this machine")
	}
	for source, want := range map[string]string{"": host, "prod-us-east": "prod-us-east/" + host} {
		sinks.Source = source
		rcv := newOTLPReceiver(t, nil)
		if _, got, _ := pushOnce(t, rcv, "", exposition); got != want {
			t.Errorf("Source %q: service.instance.id = %q, want %q", source, got, want)
		}
	}
}

// A push the collector refuses is counted; an accepted one is not.
func TestOTLPMetrics_FailuresCounted(t *testing.T) {
	collect := func(context.Context) []byte { return []byte(exposition) }
	refused := newOTLPReceiver(t, func(int) int { return http.StatusServiceUnavailable })
	m := newMetrics(t, refused, "")
	sinks.PushOTLPMetrics(context.Background(), m, collect)
	sinks.PushOTLPMetrics(context.Background(), m, collect)
	if n := m.Failures(); n != 2 {
		t.Errorf("Failures after two refused pushes = %d, want 2", n)
	}
	m = newMetrics(t, newOTLPReceiver(t, nil), "")
	sinks.PushOTLPMetrics(context.Background(), m, collect)
	if n := m.Failures(); n != 0 {
		t.Errorf("Failures after an accepted push = %d, want 0", n)
	}
}

// Run pushes on every tick and returns when its context ends.
func TestOTLPMetrics_RunPushesEachTickUntilCancelled(t *testing.T) {
	rcv := newOTLPReceiver(t, nil)
	m := newMetrics(t, rcv, "")
	sinks.SetOTLPMetricsInterval(m, 10*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Run(ctx, func(context.Context) []byte { return []byte(exposition) }); close(done) }()
	waitThenSettle(t, 0, func() bool { return rcv.count() >= 2 })
	cancel()
	<-done
	if rcv.count() < 2 {
		t.Errorf("pushes = %d, want at least 2", rcv.count())
	}
}
