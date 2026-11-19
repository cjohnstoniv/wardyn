// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package sinks_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/audit/sinks"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

// otlpReceiver is a stand-in collector that records each request it gets.
type otlpReceiver struct {
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []otlpRequest
}

type otlpRequest struct {
	path   string
	header http.Header
	body   []byte
}

func newOTLPReceiver(t *testing.T, status func(n int) int) *otlpReceiver {
	return newOTLPReceiverReply(t, status, "")
}

// newOTLPReceiverReply is newOTLPReceiver answering every request with reply as its body.
func newOTLPReceiverReply(t *testing.T, status func(n int) int, reply string) *otlpReceiver {
	t.Helper()
	r := &otlpReceiver{}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.reqs = append(r.reqs, otlpRequest{req.URL.Path, req.Header.Clone(), b})
		n := len(r.reqs)
		r.mu.Unlock()
		code := http.StatusOK
		if status != nil {
			code = status(n)
		}
		w.WriteHeader(code)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *otlpReceiver) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.reqs)
}

func (r *otlpReceiver) get(i int) otlpRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reqs[i]
}

func startOTLP(t *testing.T, cfg sinks.OTLPConfig) (*sinks.OTLPSink, context.CancelFunc, <-chan struct{}) {
	t.Helper()
	sink, err := sinks.NewOTLPSink(cfg)
	if err != nil {
		t.Fatalf("NewOTLPSink: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { sink.Run(ctx); close(done) }()
	return sink, cancel, done
}

func goldenEvent() types.AuditEvent {
	run := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	return types.AuditEvent{
		ID:        uuid.MustParse("99999999-2222-3333-4444-555555555555"),
		Time:      time.Unix(1793707200, 123456789).UTC(), // passthrough data: only its nanosecond rendering is asserted
		RunID:     &run,
		ActorType: types.ActorSystem,
		Actor:     "spiffe://wardyn/run/abc",
		Action:    "egress.deny",
		Target:    "example.com:443",
		Outcome:   "denied",
		SourceIP:  "10.0.0.9",
		Data:      json.RawMessage(`{"correlation_id":"build-42","host":"example.com","note":"not exported"}`),
		RowHash:   "deadbeef",
	}
}

// One event is exactly the golden request, so the wire shape cannot drift, and
// nothing from the row beyond the documented attributes reaches the collector.
func TestOTLPSink_GoldenRecord(t *testing.T) {
	t.Parallel()
	rcv := newOTLPReceiver(t, nil)
	sink, cancel, done := startOTLP(t, sinks.OTLPConfig{
		Endpoint: rcv.srv.URL, BatchSize: 1, FlushInterval: "20ms",
		Headers: map[string]string{"X-Scope-OrgID": "team-a"},
	})
	if err := sink.Emit(context.Background(), goldenEvent()); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	waitThenSettle(t, 30*time.Millisecond, func() bool { return rcv.count() > 0 })
	cancel()
	<-done

	if rcv.count() != 1 {
		t.Fatalf("collector requests: got %d, want 1", rcv.count())
	}
	req := rcv.get(0)
	if req.path != "/v1/logs" {
		t.Errorf("path = %q, want /v1/logs", req.path)
	}
	if ct := req.header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if got := req.header.Get("X-Scope-OrgID"); got != "team-a" {
		t.Errorf("X-Scope-OrgID = %q, want team-a", got)
	}
	want, err := os.ReadFile("testdata/otlp_logs_golden.json")
	if err != nil {
		t.Fatal(err)
	}
	if string(req.body) != string(want) {
		t.Errorf("request body differs from golden\n got: %s\nwant: %s", req.body, want)
	}
	for _, leaked := range []string{"not exported", "10.0.0.9", "deadbeef", "99999999"} {
		if strings.Contains(string(req.body), leaked) {
			t.Errorf("request body carries %q, which is outside the exported fields", leaked)
		}
	}
}

// The JSON shape, severity per outcome, the optional attributes and the batch.
func TestOTLPSink_ShapeSeverityAndOptionalAttributes(t *testing.T) {
	t.Parallel()
	rcv := newOTLPReceiver(t, nil)
	sink, cancel, done := startOTLP(t, sinks.OTLPConfig{Endpoint: rcv.srv.URL, BatchSize: 4, FlushInterval: "1h"})
	mk := func(action, outcome string, data string) types.AuditEvent {
		ev := makeEvent(action)
		ev.Outcome = outcome
		if data != "" {
			ev.Data = json.RawMessage(data)
		}
		return ev
	}
	long := `{"correlation_id":"` + strings.Repeat("x", 129) + `"}`
	for _, ev := range []types.AuditEvent{
		mk("a.ok", "success", `{"correlation_id":"c1"}`),
		mk("a.fail", "failure", `{"other":1}`),
		mk("a.deny", "denied", long),
		mk("a.odd", "weird", `not json with "correlation_id"`),
	} {
		_ = sink.Emit(context.Background(), ev)
	}
	waitThenSettle(t, 30*time.Millisecond, func() bool { return rcv.count() > 0 })
	cancel()
	<-done

	var got struct {
		ResourceLogs []struct {
			Resource struct {
				Attributes []struct {
					Key   string `json:"key"`
					Value struct {
						StringValue string `json:"stringValue"`
					} `json:"value"`
				} `json:"attributes"`
			} `json:"resource"`
			ScopeLogs []struct {
				LogRecords []struct {
					SeverityNumber int    `json:"severityNumber"`
					SeverityText   string `json:"severityText"`
					Body           struct {
						StringValue string `json:"stringValue"`
					} `json:"body"`
					Attributes []struct {
						Key   string `json:"key"`
						Value struct {
							StringValue string `json:"stringValue"`
						} `json:"value"`
					} `json:"attributes"`
				} `json:"logRecords"`
			} `json:"scopeLogs"`
		} `json:"resourceLogs"`
	}
	if err := json.Unmarshal(rcv.get(0).body, &got); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if len(got.ResourceLogs) != 1 || len(got.ResourceLogs[0].ScopeLogs) != 1 {
		t.Fatalf("want one resource and one scope, got %+v", got)
	}
	if a := got.ResourceLogs[0].Resource.Attributes; len(a) != 1 || a[0].Key != "service.name" || a[0].Value.StringValue != "wardynd" {
		t.Errorf("resource attributes = %+v, want service.name=wardynd only", a)
	}
	recs := got.ResourceLogs[0].ScopeLogs[0].LogRecords
	if len(recs) != 4 {
		t.Fatalf("records in the batch: got %d, want 4", len(recs))
	}
	wantSev := []struct {
		body string
		num  int
		text string
		corr bool
	}{{"a.ok", 9, "INFO", true}, {"a.fail", 17, "ERROR", false}, {"a.deny", 13, "WARN", false}, {"a.odd", 0, "", false}}
	for i, w := range wantSev {
		r := recs[i]
		if r.Body.StringValue != w.body || r.SeverityNumber != w.num || r.SeverityText != w.text {
			t.Errorf("record %d = body %q sev %d/%q, want %q %d/%q", i, r.Body.StringValue, r.SeverityNumber, r.SeverityText, w.body, w.num, w.text)
		}
		hasCorr, hasRun, hasTarget := false, false, false
		for _, a := range r.Attributes {
			switch a.Key {
			case "wardyn.correlation_id":
				hasCorr = true
			case "wardyn.run_id":
				hasRun = true
			case "wardyn.target":
				hasTarget = true
			}
		}
		if hasCorr != w.corr {
			t.Errorf("record %d wardyn.correlation_id present = %v, want %v", i, hasCorr, w.corr)
		}
		if hasRun || hasTarget {
			t.Errorf("record %d carries run_id=%v target=%v for an event with neither", i, hasRun, hasTarget)
		}
	}
}

func TestOTLPSink_EndpointPath(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ suffix, want string }{
		{"", "/v1/logs"}, {"/", "/v1/logs"}, {"/otel", "/otel/v1/logs"}, {"/v1/logs", "/v1/logs"},
	} {
		rcv := newOTLPReceiver(t, nil)
		sink, cancel, done := startOTLP(t, sinks.OTLPConfig{Endpoint: rcv.srv.URL + tc.suffix, BatchSize: 1, FlushInterval: "1h"})
		_ = sink.Emit(context.Background(), makeEvent("p.test"))
		waitThenSettle(t, 10*time.Millisecond, func() bool { return rcv.count() > 0 })
		cancel()
		<-done
		if rcv.count() != 1 || rcv.get(0).path != tc.want {
			t.Errorf("endpoint suffix %q: got %d requests, path %q, want path %q", tc.suffix, rcv.count(), func() string {
				if rcv.count() > 0 {
					return rcv.get(0).path
				}
				return ""
			}(), tc.want)
		}
	}
}

func TestOTLPSink_RetryOnServerError(t *testing.T) {
	t.Parallel()
	rcv := newOTLPReceiver(t, func(n int) int {
		if n < 3 {
			return http.StatusServiceUnavailable
		}
		return http.StatusOK
	})
	sink, cancel, done := startOTLP(t, sinks.OTLPConfig{
		Endpoint: rcv.srv.URL, BatchSize: 1, FlushInterval: "1h", MaxRetries: 3, RetryBaseDelay: "1ms",
	})
	_ = sink.Emit(context.Background(), makeEvent("retry.test"))
	waitThenSettle(t, 30*time.Millisecond, func() bool { return rcv.count() >= 3 })
	cancel()
	<-done
	if rcv.count() != 3 {
		t.Errorf("attempts: got %d, want 3 (two 503s, then a 200)", rcv.count())
	}
	if sink.Drops() != 0 {
		t.Errorf("drops after a delivery that succeeded on retry: got %d, want 0", sink.Drops())
	}
}

func TestOTLPSink_DropCounterOnRetryExhaustion(t *testing.T) {
	t.Parallel()
	rcv := newOTLPReceiver(t, func(int) int { return http.StatusInternalServerError })
	sink, cancel, done := startOTLP(t, sinks.OTLPConfig{
		Endpoint: rcv.srv.URL, BatchSize: 2, FlushInterval: "1h", MaxRetries: 3, RetryBaseDelay: "1ms",
	})
	_ = sink.Emit(context.Background(), makeEvent("drop.a"))
	_ = sink.Emit(context.Background(), makeEvent("drop.b"))
	waitThenSettle(t, 30*time.Millisecond, func() bool { return sink.Drops() != 0 })
	cancel()
	<-done
	if rcv.count() != 3 {
		t.Errorf("attempts: got %d, want 3", rcv.count())
	}
	if sink.Drops() != 2 {
		t.Errorf("drops after retry exhaustion: got %d, want 2 (the whole batch)", sink.Drops())
	}
}

// A full queue sheds the event and counts it; Emit does not wait on a stuck
// collector or on a flusher that is not running.
func TestOTLPSink_FullQueueDropsAndCountsWithoutBlocking(t *testing.T) {
	t.Parallel()
	sink, err := sinks.NewOTLPSink(sinks.OTLPConfig{Endpoint: "http://127.0.0.1:1", BufferSize: 2})
	if err != nil {
		t.Fatalf("NewOTLPSink: %v", err)
	}
	finished := make(chan struct{})
	go func() {
		for i := 0; i < 10; i++ {
			_ = sink.Emit(context.Background(), makeEvent("overflow.test"))
		}
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("Emit blocked on a full queue")
	}
	if got := sink.Drops(); got != 8 {
		t.Errorf("drops with no reader and buffer 2: got %d, want 8", got)
	}
}

func TestOTLPSink_CloseFlushesBufferedEvents(t *testing.T) {
	t.Parallel()
	var received atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		received.Add(int32(strings.Count(string(b), `"body":{"stringValue":"close.test"}`)))
	}))
	t.Cleanup(srv.Close)
	sink, err := sinks.NewOTLPSink(sinks.OTLPConfig{Endpoint: srv.URL, BatchSize: 100, FlushInterval: "1h"})
	if err != nil {
		t.Fatal(err)
	}
	go sink.Run(context.Background())
	for i := 0; i < 5; i++ {
		_ = sink.Emit(context.Background(), makeEvent("close.test"))
	}
	if err := sink.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := received.Load(); got != 5 {
		t.Errorf("records delivered by the shutdown flush: got %d, want 5", got)
	}
}

func TestOTLPSink_ConfigRefusals(t *testing.T) {
	t.Parallel()
	const token = "s3cr3t-b34r3r"
	tests := []struct {
		name    string
		cfg     sinks.OTLPConfig
		wantErr string // substring of the plain-sentence error; "" means accepted
	}{
		{"no endpoint", sinks.OTLPConfig{}, "endpoint is required"},
		{"http with bearer refused", sinks.OTLPConfig{Endpoint: "http://otel.internal:4318", BearerToken: token}, "bearer_token requires an https:// endpoint"},
		{"schemeless refused", sinks.OTLPConfig{Endpoint: "otel.internal:4318"}, "must be an http:// or https:// url"},
		{"hostless refused", sinks.OTLPConfig{Endpoint: "http:///x"}, "has no host"},
		{"https with bearer ok", sinks.OTLPConfig{Endpoint: "https://otel.internal", BearerToken: token}, ""},
		{"http without bearer ok", sinks.OTLPConfig{Endpoint: "http://otel.internal:4318"}, ""},
		{"authorization header refused", sinks.OTLPConfig{Endpoint: "https://o", Headers: map[string]string{"Authorization": "Bearer x"}}, "bearer_token, not in a header"},
		{"host header refused", sinks.OTLPConfig{Endpoint: "https://o", Headers: map[string]string{"host": "x"}}, "set by the sink"},
		{"bad header name", sinks.OTLPConfig{Endpoint: "https://o", Headers: map[string]string{"a b": "x"}}, "not a valid header name"},
		{"header value line break", sinks.OTLPConfig{Endpoint: "https://o", Headers: map[string]string{"x-a": "v\nw"}}, "line break"},
		{"bad flush interval", sinks.OTLPConfig{Endpoint: "https://o", FlushInterval: "soon"}, "invalid flush_interval"},
		{"zero timeout", sinks.OTLPConfig{Endpoint: "https://o", Timeout: "0s"}, "timeout \"0s\" must be positive"},
		{"timeout over the shutdown budget", sinks.OTLPConfig{Endpoint: "https://o", Timeout: "16s"}, "exceeds 15s"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := sinks.NewOTLPSink(tt.cfg)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("want accepted, got %v", err)
			case tt.wantErr != "" && err == nil:
				t.Fatalf("want error containing %q, got none", tt.wantErr)
			case tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr):
				t.Fatalf("error %q does not contain %q", err, tt.wantErr)
			}
			if err != nil && strings.Contains(err.Error(), token) {
				t.Errorf("error leaks the bearer token: %v", err)
			}
		})
	}
}

// The otlp block is part of WARDYN_AUDIT_SINKS and builds a sink named for the
// drop counter's label.
func TestParseSinks_OTLP(t *testing.T) {
	t.Parallel()
	got, err := sinks.ParseSinks([]byte(`{"otlp":{"endpoint":"https://otel.example.com:4318","bearer_token":"t","headers":{"x-a":"b"},"batch_size":10}}`))
	if err != nil {
		t.Fatalf("ParseSinks: %v", err)
	}
	if len(got) != 1 || got[0].Name() != "otlp" {
		t.Fatalf("sinks = %v, want one named otlp", got)
	}
	if _, err := sinks.ParseSinks([]byte(`{"otlp":{"endpoint":"http://o","bearer_token":"t"}}`)); err == nil {
		t.Error("an http endpoint with a bearer token booted; want a refusal")
	}
	if _, err := sinks.ParseSinks([]byte(`{"otlp":{"endpoint":"https://o","bearer":"t"}}`)); err == nil {
		t.Error("an unknown otlp key booted; want a refusal")
	}
	// The drop count is read through the fan-out, as /metrics reads it.
	fan := sinks.NewFanout(got...)
	if d := fan.DropsByName()["otlp"]; d != 0 {
		t.Errorf("drops for a fresh otlp sink: got %d, want 0", d)
	}
}

// WARDYN_AUDIT_SOURCE reaches the collector as the resource's service.instance.id.
// Not t.Parallel(): sinks.Source is process-global, as in TestSourceStamp.
func TestOTLPSink_SourceBecomesServiceInstanceID(t *testing.T) {
	orig := sinks.Source
	t.Cleanup(func() { sinks.Source = orig })
	sinks.Source = "prod-us-east"
	rcv := newOTLPReceiver(t, nil)
	sink, cancel, done := startOTLP(t, sinks.OTLPConfig{Endpoint: rcv.srv.URL, BatchSize: 1, FlushInterval: "1h"})
	_ = sink.Emit(context.Background(), makeEvent("src.test"))
	waitThenSettle(t, 10*time.Millisecond, func() bool { return rcv.count() > 0 })
	cancel()
	<-done
	if rcv.count() != 1 || !strings.Contains(string(rcv.get(0).body), `{"key":"service.instance.id","value":{"stringValue":"prod-us-east"}}`) {
		t.Errorf("service.instance.id missing from the request: %d requests", rcv.count())
	}
}

// A 200 whose partialSuccess rejects records counts them as drops, once: the
// accepted records are not resent, and only the count reaches the log, never
// the collector's errorMessage. Not t.Parallel(): it swaps the default logger.
func TestOTLPSink_PartialSuccessCountsRejectedRecordsWithoutRetry(t *testing.T) {
	var logs strings.Builder
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(orig) })

	for _, reply := range []string{
		`{"partialSuccess":{"rejectedLogRecords":"1","errorMessage":"secret-collector-detail"}}`,
		`{"partialSuccess":{"rejectedLogRecords":1,"errorMessage":"secret-collector-detail"}}`,
	} {
		rcv := newOTLPReceiverReply(t, nil, reply)
		sink, cancel, done := startOTLP(t, sinks.OTLPConfig{
			Endpoint: rcv.srv.URL, BatchSize: 2, FlushInterval: "1h", MaxRetries: 3, RetryBaseDelay: "1ms",
		})
		_ = sink.Emit(context.Background(), makeEvent("partial.a"))
		_ = sink.Emit(context.Background(), makeEvent("partial.b"))
		waitThenSettle(t, 30*time.Millisecond, func() bool { return sink.Drops() != 0 })
		cancel()
		<-done
		if rcv.count() != 1 {
			t.Errorf("reply %s: requests = %d, want 1 (no retry of accepted records)", reply, rcv.count())
		}
		if sink.Drops() != 1 {
			t.Errorf("reply %s: drops = %d, want 1", reply, sink.Drops())
		}
	}
	if !strings.Contains(logs.String(), "rejected=1") {
		t.Errorf("the rejected count is not logged: %q", logs.String())
	}
	if strings.Contains(logs.String(), "secret-collector-detail") {
		t.Errorf("the collector's errorMessage reached the log: %q", logs.String())
	}
}

// An empty, malformed or zero-rejection 2xx body is plain success.
func TestOTLPSink_UnparseableOrEmptyReplyIsSuccess(t *testing.T) {
	t.Parallel()
	for _, reply := range []string{``, `not json`, `{}`, `{"partialSuccess":{}}`, `{"partialSuccess":{"rejectedLogRecords":"0"}}`, `{"partialSuccess":{"rejectedLogRecords":"many"}}`} {
		rcv := newOTLPReceiverReply(t, nil, reply)
		sink, cancel, done := startOTLP(t, sinks.OTLPConfig{Endpoint: rcv.srv.URL, BatchSize: 1, FlushInterval: "1h"})
		_ = sink.Emit(context.Background(), makeEvent("ok.test"))
		waitThenSettle(t, 20*time.Millisecond, func() bool { return rcv.count() > 0 })
		cancel()
		<-done
		if rcv.count() != 1 || sink.Drops() != 0 {
			t.Errorf("reply %q: requests = %d, drops = %d, want 1 and 0", reply, rcv.count(), sink.Drops())
		}
	}
}
