// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package sinks_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit/sinks"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	validTP   = "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	tpTraceID = "0af7651916cd43dd8448eb211c80319c"
	tpParent  = "b7ad6b7169203331"
)

func traceEvent(corr string) types.AuditEvent {
	ev := goldenEvent()
	if corr == "" {
		ev.Data = json.RawMessage(`{}`)
	} else {
		ev.Data = json.RawMessage(`{"correlation_id":"` + corr + `"}`)
	}
	return ev
}

// sendAndCollect delivers evs through a sink with traces on and returns the
// decoded first log record and the first span the collector received (zero values
// when none arrived), plus every request path in order.
func sendAndCollect(t *testing.T, evs ...types.AuditEvent) (rec map[string]any, sp map[string]any, paths []string) {
	t.Helper()
	rcv := newOTLPReceiver(t, nil)
	sink, cancel, done := startOTLP(t, sinks.OTLPConfig{Endpoint: rcv.srv.URL, BatchSize: len(evs), FlushInterval: "20ms", Traces: true})
	for _, ev := range evs {
		if err := sink.Emit(context.Background(), ev); err != nil {
			t.Fatalf("Emit: %v", err)
		}
	}
	waitThenSettle(t, 60*time.Millisecond, func() bool { return rcv.count() >= 2 })
	cancel()
	<-done
	for i := 0; i < rcv.count(); i++ {
		req := rcv.get(i)
		paths = append(paths, req.path)
		var body map[string]any
		if err := json.Unmarshal(req.body, &body); err != nil {
			t.Fatalf("request %d is not JSON: %v", i, err)
		}
		switch req.path {
		case "/v1/logs":
			rec = firstOf(t, body, "resourceLogs", "scopeLogs", "logRecords")
		case "/v1/traces":
			sp = firstOf(t, body, "resourceSpans", "scopeSpans", "spans")
		}
	}
	return rec, sp, paths
}

func firstOf(t *testing.T, body map[string]any, a, b, c string) map[string]any {
	t.Helper()
	l1, _ := body[a].([]any)
	if len(l1) == 0 {
		t.Fatalf("no %s", a)
	}
	l2, _ := l1[0].(map[string]any)[b].([]any)
	if len(l2) == 0 {
		t.Fatalf("no %s", b)
	}
	l3, _ := l2[0].(map[string]any)[c].([]any)
	if len(l3) == 0 {
		t.Fatalf("no %s", c)
	}
	return l3[0].(map[string]any)
}

func attrOf(sp map[string]any, key string) string {
	for _, a := range sp["attributes"].([]any) {
		m := a.(map[string]any)
		if m["key"] == key {
			return m["value"].(map[string]any)["stringValue"].(string)
		}
	}
	return ""
}

func sha16(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:16])
}

// A valid traceparent in the correlation id is the span's trace and parent; the
// span shape, the three attributes and the log record's ids all follow.
func TestOTLPTraces_ValidTraceparentJoinsTheCallersTrace(t *testing.T) {
	t.Parallel()
	ev := traceEvent(validTP)
	rec, sp, paths := sendAndCollect(t, ev)
	if len(paths) != 2 || paths[0] != "/v1/logs" || paths[1] != "/v1/traces" {
		t.Fatalf("request paths = %v, want [/v1/logs /v1/traces]", paths)
	}
	spanSum := sha256.Sum256(ev.ID[:])
	wantSpan := hex.EncodeToString(spanSum[:8])
	if sp["traceId"] != tpTraceID || sp["parentSpanId"] != tpParent || sp["spanId"] != wantSpan {
		t.Errorf("span ids = %v / %v / %v, want %s / %s / %s", sp["traceId"], sp["parentSpanId"], sp["spanId"], tpTraceID, tpParent, wantSpan)
	}
	if sp["name"] != "egress.deny" {
		t.Errorf("name = %v, want the action", sp["name"])
	}
	ts := "1793707200123456789"
	if sp["startTimeUnixNano"] != ts || sp["endTimeUnixNano"] != ts {
		t.Errorf("times = %v / %v, want the row time %s", sp["startTimeUnixNano"], sp["endTimeUnixNano"], ts)
	}
	for k, want := range map[string]string{
		"wardyn.run_id": ev.RunID.String(), "wardyn.correlation_id": validTP, "wardyn.outcome": "denied",
	} {
		if got := attrOf(sp, k); got != want {
			t.Errorf("span %s = %q, want %q", k, got, want)
		}
	}
	if rec["traceId"] != sp["traceId"] || rec["spanId"] != sp["spanId"] {
		t.Errorf("log ids %v/%v differ from span ids %v/%v", rec["traceId"], rec["spanId"], sp["traceId"], sp["spanId"])
	}
}

// Anything short of a full lowercase version-00 traceparent is not joined: the
// trace id is derived from the correlation id instead, with no parent.
func TestOTLPTraces_InvalidTraceparentIsDerivedNotJoined(t *testing.T) {
	t.Parallel()
	zeros := strings.Repeat("0", 32)
	for name, corr := range map[string]string{
		"uppercase hex":      "00-0AF7651916CD43DD8448EB211C80319C-b7ad6b7169203331-01",
		"version ff":         "ff-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01",
		"version 01":         "01-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01",
		"zero trace id":      "00-" + zeros + "-b7ad6b7169203331-01",
		"zero parent id":     "00-0af7651916cd43dd8448eb211c80319c-0000000000000000-01",
		"no flags":           "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331",
		"flags one digit":    "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-1",
		"flags not hex":      "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-zz",
		"short trace id":     "00-0af7651916cd43dd8448eb211c8031-b7ad6b7169203331-01",
		"extra field":        "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01-x",
		"trailing separator": validTP + "-",
		"non-hex char":       "00-0af7651916cd43dd8448eb211c80319g-b7ad6b7169203331-01",
		"plain build id":     "build-42",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, sp, _ := sendAndCollect(t, traceEvent(corr))
			if want := sha16(corr); sp["traceId"] != want {
				t.Errorf("traceId = %v, want SHA-256(correlation id)[:16] = %s", sp["traceId"], want)
			}
			if _, has := sp["parentSpanId"]; has {
				t.Errorf("parentSpanId = %v, want none for a correlation id that is not a valid traceparent", sp["parentSpanId"])
			}
		})
	}
}

// With no correlation id the trace is the run's own id.
func TestOTLPTraces_NoCorrelationIDUsesTheRunID(t *testing.T) {
	t.Parallel()
	ev := traceEvent("")
	_, sp, _ := sendAndCollect(t, ev)
	if want := strings.ReplaceAll(ev.RunID.String(), "-", ""); sp["traceId"] != want {
		t.Errorf("traceId = %v, want the run id's 16 bytes %s", sp["traceId"], want)
	}
}

// A row with no run has no trace: no span, and its log record carries no ids.
func TestOTLPTraces_RowWithoutARunIsNotSpanned(t *testing.T) {
	t.Parallel()
	rcv := newOTLPReceiver(t, nil)
	sink, cancel, done := startOTLP(t, sinks.OTLPConfig{Endpoint: rcv.srv.URL, BatchSize: 1, FlushInterval: "20ms", Traces: true})
	ev := traceEvent(validTP)
	ev.RunID = nil
	if err := sink.Emit(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	waitThenSettle(t, 100*time.Millisecond, func() bool { return rcv.count() >= 2 })
	cancel()
	<-done
	if rcv.count() != 1 || rcv.get(0).path != "/v1/logs" {
		t.Fatalf("requests = %d, want one logs request and no traces request", rcv.count())
	}
	if strings.Contains(string(rcv.get(0).body), "traceId") {
		t.Errorf("a row with no run carries a trace id: %s", rcv.get(0).body)
	}
}

// The ids are a pure function of the row: the same row encodes the same ids from
// two sinks, so a retry and the logs and traces deliveries cannot disagree.
func TestOTLPTraces_IDsArePureFunctionsOfTheRow(t *testing.T) {
	t.Parallel()
	ev := traceEvent("build-42")
	recA, spA, _ := sendAndCollect(t, ev)
	recB, spB, _ := sendAndCollect(t, ev)
	for _, k := range []string{"traceId", "spanId"} {
		if spA[k] != spB[k] || recA[k] != recB[k] || spA[k] != recA[k] {
			t.Errorf("%s differs: spans %v/%v logs %v/%v", k, spA[k], spB[k], recA[k], recB[k])
		}
	}
	other := ev
	other.ID = uuid.New()
	_, spC, _ := sendAndCollect(t, other)
	if spC["spanId"] == spA["spanId"] {
		t.Error("two rows with different event ids share a span id")
	}
	if spC["traceId"] != spA["traceId"] {
		t.Error("two rows with one correlation id are on different traces")
	}
}

// Traces are off by default: a logs-only collector sees one request and no ids.
func TestOTLPTraces_OffByDefault(t *testing.T) {
	t.Parallel()
	rcv := newOTLPReceiver(t, nil)
	sink, cancel, done := startOTLP(t, sinks.OTLPConfig{Endpoint: rcv.srv.URL, BatchSize: 1, FlushInterval: "20ms"})
	if err := sink.Emit(context.Background(), traceEvent(validTP)); err != nil {
		t.Fatal(err)
	}
	waitThenSettle(t, 100*time.Millisecond, func() bool { return rcv.count() >= 2 })
	cancel()
	<-done
	if rcv.count() != 1 || rcv.get(0).path != "/v1/logs" || strings.Contains(string(rcv.get(0).body), "traceId") {
		t.Fatalf("with traces off: %d requests, body %s", rcv.count(), rcv.get(0).body)
	}
}

// Spans use the same retry and drop accounting as logs, counted per span.
func TestOTLPTraces_RetryAndDropAccounting(t *testing.T) {
	t.Parallel()
	// One receiver fails every request, logs and traces alike.
	failTraces := newOTLPReceiver(t, func(int) int { return 500 })
	sink, cancel, done := startOTLP(t, sinks.OTLPConfig{
		Endpoint: failTraces.srv.URL, BatchSize: 2, FlushInterval: "20ms", MaxRetries: 2, RetryBaseDelay: "1ms", Traces: true,
	})
	noRun := traceEvent("")
	noRun.RunID = nil
	for _, ev := range []types.AuditEvent{traceEvent(validTP), noRun} {
		if err := sink.Emit(context.Background(), ev); err != nil {
			t.Fatal(err)
		}
	}
	waitThenSettle(t, 100*time.Millisecond, func() bool { return failTraces.count() >= 4 })
	cancel()
	<-done
	// The logs request fails too on this receiver: 2 rows dropped from logs, 1 span from traces.
	if got := sink.Drops(); got != 3 {
		t.Errorf("drops = %d, want 3 (2 log records + 1 span)", got)
	}
	if failTraces.count() != 4 {
		t.Errorf("requests = %d, want 2 attempts each for logs and traces", failTraces.count())
	}
}

// A collector's partialSuccess on the traces endpoint adds its rejected spans to the drop counter.
func TestOTLPTraces_RejectedSpansAreCounted(t *testing.T) {
	t.Parallel()
	rcv := newOTLPReceiverReply(t, nil, `{"partialSuccess":{"rejectedSpans":"1"}}`)
	sink, cancel, done := startOTLP(t, sinks.OTLPConfig{Endpoint: rcv.srv.URL, BatchSize: 1, FlushInterval: "20ms", Traces: true})
	if err := sink.Emit(context.Background(), traceEvent(validTP)); err != nil {
		t.Fatal(err)
	}
	waitThenSettle(t, 60*time.Millisecond, func() bool { return rcv.count() >= 2 })
	cancel()
	<-done
	// The same reply answers the logs request, which names no rejected log records.
	if got := sink.Drops(); got != 1 {
		t.Errorf("drops = %d, want 1 rejected span", got)
	}
}

// Both requests keep the endpoint's escaped path prefix and its query, whether or
// not the endpoint already ends in /v1/logs.
func TestOTLPTraces_ConfigAndPath(t *testing.T) {
	t.Parallel()
	got, err := sinks.ParseSinks([]byte(`{"otlp":{"endpoint":"https://o.example.com:4318/prefix/v1/logs?k=v","traces":true}}`))
	if err != nil || len(got) != 1 {
		t.Fatalf("ParseSinks: %v, %d sinks", err, len(got))
	}
	for name, suffix := range map[string]string{"bare prefix": "", "ends in /v1/logs": "/v1/logs"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var mu sync.Mutex
			var uris []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				uris = append(uris, r.RequestURI)
				mu.Unlock()
			}))
			t.Cleanup(srv.Close)
			sink, cancel, done := startOTLP(t, sinks.OTLPConfig{
				Endpoint: srv.URL + "/tenant%2Fblue" + suffix + "?route=1", BatchSize: 1, FlushInterval: "20ms", Traces: true,
			})
			if err := sink.Emit(context.Background(), traceEvent(validTP)); err != nil {
				t.Fatal(err)
			}
			waitThenSettle(t, 60*time.Millisecond, func() bool { mu.Lock(); defer mu.Unlock(); return len(uris) >= 2 })
			cancel()
			<-done
			mu.Lock()
			defer mu.Unlock()
			want := []string{"/tenant%2Fblue/v1/logs?route=1", "/tenant%2Fblue/v1/traces?route=1"}
			if len(uris) != 2 || uris[0] != want[0] || uris[1] != want[1] {
				t.Errorf("request URIs = %v, want %v", uris, want)
			}
		})
	}
}

// A row shed on a full queue loses its log record and, with traces on and a run on
// the row, its span too; the delivery-loss accounting counts the same two.
func TestOTLPTraces_QueueOverflowDropAccounting(t *testing.T) {
	t.Parallel()
	noRun := traceEvent("")
	noRun.RunID = nil
	for name, tc := range map[string]struct {
		traces bool
		second types.AuditEvent
		want   int64
	}{
		"traces on, run-scoped":  {true, traceEvent(validTP), 2},
		"traces on, no run":      {true, noRun, 1},
		"traces off, run-scoped": {false, traceEvent(validTP), 1},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// The flusher is never started, so the one-slot queue stays full.
			sink, err := sinks.NewOTLPSink(sinks.OTLPConfig{Endpoint: "http://127.0.0.1:1", BufferSize: 1, Traces: tc.traces})
			if err != nil {
				t.Fatal(err)
			}
			for _, ev := range []types.AuditEvent{traceEvent(validTP), tc.second} {
				if err := sink.Emit(context.Background(), ev); err != nil {
					t.Fatal(err)
				}
			}
			if got := sink.Drops(); got != tc.want {
				t.Errorf("drops = %d, want %d", got, tc.want)
			}
		})
	}
}
