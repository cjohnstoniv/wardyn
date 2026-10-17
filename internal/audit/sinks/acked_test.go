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
	"log/slog"
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
	retryAfter string
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
			if c.retryAfter != "" {
				w.Header().Set("Retry-After", c.retryAfter)
			}
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
	rows := ackedRows(5)[3:] // seqs 4,5: retention dropped 1..3, so seq 3 was never delivered
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

// Retention that removed only rows the collector had already accepted (the
// checkpoint row is gone, the next retained row is the very next seq) costs
// nothing: no reset event, no resets bump, and the resend starts at the first
// unacknowledged row.
func TestAcked_GapOfOnlyAcknowledgedRowsIsNotReported(t *testing.T) {
	t.Parallel()

	st := &fakeDeliveryStore{haveRow: true, acked: 2, hash: "h2"}
	rows := ackedRows(5)[2:] // seqs 3,4,5: only rows at or before the checkpoint are gone
	st.setRows(rows)
	col := &lineCollector{}
	srv := col.server(t)
	rec := &fakeRecorder{}

	a, err := NewAckedWebhook(WebhookConfig{URL: srv.URL, RetryBaseDelay: "1ms"}, st, rec)
	if err != nil {
		t.Fatalf("NewAckedWebhook: %v", err)
	}
	if _, stop := a.step(context.Background()); stop {
		t.Fatal("step stopped; a harmless gap is not a refusal")
	}

	if n := rec.count(); n != 0 {
		t.Errorf("recorder holds %d events, want none for a gap that lost nothing", n)
	}
	acked, _, _, _, resets := st.state()
	if resets != 0 {
		t.Errorf("resets = %d, want 0", resets)
	}
	if acked != rows[2].Seq {
		t.Errorf("checkpoint = %d, want %d after delivering the retained rows", acked, rows[2].Seq)
	}
	got := col.snapshot()
	if len(got) != len(rows) || got[0].Seq != rows[0].Seq {
		t.Errorf("collector holds %+v, want the three retained rows from seq %d", got, rows[0].Seq)
	}
}

// A redirect is never followed: net/http would replay the POST as a bodyless
// GET and the final 200 would acknowledge rows the collector never received.
// The 3xx halts the loop and the checkpoint stays put.
func TestAcked_RedirectIsNotFollowedAndHaltsWithoutAdvancing(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var methods []string
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		methods = append(methods, r.Method)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(final.Close)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.URL+"/final", http.StatusMovedPermanently)
	}))
	t.Cleanup(srv.Close)

	st := &fakeDeliveryStore{haveRow: true}
	st.setRows(ackedRows(3))
	a, err := NewAckedWebhook(WebhookConfig{URL: srv.URL + "/ingest", RetryBaseDelay: "1ms"}, st, &fakeRecorder{})
	if err != nil {
		t.Fatalf("NewAckedWebhook: %v", err)
	}
	if _, stop := a.step(context.Background()); !stop {
		t.Fatal("step did not stop on a 301; a redirect must halt the loop")
	}

	acked, _, lastErr, halted, _ := st.state()
	if acked != 0 {
		t.Errorf("checkpoint = %d after a redirect, want 0: the collector received nothing", acked)
	}
	if !halted || !strings.Contains(lastErr, "301") || !strings.Contains(lastErr, "redirect") {
		t.Errorf("halted = %v last_error = %q, want a halt that names the 301 redirect", halted, lastErr)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(methods) != 0 {
		t.Errorf("the redirect target was contacted with %v, want it never followed", methods)
	}
}

// A halt or an error left in the durable row by a previous run clears on the
// first accepted batch after a restart, so the gauge and last_error describe
// the loop that is running now.
func TestAcked_RestartClearsAStoredHaltAndError(t *testing.T) {
	t.Parallel()

	st := &fakeDeliveryStore{haveRow: true, acked: 1, hash: "h1", halted: true, lastErr: "collector answered HTTP 401"}
	st.setRows(ackedRows(3))
	col := &lineCollector{}
	srv := col.server(t)

	a, err := NewAckedWebhook(WebhookConfig{URL: srv.URL, RetryBaseDelay: "1ms"}, st, &fakeRecorder{})
	if err != nil {
		t.Fatalf("NewAckedWebhook: %v", err)
	}
	if _, stop := a.step(context.Background()); stop {
		t.Fatal("step stopped on a healthy collector")
	}
	acked, _, lastErr, halted, _ := st.state()
	if acked != 3 {
		t.Fatalf("checkpoint = %d, want 3: the restart delivered the backlog", acked)
	}
	if halted || lastErr != "" {
		t.Errorf("after a healthy delivery halted = %v last_error = %q, want both clear", halted, lastErr)
	}
}

// The stored error and the log carry the collector's cause, never the URL: a
// query token (many collectors authenticate with ?token=) must not reach
// audit_delivery_cursors or the log.
func TestAcked_TransportErrorDoesNotLeakTheURL(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL + "/hec?token=SECRETQ"
	srv.Close() // nothing listens: the POST fails in the transport

	st := &fakeDeliveryStore{haveRow: true}
	st.setRows(ackedRows(2))
	a, err := NewAckedWebhook(WebhookConfig{URL: url, RetryBaseDelay: "1ms"}, st, &fakeRecorder{})
	if err != nil {
		t.Fatalf("NewAckedWebhook: %v", err)
	}
	if _, stop := a.step(context.Background()); stop {
		t.Fatal("step stopped; an unreachable collector is retried")
	}
	_, _, lastErr, _, _ := st.state()
	if !strings.Contains(lastErr, "collector unreachable") {
		t.Errorf("last_error = %q, want it to say the collector is unreachable", lastErr)
	}
	for name, text := range map[string]string{"last_error": lastErr, "log": logs.String()} {
		for _, secret := range []string{"SECRETQ", "token=", "/hec"} {
			if strings.Contains(text, secret) {
				t.Errorf("%s = %q leaks %q", name, text, secret)
			}
		}
	}
}

// Every leader term builds its own loop: a replica that led, lost the lease
// and leads again must reload the checkpoint another replica advanced, not
// resend from its stale in-memory position and write the durable one backward.
func TestAcked_EachLeaderTermReloadsTheCheckpoint(t *testing.T) {
	t.Parallel()

	st := &fakeDeliveryStore{haveRow: true}
	st.setRows(ackedRows(3))
	col := &lineCollector{}
	srv := col.server(t)
	run, err := AckedTerms(WebhookConfig{URL: srv.URL, FlushInterval: "5ms", RetryBaseDelay: "1ms", BatchSize: 3}, st, &fakeRecorder{})
	if err != nil {
		t.Fatalf("AckedTerms: %v", err)
	}

	term := func(until func() bool) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		done := make(chan struct{})
		go func() { run(ctx); close(done) }()
		for until != nil && !until() && ctx.Err() == nil {
			time.Sleep(time.Millisecond)
		}
		time.Sleep(100 * time.Millisecond) // let a stale loop, if any, act
		cancel()
		<-done
	}
	term(func() bool { a, _, _, _, _ := st.state(); return a == 3 })
	if a, _, _, _, _ := st.state(); a != 3 {
		t.Fatalf("first term left the checkpoint at %d, want 3", a)
	}

	// Another replica led meanwhile and delivered up to 10.
	rows := ackedRows(10)
	st.setRows(rows)
	st.mu.Lock()
	st.acked, st.hash = 10, rows[9].RowHash
	st.mu.Unlock()
	before := len(col.snapshot())

	term(nil)
	if a, _, _, _, _ := st.state(); a != 10 {
		t.Errorf("checkpoint = %d after the second term, want 10: it must not move backward", a)
	}
	if n := len(col.snapshot()); n != before {
		t.Errorf("second term resent %d lines, want none", n-before)
	}
}

// 408 and 429 ask to be retried, so they back off and keep going; a Retry-After
// in seconds lengthens the wait up to the cap; every other 4xx halts.
func TestAcked_RetryClassificationAndRetryAfter(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		status     int
		retryAfter string
		wantStop   bool
		wantWait   time.Duration
	}{
		{"408 retries", http.StatusRequestTimeout, "", false, time.Millisecond},
		{"429 retries", http.StatusTooManyRequests, "", false, time.Millisecond},
		{"503 retries", http.StatusServiceUnavailable, "", false, time.Millisecond},
		{"429 honours Retry-After", http.StatusTooManyRequests, "30", false, 30 * time.Second},
		{"Retry-After is capped", http.StatusServiceUnavailable, "99999", false, deliveryMaxBackoff},
		{"HTTP-date Retry-After is ignored", http.StatusServiceUnavailable, "Wed, 21 Oct 2026 07:28:00 GMT", false, time.Millisecond},
		{"400 halts", http.StatusBadRequest, "", true, 0},
		{"404 halts", http.StatusNotFound, "", true, 0},
		{"422 halts", http.StatusUnprocessableEntity, "", true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			col := &lineCollector{failStatus: tc.status, failFirst: 10, retryAfter: tc.retryAfter}
			srv := col.server(t)
			st := &fakeDeliveryStore{haveRow: true}
			st.setRows(ackedRows(2))
			a, err := NewAckedWebhook(WebhookConfig{URL: srv.URL, RetryBaseDelay: "1ms"}, st, &fakeRecorder{})
			if err != nil {
				t.Fatalf("NewAckedWebhook: %v", err)
			}
			wait, stop := a.step(context.Background())
			if stop != tc.wantStop {
				t.Fatalf("stop = %v, want %v", stop, tc.wantStop)
			}
			if !tc.wantStop && wait != tc.wantWait {
				t.Errorf("wait = %v, want %v", wait, tc.wantWait)
			}
			if acked, _, _, _, _ := st.state(); acked != 0 {
				t.Errorf("checkpoint = %d after a refused batch, want 0", acked)
			}
		})
	}
}

// A non-positive flush_interval or retry_base_delay would spin the loop (poll
// Postgres, or hammer a failing collector, with no pause) and is refused.
func TestAcked_RejectsNonPositiveWaits(t *testing.T) {
	t.Parallel()

	for _, v := range []string{"0s", "-5s"} {
		for name, cfg := range map[string]WebhookConfig{
			"flush_interval":   {URL: "http://siem.internal/in", FlushInterval: v},
			"retry_base_delay": {URL: "http://siem.internal/in", RetryBaseDelay: v},
		} {
			if _, err := NewAckedWebhook(cfg, &fakeDeliveryStore{}, &fakeRecorder{}); err == nil {
				t.Errorf("NewAckedWebhook(%s %q) = nil error, want a refusal", name, v)
			} else if !strings.Contains(err.Error(), name) {
				t.Errorf("NewAckedWebhook(%s %q) = %v, want it to name %s", name, v, err, name)
			}
		}
	}
}

// A benign gap leaves the cursor's memory at zero while the durable checkpoint
// stays at 3. If the next delivery fails and retention then drops the rows after
// the checkpoint, the loop must re-read the checkpoint and report the loss, not
// resend from zero and hide it.
func TestAcked_BenignGapThenOutageStillReportsALaterLoss(t *testing.T) {
	t.Parallel()

	all := ackedRows(7)
	st := &fakeDeliveryStore{haveRow: true, acked: 3, hash: "h3"}
	st.setRows(all[3:5]) // seqs 4,5: only accepted rows 1..3 are gone
	col := &lineCollector{failStatus: http.StatusServiceUnavailable, failFirst: 1}
	srv := col.server(t)
	rec := &fakeRecorder{}

	a, err := NewAckedWebhook(WebhookConfig{URL: srv.URL, RetryBaseDelay: "1ms"}, st, rec)
	if err != nil {
		t.Fatalf("NewAckedWebhook: %v", err)
	}
	ctx := context.Background()
	if _, stop := a.step(ctx); stop {
		t.Fatal("step stopped on a 503")
	}
	if n := rec.count(); n != 0 {
		t.Fatalf("recorder holds %d events after the benign gap, want none", n)
	}

	st.setRows(all[5:]) // retention dropped the unacknowledged 4,5; 6,7 arrived
	if _, stop := a.step(ctx); stop {
		t.Fatal("step stopped on a healthy collector")
	}
	if n := rec.count(); n != 1 {
		t.Errorf("recorder holds %d events, want one audit.delivery.reset for the lost rows", n)
	}
	if _, _, _, _, resets := st.state(); resets != 1 {
		t.Errorf("resets = %d, want 1", resets)
	}
	if acked, _, _, _, _ := st.state(); acked != 7 {
		t.Errorf("checkpoint = %d, want 7", acked)
	}
}

// swapAfterGapCheck replaces the rows once the second read has run (the cursor
// reads the checkpoint row, then reset re-reads it to classify the gap): the
// interleaving of retention running between the gap check and the batch read.
type swapAfterGapCheck struct {
	*fakeDeliveryStore
	after []types.FederatedAuditEvent
	reads int
}

func (s *swapAfterGapCheck) ListAuditEventsAfterSeq(ctx context.Context, seq int64, limit int) ([]types.FederatedAuditEvent, error) {
	rows, err := s.fakeDeliveryStore.ListAuditEventsAfterSeq(ctx, seq, limit)
	if s.reads++; s.reads == 2 {
		s.setRows(s.after)
	}
	return rows, err
}

// A gap that looked benign when checked must still be reported when retention
// drops the first unacknowledged row before the batch is read: the batch that
// is delivered no longer begins at the checkpoint's successor.
func TestAcked_BenignVerdictGoesStaleBeforeTheBatchIsRead(t *testing.T) {
	t.Parallel()

	all := ackedRows(7)
	base := &fakeDeliveryStore{haveRow: true, acked: 3, hash: "h3"}
	// Seqs 4,5 remain: only accepted rows are gone, so the check says benign. Then 4,5 are dropped.
	base.setRows(all[3:5])
	st := &swapAfterGapCheck{fakeDeliveryStore: base, after: all[5:]}
	col := &lineCollector{}
	srv := col.server(t)
	rec := &fakeRecorder{}

	a, err := NewAckedWebhook(WebhookConfig{URL: srv.URL, RetryBaseDelay: "1ms"}, st, rec)
	if err != nil {
		t.Fatalf("NewAckedWebhook: %v", err)
	}
	if _, stop := a.step(context.Background()); stop {
		t.Fatal("step stopped on a healthy collector")
	}
	if n := rec.count(); n != 1 {
		t.Errorf("recorder holds %d events, want one audit.delivery.reset for the lost rows", n)
	}
	if acked, _, _, _, resets := base.state(); resets != 1 || acked != 7 {
		t.Errorf("checkpoint = %d resets = %d, want 7 and 1", acked, resets)
	}
}
