// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package sinks

import (
	"encoding/json"
	"reflect"
	"testing"
)

// A metrics reply's partialSuccess count is read from rejectedDataPoints, and
// that field is not read for logs or spans.
func TestRejectedRecords_MetricsField(t *testing.T) {
	reply := []byte(`{"partialSuccess":{"rejectedDataPoints":"3","errorMessage":"x"}}`)
	if n := rejectedRecords(reply, signalMetrics); n != 3 {
		t.Errorf("metrics rejected = %d, want 3", n)
	}
	if n := rejectedRecords(reply, signalLogs) + rejectedRecords(reply, signalTraces); n != 0 {
		t.Errorf("logs and spans read %d from a metrics reply, want 0", n)
	}
}

// Shapes no writer emits today are still read correctly: a summary's quantile
// samples become quantile values, a histogram with no _count takes its total
// from the +Inf bucket, a quantile label on a gauge stays an attribute, and a
// timestamped line is skipped.
func TestEncodeOTLPMetrics_QuantilesAndCountlessHistogram(t *testing.T) {
	text := `# TYPE s summary
s{op="a",quantile="0.5"} 1.5
s{op="a",quantile="0.9"} 4
s_sum{op="a"} 20
s_count{op="a"} 8
# TYPE h histogram
h_bucket{le="1"} 2
h_bucket{le="+Inf"} 5
h_sum 9
# TYPE g gauge
g{quantile="x"} 1
# TYPE t gauge
t 1 1700000000000
`
	body, n, err := encodeOTLPMetrics([]byte(text), "2")
	if err != nil || n != 3 {
		t.Fatalf("encodeOTLPMetrics = %d metrics, %v; want 3 (the timestamped gauge skipped)", n, err)
	}
	var req otlpMetricsRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	ms := map[string]otlpMetric{}
	for _, m := range req.ResourceMetrics[0].ScopeMetrics[0].Metrics {
		ms[m.Name] = m
	}
	s := ms["s"].Summary
	if s == nil || len(s.DataPoints) != 1 {
		t.Fatalf("summary = %+v, want one point for op=a", s)
	}
	sp := s.DataPoints[0]
	if sp.Count != "8" || sp.Sum != 20 || len(sp.Attributes) != 1 ||
		!reflect.DeepEqual(sp.QuantileValues, []otlpQuantile{{0.5, 1.5}, {0.9, 4}}) {
		t.Errorf("summary point = %+v, want count 8, sum 20, op only, quantiles 0.5=1.5 and 0.9=4", sp)
	}
	h := ms["h"].Histogram
	if h == nil || len(h.DataPoints) != 1 || h.DataPoints[0].Count != "5" ||
		!reflect.DeepEqual(h.DataPoints[0].BucketCounts, []string{"2", "3"}) {
		t.Errorf("histogram = %+v, want count 5 from +Inf and buckets [2 3]", h)
	}
	if g := ms["g"].Gauge; g == nil || len(g.DataPoints) != 1 || len(g.DataPoints[0].Attributes) != 1 ||
		g.DataPoints[0].Attributes[0].Key != "quantile" {
		t.Errorf("gauge = %+v, want its quantile label kept as an attribute", g)
	}
}
