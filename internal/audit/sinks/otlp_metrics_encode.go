// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package sinks

import (
	"bufio"
	"bytes"
	"encoding/json"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
)

// temporalityCumulative is OTLP's AGGREGATION_TEMPORALITY_CUMULATIVE: each value
// is a running total since the point's start time, as a Prometheus counter is.
const temporalityCumulative = 2

// processStart is when this process started (package init): the start time of
// every cumulative point the process counts itself, from zero at boot.
var processStart = strconv.FormatInt(time.Now().UnixNano(), 10)

// carriedCounters are the counters the scrape reads from state that outlives a
// wardynd restart: the eBPF sensor's counts since the sensor started, the
// acknowledged-delivery checkpoint table, and the spool quarantine count read
// from disk at boot. Their start is not known here, so their points carry no
// start time, and a backend reads them as it reads a scraped counter.
var carriedCounters = map[string]bool{
	"wardyn_groundtruth_observed_total":         true,
	"wardyn_groundtruth_dropped_total":          true,
	"wardyn_groundtruth_dropped_unmapped_total": true,
	"wardyn_groundtruth_observed_by_kind_total": true,
	"wardyn_audit_delivery_resets_total":        true,
	"wardyn_audit_spool_quarantined_total":      true,
}

// The OTLP/HTTP JSON subset of ExportMetricsServiceRequest the exporter writes.
// 64-bit counts are decimal strings, as the protocol's JSON mapping requires.

type otlpNumberPoint struct {
	Attributes        []otlpAttr `json:"attributes,omitempty"`
	StartTimeUnixNano string     `json:"startTimeUnixNano,omitempty"`
	TimeUnixNano      string     `json:"timeUnixNano"`
	AsDouble          float64    `json:"asDouble"`
}

type otlpQuantile struct {
	Quantile float64 `json:"quantile"`
	Value    float64 `json:"value"`
}

type otlpSummaryPoint struct {
	Attributes        []otlpAttr     `json:"attributes,omitempty"`
	StartTimeUnixNano string         `json:"startTimeUnixNano"`
	TimeUnixNano      string         `json:"timeUnixNano"`
	Count             string         `json:"count"`
	Sum               float64        `json:"sum"`
	QuantileValues    []otlpQuantile `json:"quantileValues,omitempty"`
}

type otlpHistogramPoint struct {
	Attributes        []otlpAttr `json:"attributes,omitempty"`
	StartTimeUnixNano string     `json:"startTimeUnixNano"`
	TimeUnixNano      string     `json:"timeUnixNano"`
	Count             string     `json:"count"`
	Sum               float64    `json:"sum"`
	BucketCounts      []string   `json:"bucketCounts"`
	ExplicitBounds    []float64  `json:"explicitBounds"`
}

type otlpSum struct {
	AggregationTemporality int               `json:"aggregationTemporality"`
	IsMonotonic            bool              `json:"isMonotonic"`
	DataPoints             []otlpNumberPoint `json:"dataPoints"`
}

type otlpGauge struct {
	DataPoints []otlpNumberPoint `json:"dataPoints"`
}

type otlpSummary struct {
	DataPoints []otlpSummaryPoint `json:"dataPoints"`
}

type otlpHistogram struct {
	AggregationTemporality int                  `json:"aggregationTemporality"`
	DataPoints             []otlpHistogramPoint `json:"dataPoints"`
}

type otlpMetric struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Sum         *otlpSum       `json:"sum,omitempty"`
	Gauge       *otlpGauge     `json:"gauge,omitempty"`
	Summary     *otlpSummary   `json:"summary,omitempty"`
	Histogram   *otlpHistogram `json:"histogram,omitempty"`
}

type otlpScopeMetrics struct {
	Scope struct {
		Name string `json:"name"`
	} `json:"scope"`
	Metrics []otlpMetric `json:"metrics"`
}

type otlpResourceMetrics struct {
	Resource struct {
		Attributes []otlpAttr `json:"attributes"`
	} `json:"resource"`
	ScopeMetrics []otlpScopeMetrics `json:"scopeMetrics"`
}

type otlpMetricsRequest struct {
	ResourceMetrics []otlpResourceMetrics `json:"resourceMetrics"`
}

// promSample is one sample line of a Prometheus text exposition.
type promSample struct {
	name   string
	labels [][2]string // key, value, in line order
	value  float64
}

// parsePromLine reads `name{k="v",...} value`. Every writer of the /metrics body
// writes a label value with %q or as a plain "..." literal, so strconv reads it
// back exactly. ok is false for a line of any other shape, including one with a
// timestamp after the value (no writer adds one).
func parsePromLine(line string) (promSample, bool) {
	var s promSample
	i := strings.IndexAny(line, "{ ")
	if i <= 0 {
		return s, false
	}
	s.name, line = line[:i], line[i:]
	if line[0] == '{' {
		line = line[1:]
		for !strings.HasPrefix(line, "}") {
			key, rest, ok := strings.Cut(line, "=")
			if !ok || key == "" {
				return s, false
			}
			quoted, err := strconv.QuotedPrefix(rest)
			if err != nil {
				return s, false
			}
			val, err := strconv.Unquote(quoted)
			if err != nil {
				return s, false
			}
			s.labels = append(s.labels, [2]string{key, val})
			line = strings.TrimPrefix(rest[len(quoted):], ",")
		}
		line = line[1:]
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(line), 64)
	if err != nil {
		return s, false
	}
	s.value = v
	return s, true
}

// promFamily gathers one # TYPE family's samples while the exposition is read.
type promFamily struct {
	name, typ, help string
	points          []otlpNumberPoint     // counter and gauge samples
	groups          map[string]*promGroup // summary and histogram samples, by label set
	order           []string              // groups' keys, first seen first
}

// promGroup is one label set of a summary or histogram family.
type promGroup struct {
	attrs      []otlpAttr
	sum, count float64
	hasCount   bool
	quantiles  []otlpQuantile // a summary's {quantile="q"} samples
	bounds     []float64      // the finite le bounds, in line order (ascending)
	cumulative []float64      // the _bucket value at each bound
	inf        float64        // the le="+Inf" bucket, the total when _count is missing
	hasInf     bool
}

// familyOf finds the family a sample belongs to: the one named for it, else the
// summary or histogram family its _sum, _count or _bucket suffix names. part is
// that suffix, or "" for a sample named for its family.
func familyOf(fams map[string]*promFamily, name string) (*promFamily, string) {
	if f := fams[name]; f != nil {
		return f, ""
	}
	for _, part := range []string{"_sum", "_count", "_bucket"} {
		base, ok := strings.CutSuffix(name, part)
		if !ok {
			continue
		}
		if f := fams[base]; f != nil && (f.typ == "summary" || f.typ == "histogram") {
			return f, part
		}
	}
	return nil, ""
}

// add files one sample under its family. A histogram's le label becomes a bucket
// bound and a summary's quantile label a quantile, not attributes; a sample
// whose bound does not parse is skipped.
func (f *promFamily) add(s promSample, part, now string) {
	attrs := make([]otlpAttr, 0, len(s.labels))
	bound := ""
	for _, l := range s.labels {
		if (part == "_bucket" && l[0] == "le") || (part == "" && f.typ == "summary" && l[0] == "quantile") {
			bound = l[1]
			continue
		}
		attrs = append(attrs, otlpAttr{l[0], otlpValue{l[1]}})
	}
	if f.typ != "summary" && f.typ != "histogram" {
		p := otlpNumberPoint{Attributes: attrs, TimeUnixNano: now, AsDouble: s.value}
		if f.typ == "counter" && !carriedCounters[f.name] {
			p.StartTimeUnixNano = processStart
		}
		f.points = append(f.points, p)
		return
	}
	var at float64
	if part == "" || part == "_bucket" {
		v, err := strconv.ParseFloat(bound, 64)
		if err != nil {
			return
		}
		at = v
	}
	var key strings.Builder
	for _, a := range attrs {
		key.WriteString(a.Key + "\x00" + a.Value.StringValue + "\x00")
	}
	g := f.groups[key.String()]
	if g == nil {
		g = &promGroup{attrs: attrs}
		f.groups[key.String()] = g
		f.order = append(f.order, key.String())
	}
	switch part {
	case "":
		g.quantiles = append(g.quantiles, otlpQuantile{Quantile: at, Value: s.value})
	case "_sum":
		g.sum = s.value
	case "_count":
		g.count, g.hasCount = s.value, true
	case "_bucket":
		if math.IsInf(at, 1) {
			g.inf, g.hasInf = s.value, true
			return
		}
		g.bounds = append(g.bounds, at)
		g.cumulative = append(g.cumulative, s.value)
	}
}

// otlpCount writes a count as the decimal string OTLP's JSON mapping uses for a fixed64.
func otlpCount(v float64) string { return strconv.FormatUint(uint64(max(v, 0)), 10) }

// metric is the family as one OTLP metric, and false when it has no point.
func (f *promFamily) metric(now string) (otlpMetric, bool) {
	m := otlpMetric{Name: f.name, Description: f.help}
	switch f.typ {
	case "summary":
		pts := make([]otlpSummaryPoint, 0, len(f.order))
		for _, k := range f.order {
			g := f.groups[k]
			pts = append(pts, otlpSummaryPoint{Attributes: g.attrs, StartTimeUnixNano: processStart, TimeUnixNano: now,
				Count: otlpCount(g.count), Sum: g.sum, QuantileValues: g.quantiles})
		}
		m.Summary = &otlpSummary{DataPoints: pts}
		return m, len(pts) > 0
	case "histogram":
		pts := make([]otlpHistogramPoint, 0, len(f.order))
		for _, k := range f.order {
			g := f.groups[k]
			buckets := make([]string, 0, len(g.cumulative)+1)
			prev := 0.0
			for _, c := range g.cumulative {
				buckets = append(buckets, otlpCount(c-prev))
				prev = c
			}
			// The total is _count; without it, the +Inf bucket; without both, the last bucket.
			count := g.count
			if !g.hasCount {
				count = prev
				if g.hasInf {
					count = g.inf
				}
			}
			buckets = append(buckets, otlpCount(count-prev))
			pts = append(pts, otlpHistogramPoint{Attributes: g.attrs, StartTimeUnixNano: processStart, TimeUnixNano: now,
				Count: otlpCount(count), Sum: g.sum, BucketCounts: buckets, ExplicitBounds: append([]float64{}, g.bounds...)})
		}
		m.Histogram = &otlpHistogram{AggregationTemporality: temporalityCumulative, DataPoints: pts}
		return m, len(pts) > 0
	case "counter":
		m.Sum = &otlpSum{AggregationTemporality: temporalityCumulative, IsMonotonic: true, DataPoints: f.points}
	default:
		m.Gauge = &otlpGauge{DataPoints: f.points}
	}
	return m, len(f.points) > 0
}

// metricsResource is service.name=wardynd plus a service.instance.id per
// replica: "<WARDYN_AUDIT_SOURCE>/<host name>", or either one alone when the
// other is empty. Every replica pushes its own running totals, and the source is
// often one value for a whole install, so two replicas under it alone would read
// as one series that keeps resetting.
func metricsResource() []otlpAttr {
	host, _ := os.Hostname()
	id := Source
	switch {
	case Source == "":
		id = host
	case host != "":
		id = Source + "/" + host
	}
	attrs := []otlpAttr{{"service.name", otlpValue{"wardynd"}}}
	if id != "" {
		attrs = append(attrs, otlpAttr{"service.instance.id", otlpValue{id}})
	}
	return attrs
}

// encodeOTLPMetrics translates a Prometheus text exposition into one
// ExportMetricsServiceRequest. Each family keeps its name, its # HELP text as
// the description, and its labels as point attributes:
//   - counter: a cumulative monotonic sum starting at processStart, or with no
//     start time for the carriedCounters;
//   - gauge, and any sample with no # TYPE line: a gauge;
//   - summary: one point per label set from <name>_sum, <name>_count and any
//     {quantile="q"} samples;
//   - histogram: one cumulative point per label set, the _bucket running counts
//     turned into the per-bucket counts OTLP carries.
//
// A sample whose value is NaN or infinite is skipped (JSON has no such number), as
// is a line the parser cannot read, a timestamped one included. n is the number
// of metrics; an exposition with no sample yields n == 0 and no body.
func encodeOTLPMetrics(text []byte, now string) (body []byte, n int, err error) {
	fams := map[string]*promFamily{}
	var order []*promFamily
	family := func(name string) *promFamily {
		f := fams[name]
		if f == nil {
			f = &promFamily{name: name, groups: map[string]*promGroup{}}
			fams[name] = f
			order = append(order, f)
		}
		return f
	}
	sc := bufio.NewScanner(bytes.NewReader(text))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if rest, ok := strings.CutPrefix(line, "# HELP "); ok {
			name, help, _ := strings.Cut(rest, " ")
			family(name).help = help
			continue
		}
		if rest, ok := strings.CutPrefix(line, "# TYPE "); ok {
			name, typ, _ := strings.Cut(rest, " ")
			family(name).typ = typ
			continue
		}
		if line == "" || line[0] == '#' {
			continue
		}
		s, ok := parsePromLine(line)
		if !ok || math.IsNaN(s.value) || math.IsInf(s.value, 0) {
			continue
		}
		f, part := familyOf(fams, s.name)
		if f == nil {
			f = family(s.name)
		}
		f.add(s, part, now)
	}
	if err := sc.Err(); err != nil {
		return nil, 0, err
	}
	metrics := make([]otlpMetric, 0, len(order))
	for _, f := range order {
		if m, ok := f.metric(now); ok {
			metrics = append(metrics, m)
		}
	}
	if len(metrics) == 0 {
		return nil, 0, nil
	}
	var rm otlpResourceMetrics
	rm.Resource.Attributes = metricsResource()
	var scope otlpScopeMetrics
	scope.Scope.Name = "wardynd"
	scope.Metrics = metrics
	rm.ScopeMetrics = []otlpScopeMetrics{scope}
	body, err = json.Marshal(otlpMetricsRequest{ResourceMetrics: []otlpResourceMetrics{rm}})
	return body, len(metrics), err
}
