// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Tests for acknowledged audit delivery (#1513): the config gate that keeps
// the acknowledged webhook out of the fan-out, and the loop's outage resume,
// restart resume, terminal refusal and gap reporting.
//
// This file is in package sinks, not sinks_test, because it drives
// AckedWebhook.step directly — the unit that decides wait/stop — instead of
// sleeping on the loop's real backoff.
package sinks

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// fakeDeliveryStore is an in-memory DeliveryStore holding the audit rows and
// the one checkpoint row, with the store's own semantics: EnsureAuditDeliveryCursor
// starts an ABSENT row at the head (first enable sends only new events) and
// leaves an existing one alone, and Set of the zero position counts a reset.
type fakeDeliveryStore struct {
	mu      sync.Mutex
	rows    []types.FederatedAuditEvent
	haveRow bool
	acked   int64
	hash    string
	lastErr string
	halted  bool
	resets  int
}

var _ DeliveryStore = (*fakeDeliveryStore)(nil)

func (f *fakeDeliveryStore) ListAuditEventsAfterSeq(_ context.Context, seq int64, limit int) ([]types.FederatedAuditEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if limit <= 0 {
		limit = 1000
	}
	out := []types.FederatedAuditEvent{}
	for _, r := range f.rows {
		if r.Seq <= seq {
			continue
		}
		out = append(out, r)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (f *fakeDeliveryStore) EnsureAuditDeliveryCursor(_ context.Context, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.haveRow {
		return nil
	}
	f.haveRow = true
	for _, r := range f.rows {
		if r.Seq > f.acked {
			f.acked, f.hash = r.Seq, r.RowHash
		}
	}
	return nil
}

func (f *fakeDeliveryStore) GetAuditDeliveryCursor(_ context.Context, _ string) (int64, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.acked, f.hash, nil
}

func (f *fakeDeliveryStore) SetAuditDeliveryCursor(_ context.Context, _ string, seq int64, hash string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if seq == 0 {
		f.resets++
	}
	f.acked, f.hash = seq, hash
	return nil
}

func (f *fakeDeliveryStore) SetAuditDeliveryError(_ context.Context, _, msg string, halted bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastErr, f.halted = msg, halted
	return nil
}

// state is the checkpoint row as the loop left it, for assertions.
func (f *fakeDeliveryStore) state() (acked int64, hash, lastErr string, halted bool, resets int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.acked, f.hash, f.lastErr, f.halted, f.resets
}

// setRows replaces the audit rows the source reads.
func (f *fakeDeliveryStore) setRows(rows []types.FederatedAuditEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows = rows
}

// fakeRecorder records what the loop wrote to the audit trail.
type fakeRecorder struct {
	mu     sync.Mutex
	events []types.AuditEvent
}

var _ audit.Recorder = (*fakeRecorder)(nil)

func (r *fakeRecorder) Record(_ context.Context, ev types.AuditEvent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
	return nil
}

func (r *fakeRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

func (r *fakeRecorder) first() types.AuditEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.events[0]
}

// ackedRows gives n chained rows, seq 1..n, each with a distinct id and hash
// the way audit_events stores them.
func ackedRows(n int) []types.FederatedAuditEvent {
	out := make([]types.FederatedAuditEvent, 0, n)
	for seq := int64(1); seq <= int64(n); seq++ {
		out = append(out, types.FederatedAuditEvent{
			AuditEvent: types.AuditEvent{
				ID: uuid.New(), Time: time.Now().UTC(), ActorType: types.ActorSystem,
				Actor: "acked-probe", Action: "acked.probe", Outcome: "success",
				RowHash: "h" + strconv.FormatInt(seq, 10),
			},
			Seq: seq,
		})
	}
	return out
}

// lineCollector is a collector that records every NDJSON line it accepts and
// refuses the first refuses requests with failStatus, if set.
type lineCollector struct {
	mu         sync.Mutex
	lines      []ackedLine
	requests   int
	failStatus int
	failFirst  int
	tls        bool
}

type ackedLine struct {
	ID      uuid.UUID `json:"id"`
	Seq     int64     `json:"seq"`
	RowHash string    `json:"row_hash"`
}

func (c *lineCollector) server(t *testing.T) *httptest.Server {
	t.Helper()
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.requests++
		n, fail := c.requests, c.failStatus
		c.mu.Unlock()
		if fail != 0 && n <= c.failFirst {
			w.WriteHeader(fail)
			return
		}
		body, _ := readAll(r)
		sc := bufio.NewScanner(bytes.NewReader(body))
		c.mu.Lock()
		defer c.mu.Unlock()
		for sc.Scan() {
			if len(sc.Bytes()) == 0 {
				continue
			}
			var l ackedLine
			if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
				t.Errorf("collector: unmarshal line: %v", err)
				continue
			}
			c.lines = append(c.lines, l)
		}
		w.WriteHeader(http.StatusOK)
	})
	var srv *httptest.Server
	if c.tls {
		srv = httptest.NewTLSServer(h)
	} else {
		srv = httptest.NewServer(h)
	}
	t.Cleanup(srv.Close)
	return srv
}

func (c *lineCollector) snapshot() []ackedLine {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]ackedLine(nil), c.lines...)
}

func readAll(r *http.Request) ([]byte, error) {
	var buf bytes.Buffer
	_, err := buf.ReadFrom(r.Body)
	return buf.Bytes(), err
}

// In acknowledged mode ParseSinks returns no webhook sink — the leader-only
// loop reads the stored trail instead — and AcknowledgedWebhook hands back the
// config to build it with. With the delivery field absent the fan-out sink is
// unchanged and AcknowledgedWebhook has nothing to say.
func TestAcked_ParseSinksLeavesAcknowledgedWebhookOutOfTheFanout(t *testing.T) {
	t.Parallel()

	const url = "https://siem.example.com/ingest"
	ackJSON := []byte(`{"webhook":{"url":"` + url + `","delivery":"acknowledged","bearer_token":"t"}}`)

	built, err := ParseSinks(ackJSON)
	if err != nil {
		t.Fatalf("ParseSinks (acknowledged): %v", err)
	}
	for _, s := range built {
		if s.Name() == "webhook" {
			t.Fatal("acknowledged mode still built a fan-out webhook sink; every event would go down both paths")
		}
	}
	cfg, err := AcknowledgedWebhook(ackJSON)
	if err != nil {
		t.Fatalf("AcknowledgedWebhook: %v", err)
	}
	if cfg == nil {
		t.Fatal("AcknowledgedWebhook returned nil in acknowledged mode")
	}
	if cfg.URL != url || cfg.Delivery != DeliveryAcknowledged || cfg.BatchSize != 100 {
		t.Errorf("AcknowledgedWebhook = %+v, want the defaulted config for %s", *cfg, url)
	}

	bestJSON := []byte(`{"webhook":{"url":"` + url + `"}}`)
	built, err = ParseSinks(bestJSON)
	if err != nil {
		t.Fatalf("ParseSinks (best effort): %v", err)
	}
	var fanOut bool
	for _, s := range built {
		if _, ok := s.(*WebhookSink); ok {
			fanOut = true
		}
	}
	if !fanOut {
		t.Error("best-effort mode built no *WebhookSink; the default path changed")
	}
	if cfg, err := AcknowledgedWebhook(bestJSON); err != nil || cfg != nil {
		t.Errorf("AcknowledgedWebhook (best effort) = %v, %v; want nil, nil", cfg, err)
	}
	for _, blank := range []string{"", "   ", "\t\n"} {
		if cfg, err := AcknowledgedWebhook([]byte(blank)); err != nil || cfg != nil {
			t.Errorf("AcknowledgedWebhook(%q) = %v, %v; want nil, nil", blank, cfg, err)
		}
	}
}

// An unknown delivery value refuses boot, and the bearer-on-plaintext rule
// still holds in acknowledged mode: the credential gate belongs to the
// delivery, not to which mode carries it.
func TestAcked_RejectsUnknownDeliveryAndKeepsTheBearerHTTPSRule(t *testing.T) {
	t.Parallel()

	_, err := ParseSinks([]byte(`{"webhook":{"url":"https://siem.example.com/ingest","delivery":"sometimes"}}`))
	if err == nil {
		t.Fatal("ParseSinks accepted an unknown delivery mode")
	}
	if want := `delivery "sometimes" must be "best_effort" or "acknowledged"`; !strings.Contains(err.Error(), want) {
		t.Errorf("unknown delivery error = %v, want it to name %q", err, want)
	}
	if _, err := NewAckedWebhook(WebhookConfig{URL: "https://siem.example.com/ingest", Delivery: "sometimes"}, nil, nil); err == nil {
		t.Error("NewAckedWebhook accepted an unknown delivery mode")
	}

	plainBearer := []byte(`{"webhook":{"url":"http://siem.example.com/ingest","delivery":"acknowledged","bearer_token":"t"}}`)
	if _, err := AcknowledgedWebhook(plainBearer); err == nil {
		t.Error("AcknowledgedWebhook accepted a bearer on a plaintext url")
	} else if want := "bearer_token requires an https:// url"; !strings.Contains(err.Error(), want) {
		t.Errorf("plaintext bearer error = %v, want it to name the https rule", err)
	}
	if _, err := ParseSinks(plainBearer); err == nil {
		t.Error("ParseSinks accepted a bearer on a plaintext url in acknowledged mode")
	}
}

// A collector that answers 503 twice then 200: every event is still delivered
// from the checkpoint, in order, and the checkpoint stops on the last one —
// not on the first failure.
func TestAcked_OutageThenResumeDeliversEveryEventFromTheCheckpoint(t *testing.T) {
	t.Parallel()

	st := &fakeDeliveryStore{}
	col := &lineCollector{failStatus: http.StatusServiceUnavailable, failFirst: 2}
	srv := col.server(t)

	// First enable happens on an empty trail, so the checkpoint starts at 0
	// and everything appended afterwards is in scope.
	ctx := context.Background()
	if err := st.EnsureAuditDeliveryCursor(ctx, AckedDestination); err != nil {
		t.Fatalf("EnsureAuditDeliveryCursor: %v", err)
	}
	rows := ackedRows(5)
	st.setRows(rows)

	a, err := NewAckedWebhook(WebhookConfig{URL: srv.URL, RetryBaseDelay: "1ms", BatchSize: 2}, st, &fakeRecorder{})
	if err != nil {
		t.Fatalf("NewAckedWebhook: %v", err)
	}
	var stop bool
	for range 8 {
		if _, stop = a.step(ctx); stop {
			t.Fatal("step stopped; an outage is not a terminal refusal")
		}
		if a.cursor.Pos().Seq == rows[4].Seq {
			break
		}
	}

	got := col.snapshot()
	if len(got) != len(rows) {
		t.Fatalf("collector holds %d lines, want all %d: %+v", len(got), len(rows), got)
	}
	for i, l := range got {
		if l.ID != rows[i].ID || l.Seq != rows[i].Seq || l.RowHash != rows[i].RowHash {
			t.Errorf("line %d = %+v, want the row at seq %d (%s)", i, l, rows[i].Seq, rows[i].ID)
		}
	}
	acked, hash, lastErr, halted, _ := st.state()
	if acked != rows[4].Seq || hash != rows[4].RowHash {
		t.Errorf("checkpoint = (%d, %q), want (%d, %q)", acked, hash, rows[4].Seq, rows[4].RowHash)
	}
	if lastErr != "" || halted {
		t.Errorf("checkpoint after recovery = last_error %q halted %v, want both clear", lastErr, halted)
	}
}

// A restart resumes from the stored checkpoint: rows 1..3 were already
// accepted, so only 4 and 5 go out.
func TestAcked_RestartResumesFromTheStoredCheckpoint(t *testing.T) {
	t.Parallel()

	st := &fakeDeliveryStore{haveRow: true, acked: 3, hash: "h3"}
	rows := ackedRows(5)
	st.setRows(rows)
	col := &lineCollector{}
	srv := col.server(t)

	a, err := NewAckedWebhook(WebhookConfig{URL: srv.URL, RetryBaseDelay: "1ms"}, st, &fakeRecorder{})
	if err != nil {
		t.Fatalf("NewAckedWebhook: %v", err)
	}
	if _, stop := a.step(context.Background()); stop {
		t.Fatal("step stopped on a healthy collector")
	}

	got := col.snapshot()
	if len(got) != 2 {
		t.Fatalf("collector holds %d lines, want only the two after the checkpoint: %+v", len(got), got)
	}
	for i, l := range got {
		if want := rows[3+i]; l.ID != want.ID || l.Seq != want.Seq {
			t.Errorf("line %d = seq %d (%s), want seq %d (%s)", i, l.Seq, l.ID, want.Seq, want.ID)
		}
	}
	if acked, _, _, _, _ := st.state(); acked != rows[4].Seq {
		t.Errorf("checkpoint after the restart = %d, want %d", acked, rows[4].Seq)
	}
}

// A terminal 4xx stops the loop, durably halts delivery and records the
// collector's answer — without the bearer token or the URL's password.
func TestAcked_TerminalStatusHaltsAndRecordsTheError(t *testing.T) {
	t.Parallel()

	const token = "tok-123"
	col := &lineCollector{failStatus: http.StatusUnauthorized, failFirst: 10, tls: true}
	srv := col.server(t)
	// A URL that carries a password, as an operator's config might.
	secretURL := strings.Replace(srv.URL, "://", "://u:secret@", 1)

	st := &fakeDeliveryStore{haveRow: true}
	st.setRows(ackedRows(2))

	a, err := NewAckedWebhook(WebhookConfig{URL: secretURL, BearerToken: token, RetryBaseDelay: "1ms"}, st, &fakeRecorder{})
	if err != nil {
		t.Fatalf("NewAckedWebhook: %v", err)
	}
	// The loop builds its own client; the test server's is the one that trusts
	// the httptest certificate, so the POST actually reaches the collector.
	a.client = srv.Client()
	_, stop := a.step(context.Background())
	if !stop {
		t.Fatal("step did not stop on a 401; a definitive refusal must halt the loop")
	}
	_, _, lastErr, halted, _ := st.state()
	if !halted {
		t.Error("the checkpoint is not marked halted; the halt would not survive a restart")
	}
	if !strings.Contains(lastErr, "401") {
		t.Errorf("last_error = %q, want it to name the 401", lastErr)
	}
	for _, secret := range []string{token, "secret"} {
		if strings.Contains(lastErr, secret) {
			t.Errorf("last_error = %q leaks %q", lastErr, secret)
		}
	}
}

// Retention past the checkpoint leaves the row at the cursor gone: the loop
// records audit.delivery.reset so the collector learns of the gap, and
// resends from the oldest retained row.
func TestAcked_GapRecordsAResetEvent(t *testing.T) {
	t.Parallel()

	st := &fakeDeliveryStore{haveRow: true, acked: 2, hash: "h2"}
	rows := ackedRows(5)[2:] // seqs 3,4,5: retention dropped the rows at 1 and 2
	st.setRows(rows)
	col := &lineCollector{}
	srv := col.server(t)
	rec := &fakeRecorder{}

	a, err := NewAckedWebhook(WebhookConfig{URL: srv.URL, RetryBaseDelay: "1ms"}, st, rec)
	if err != nil {
		t.Fatalf("NewAckedWebhook: %v", err)
	}
	if _, stop := a.step(context.Background()); stop {
		t.Fatal("step stopped; a gap is resent, not a refusal")
	}

	if n := rec.count(); n != 1 {
		t.Fatalf("recorder holds %d events, want one audit.delivery.reset", n)
	}
	ev := rec.first()
	if ev.Action != "audit.delivery.reset" || ev.Outcome != "failure" || ev.ActorType != types.ActorSystem || ev.Actor != "wardynd" {
		t.Errorf("reset event = %s %s %s %q, want the delivery reset the docs name", ev.Action, ev.Outcome, ev.ActorType, ev.Actor)
	}
	if ev.Target != AckedDestination {
		t.Errorf("reset event target = %q, want %q", ev.Target, AckedDestination)
	}
	var data struct {
		Destination string `json:"destination"`
		AckedSeq    int64  `json:"acked_seq"`
	}
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		t.Fatalf("reset event data: %v", err)
	}
	if data.Destination != AckedDestination || data.AckedSeq != 2 {
		t.Errorf("reset event data = %+v, want destination %q and the lost acked_seq 2", data, AckedDestination)
	}
	if _, _, _, _, resets := st.state(); resets != 1 {
		t.Errorf("resets = %d, want 1", resets)
	}

	got := col.snapshot()
	if len(got) == 0 || got[0].Seq != rows[0].Seq {
		t.Fatalf("collector holds %+v, want the resend to start at the oldest retained row (seq %d)", got, rows[0].Seq)
	}
	for i, l := range got {
		if l.ID != rows[i].ID {
			t.Errorf("resend line %d = seq %d, want seq %d", i, l.Seq, rows[i].Seq)
		}
	}
}
