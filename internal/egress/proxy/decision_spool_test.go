// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/contentscan"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/runnerspool"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

// eachRow checks a replayed batch row by row.
func eachRow(fn func(runnerspool.Record) error) func(context.Context, []runnerspool.Record) error {
	return func(_ context.Context, rows []runnerspool.Record) error {
		for _, r := range rows {
			if err := fn(r); err != nil {
				return err
			}
		}
		return nil
	}
}

// maxReservationPath is a path long enough that its request reserves the
// largest wire record, so the two tests below still fill the spool.
var maxReservationPath = strings.Repeat("x", 100_000)

// An ordinary request reserves its own record's bound, not the largest wire
// record: more than 32 in flight at once are all admitted, and nothing is
// counted as dropped.
func TestLocalSpoolAdmitsMoreThan32OrdinaryRequestsInFlight(t *testing.T) {
	ctx := context.Background()
	s, err := newLocalDecisionSink(filepath.Join(t.TempDir(), "spool"), uuid.New(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close(ctx)
	p := &Proxy{sink: s}
	for i := range 64 {
		w := httptest.NewRecorder()
		if admitted, _ := p.admitSpoolHTTP(w, httptest.NewRequest("GET", "http://allowed.test/x", nil), ""); admitted == nil {
			t.Fatalf("request %d of 64 in flight refused: %d", i+1, w.Code)
		}
	}
	if _, _, drops, err := s.local.Stats(ctx); err != nil || drops != 0 {
		t.Fatalf("drops %d %v", drops, err)
	}
}

// A request that may be content-scanned reserves room for its scanned record:
// 500 findings, each with a 256-byte field path, fit the reservation of an
// inspected LLM call.
func TestScannedRecordFitsItsReservation(t *testing.T) {
	p := &Proxy{scanner: scanEngine(t, "alert", scanTestSecret)}
	r := httptest.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
	findings := make([]egress.ScanFinding, 500)
	for i := range findings {
		findings[i] = egress.ScanFinding{Detector: "corpus", Category: "secret", FieldPath: strings.Repeat("p", 256), Offset: i, Length: 32, Severity: "high", Sample: "[REDACTED]"}
	}
	rec := runnerspool.Record{SpoolID: uuid.New(), Seq: 1, Hash: strings.Repeat("0", 64), Decision: egress.DecisionLog{
		Request:  egress.Request{RunID: uuid.New(), Host: "api.anthropic.com", Port: 443, Method: r.Method, Path: r.URL.Path, Time: time.Now()},
		Decision: egress.Allow, RuleSource: "scan:mitm",
		Scan: &egress.ScanSummary{Scanned: true, Coverage: "inspectable", Mode: "alert", Action: "alert", Channel: "anthropic.messages", Findings: findings, FindingsTotal: 500},
	}}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.spoolReservationBytes(r); int64(len(b)) > got {
		t.Fatalf("a scanned record of %d bytes overruns its reservation of %d", len(b), got)
	}
}

// queryScan is a bodyless GET through an API door whose JSON-shaped query the
// forward scan reads as about 500 findings, each under its own long field path,
// and the scan summary the door attaches to that request's allow.
func queryScan(t *testing.T) (*Proxy, *http.Request, *egress.ScanSummary) {
	t.Helper()
	const secret = "s3cr3t-8" // secretmask.MinLen bytes, so the query stays small
	eng, err := contentscan.NewEngine(types.LLMInspectionSpec{Mode: "alert", DetectSecrets: true, InspectForwardEgress: true}, [][]byte{[]byte(secret)})
	if err != nil || eng == nil {
		t.Fatalf("NewEngine: %v", err)
	}
	query := `{"` + strings.Repeat("k", 240) + `":[` + strings.TrimSuffix(strings.Repeat(`"`+secret+`",`, 500), ",") + `]}`
	r := httptest.NewRequest(http.MethodGet, "https://api.github.com/repos/acme/app/issues?"+url.QueryEscape(query), nil)
	res, _, serr := eng.ScanRequest(contentscan.ChannelGeneric, []byte(query))
	if serr != nil || len(res.Findings) < 400 {
		t.Fatalf("the query scan found %d findings (%v), want about 500", len(res.Findings), serr)
	}
	return &Proxy{scanner: eng}, r, scanSummaryFrom(res, serr, eng, "", contentscan.ChannelGeneric)
}

// A bodyless request is scanned on its query when forward egress is inspected,
// so it reserves room for its findings too.
func TestQueryScannedRecordFitsItsReservation(t *testing.T) {
	p, r, scan := queryScan(t)
	rec := runnerspool.Record{SpoolID: uuid.New(), Seq: 1, Hash: strings.Repeat("0", 64), Decision: egress.DecisionLog{
		Request:  egress.Request{RunID: uuid.New(), Host: "api.github.com", Port: 443, Method: r.Method, Path: r.URL.Path, Time: time.Now()},
		Decision: egress.Allow, RuleSource: "policy:allowed", Scan: scan,
	}}
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.spoolReservationBytes(r); int64(len(b)) > got {
		t.Fatalf("a query-scanned record of %d bytes (%d findings) overruns its reservation of %d", len(b), len(scan.Findings), got)
	}
}

// Near a full spool, query-scanned requests compete for what is left: every one
// admitted keeps its decision, and one that cannot is refused before it is
// forwarded and counted.
func TestQueryScannedDecisionsPersistNearCapacity(t *testing.T) {
	ctx := context.Background()
	id := uuid.New()
	p, r, scan := queryScan(t)
	s, err := newLocalDecisionSink(filepath.Join(t.TempDir(), "spool"), id, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close(ctx)
	p.sink = s
	const filled = 30
	for range filled {
		if err := s.local.Append(ctx, egress.DecisionLog{Request: egress.Request{RunID: id, Host: "allowed.test", Time: time.Now()},
			Decision: egress.Allow, RuleSource: "policy:allowed", Cause: strings.Repeat("x", 1_000_000)}); err != nil {
			t.Fatal(err)
		}
	}
	const asked = 24
	var tickets []uuid.UUID
	for range asked {
		if admitted, _ := p.admitSpoolHTTP(httptest.NewRecorder(), r.Clone(ctx), ""); admitted != nil {
			tickets = append(tickets, spoolTicket(admitted.Context()))
		}
	}
	if len(tickets) == 0 || len(tickets) == asked {
		t.Fatalf("%d of %d admitted: the spool was not near capacity", len(tickets), asked)
	}
	for _, ticket := range tickets {
		s.emit(egress.DecisionLog{Request: egress.Request{RunID: id, Host: "api.github.com", Port: 443, Method: r.Method, Path: r.URL.Path, Time: time.Now(), AuditReservation: ticket},
			Decision: egress.Allow, RuleSource: "policy:allowed", Scan: scan})
	}
	depth, _, drops, err := s.local.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if depth != filled+len(tickets) || drops != uint64(asked-len(tickets)) {
		t.Fatalf("admitted %d of %d: %d of their decisions persisted and %d drops were counted, want %d and %d",
			len(tickets), asked, depth-filled, drops, len(tickets), asked-len(tickets))
	}
}

func TestLocalSpoolReservationsBoundConcurrentForwarding(t *testing.T) {
	// Every refusal of a spool held full by the admitted reservations is counted.
	ctx := context.Background()
	id := uuid.New()
	dir := filepath.Join(t.TempDir(), "spool")
	s, err := newLocalDecisionSink(dir, id, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close(ctx)
	p := &Proxy{sink: s}
	var mu sync.Mutex
	var accepted []*httptest.ResponseRecorder
	var tickets []uuid.UUID
	var releases []func()
	var wg sync.WaitGroup
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := httptest.NewRecorder()
			r := httptest.NewRequest("GET", "http://allowed.test/"+maxReservationPath, nil)
			admitted, release := p.admitSpoolHTTP(w, r, "")
			mu.Lock()
			defer mu.Unlock()
			if admitted != nil {
				tickets = append(tickets, spoolTicket(admitted.Context()))
				releases = append(releases, release)
				accepted = append(accepted, w)
			} else if w.Code != 503 {
				t.Errorf("refusal status%d", w.Code)
			}
		}()
	}
	wg.Wait()
	if len(tickets) == 0 || len(tickets) > runnerspool.MaxBytes/runnerspool.MaxRecordBytes {
		t.Fatalf("capacity over-admitted %d", len(tickets))
	}
	for i, ticket := range tickets {
		s.emit(egress.DecisionLog{Request: egress.Request{RunID: id, Host: "allowed.test", Method: "GET", Time: time.Now(), AuditReservation: ticket}, Decision: egress.Allow, RuleSource: "policy:allowed"})
		releases[i]()
	}
	depth, _, drops, err := s.local.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if depth != len(tickets) || drops != uint64(64-len(tickets)) {
		t.Fatalf("lost admitted tail or refusal count: depth%d admitted%d drops%d", depth, len(tickets), drops)
	}
	first := true
	rows := 0
	if err := s.local.Replay(ctx, eachRow(func(row runnerspool.Record) error {
		if first {
			if row.Decision.RuleSource != "builtin:spool-full" || row.Count != drops {
				t.Fatalf("overflow not first: %+v", row)
			}
			first = false
		} else {
			rows++
		}
		return nil
	})); err != nil {
		t.Fatal(err)
	}
	if rows != len(tickets) {
		t.Fatalf("history rows%d admitted%d", rows, len(tickets))
	}
	w := httptest.NewRecorder()
	r, release := p.admitSpoolHTTP(w, httptest.NewRequest("GET", "http://allowed.test/again", nil), "")
	if r == nil {
		t.Fatal("drain did not reopen admission")
	}
	release()
}

func TestLocalSpoolCountsOnlyAFullSpoolsRefusal(t *testing.T) {
	// A full spool's refusal is counted once. A request whose admission deadline ran out or whose client
	// hung up is refused with a 503 but not counted: a count would put the whole run's egress into
	// overflow, and the next request must still be admitted.
	ctx := context.Background()
	s, err := newLocalDecisionSink(filepath.Join(t.TempDir(), "spool"), uuid.New(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close(ctx)
	p := &Proxy{sink: s}
	drops := func() uint64 {
		t.Helper()
		_, _, d, err := s.local.Stats(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	expired, cancelExpired := context.WithDeadline(ctx, time.Now().Add(-time.Second))
	defer cancelExpired()
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	for name, c := range map[string]context.Context{"deadline": expired, "cancel": cancelled} {
		w := httptest.NewRecorder()
		if admitted, _ := p.admitSpoolHTTP(w, httptest.NewRequest("GET", "http://allowed.test/", nil).WithContext(c), ""); admitted != nil || w.Code != 503 {
			t.Fatalf("%s: admitted %v, status %d, want a 503 refusal", name, admitted != nil, w.Code)
		}
		if d := drops(); d != 0 {
			t.Fatalf("%s: drops %d, want an uncounted refusal", name, d)
		}
	}
	w := httptest.NewRecorder()
	r, release := p.admitSpoolHTTP(w, httptest.NewRequest("GET", "http://allowed.test/next", nil), "")
	if r == nil {
		t.Fatalf("a timed-out or cancelled admission closed admission: status %d", w.Code)
	}
	release()

	s.mu.Lock()
	s.heldBytes = runnerspool.MaxBytes
	s.mu.Unlock()
	w = httptest.NewRecorder()
	short, cancelShort := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancelShort()
	if admitted, _ := p.admitSpoolHTTP(w, httptest.NewRequest("GET", "http://allowed.test/", nil).WithContext(short), ""); admitted != nil || w.Code != 503 {
		t.Fatalf("full spool: admitted %v, status %d, want a 503 refusal", admitted != nil, w.Code)
	}
	if d := drops(); d != 1 {
		t.Fatalf("full spool: drops %d, want the refusal counted once", d)
	}
}

func TestLocalSpoolSingleWriterAndSecretMask(t *testing.T) {
	ctx := context.Background()
	id := uuid.New()
	dir := filepath.Join(t.TempDir(), "spool")
	s, err := newLocalDecisionSink(dir, id, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close(ctx)
	if other, err := newLocalDecisionSink(dir, id, nil); err == nil {
		other.close(ctx)
		t.Fatal("two proxy writers can overcommit reservations")
	}
	secret := "spool-secret-" + uuid.NewString()
	procMask([]byte(secret))
	s.emit(egress.DecisionLog{Request: egress.Request{RunID: id, Host: "example.test", Path: "/" + secret, Time: time.Now()}, Decision: egress.Deny, RuleSource: "policy:denied"})
	rows, err := s.local.After(ctx, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Decision.Request.Path == "/"+secret {
		t.Fatalf("secret retained: %+v", rows)
	}
}

func TestLocalSpoolOverflowStopsActualUpstreamAndDrains(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	id := uuid.New()
	dir := filepath.Join(t.TempDir(), "spool")
	sink, err := newLocalDecisionSink(dir, id, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sink.close(context.Background())
	arrived := make(chan bool, 64)
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrived <- true
		select {
		case <-release:
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	p := newProxy(Options{RunID: id, Policy: CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{"allowed.test"}}), Sink: sink, Resolver: publicResolver{}, Dial: redirectDial(upstreamAddr(upstream))})
	var wg sync.WaitGroup
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := httptest.NewRecorder()
			p.ServeHTTP(w, httptest.NewRequest("GET", "http://allowed.test/"+maxReservationPath, nil).WithContext(ctx))
			if w.Code == 503 {
				arrived <- false
			} else if w.Code != 200 {
				t.Errorf("unexpected status %d: %s", w.Code, w.Body.String())
				arrived <- false
			}
		}()
	}
	forwards := 0
	for range 64 {
		select {
		case allowed := <-arrived:
			if allowed {
				forwards++
			}
		case <-ctx.Done():
			close(release)
			wg.Wait()
			t.Fatal("requests did not reach upstream or fail closed")
		}
	}
	close(release)
	wg.Wait()
	if forwards == 0 || forwards > runnerspool.MaxBytes/runnerspool.MaxRecordBytes {
		t.Fatalf("forwarded beyond reserved capacity: %d", forwards)
	}
	depth, _, drops, err := sink.local.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if depth != forwards || drops != uint64(64-forwards) {
		t.Fatalf("network/audit mismatch: forward%d records%d drops%d", forwards, depth, drops)
	}
	records := 0
	if err := sink.local.Replay(ctx, eachRow(func(r runnerspool.Record) error {
		if records == 0 && r.Decision.RuleSource != "builtin:spool-full" {
			t.Fatal("overflow was not first")
		}
		records++
		return nil
	})); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	p.ServeHTTP(w, httptest.NewRequest("GET", "http://allowed.test/drained", nil).WithContext(ctx))
	if w.Code != 200 {
		t.Fatalf("drained status%d", w.Code)
	}
	// A corrupt durable manifest must refuse before any upstream request.
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	p.ServeHTTP(w, httptest.NewRequest("GET", "http://allowed.test/corrupt", nil).WithContext(ctx))
	if w.Code != 503 {
		t.Fatalf("corrupt storage forwarded: %d", w.Code)
	}
	if len(arrived) != 1 {
		t.Fatalf("corrupt storage reached upstream: arrivals%d", len(arrived))
	}
}

// On a runner the proxy-only spool replaces the run-token decisions POST: a
// configured spool directory must never select the control-plane sink.
func TestConfiguredSpoolNeverPostsDecisionsWithTheRunToken(t *testing.T) {
	ctx := context.Background()
	var posts sync.Map
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Store(r.URL.Path, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusNoContent)
	}))
	defer cp.Close()
	id := uuid.New()
	cfg := &Config{RunID: id, ControlPlaneURL: cp.URL, DecisionSpoolDir: filepath.Join(t.TempDir(), "spool"), DecisionBufferSize: 4}
	s, err := configuredDecisionSink(cfg, newTokenSource("run-tok"), cp.Client(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.local == nil {
		s.close(ctx)
		t.Fatal("a configured runner spool selected the run-token decisions sink")
	}
	s.emit(egress.DecisionLog{Request: egress.Request{RunID: id, Host: "example.test", Time: time.Now()}, Decision: egress.Allow, RuleSource: "policy:allowed"})
	rows, err := s.local.After(ctx, 0, 2)
	if cerr := s.close(ctx); cerr != nil {
		t.Fatal(cerr)
	}
	if err != nil || len(rows) != 1 {
		t.Fatalf("decision not spooled: %d rows %v", len(rows), err)
	}
	posts.Range(func(path, auth any) bool {
		t.Fatalf("decision posted to %v with %v", path, auth)
		return false
	})
}
