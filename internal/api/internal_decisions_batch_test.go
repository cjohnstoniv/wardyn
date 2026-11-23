// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// batchDoorStore counts what the decision door asks of the store: run reads, the database clock, TouchRun
// and the pause stamp. Every other method is the nil embedded store's, so the door must not reach one.
type batchDoorStore struct {
	anyRunLive
	mu                sync.Mutex
	run               types.AgentRun
	getRuns, dbNows   int
	touched, stamped  []uuid.UUID
	getRunErr, nowErr error
}

func (s *batchDoorStore) GetRun(_ context.Context, id uuid.UUID) (types.AgentRun, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.getRuns++
	if s.getRunErr != nil {
		return types.AgentRun{}, s.getRunErr
	}
	run := s.run
	run.ID = id
	if run.State == "" {
		run.State, run.UpdatedAt = types.RunRunning, time.Now().UTC()
	}
	return run, nil
}

func (s *batchDoorStore) DatabaseNow(context.Context) (time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dbNows++
	return time.Now().UTC(), s.nowErr
}

func (s *batchDoorStore) TouchRun(_ context.Context, id uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.touched = append(s.touched, id)
	return nil
}

func (s *batchDoorStore) StampRunActive(_ context.Context, id uuid.UUID) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stamped = append(s.stamped, id)
	return false, nil
}

func (s *batchDoorStore) ListPauseCandidates(context.Context) ([]store.PauseCandidate, time.Time, error) {
	return nil, time.Time{}, nil
}

func (s *batchDoorStore) MarkRunPaused(context.Context, uuid.UUID, types.PauseReason, *time.Time) (bool, error) {
	return false, nil
}

func (s *batchDoorStore) ClearRunPaused(context.Context, uuid.UUID) (bool, error) { return false, nil }

func (s *batchDoorStore) RunHasOpenRequest(context.Context, uuid.UUID) (bool, error) {
	return false, nil
}

var (
	_ store.DatabaseClock = (*batchDoorStore)(nil)
	_ store.RunPauser     = (*batchDoorStore)(nil)
)

// batchCases are the decisions a batch must record exactly as single posts do.
func batchCases() []egress.DecisionLog {
	approval := uuid.New()
	return []egress.DecisionLog{
		{Request: egress.Request{Host: "api.example.com", Port: 443, Method: "POST", Path: "/v1/x"}, Decision: egress.Allow, RuleSource: "policy:allowlist"},
		{Request: egress.Request{Host: "deny.example.com", Port: 443, Method: "GET"}, Decision: egress.Deny, RuleSource: "policy:denied",
			Refusal: &egress.Refusal{By: egress.RefusedByWardyn, Reason: "policy"}},
		{DNS: &egress.DNSQuery{Name: "dns.example.com", Type: "A", Outcome: "answered", Via: "proxy"}},
		{Request: egress.Request{Host: "repeat.example.com", Method: "GET"}, Decision: egress.Deny, RuleSource: "policy:denied", Repeat: 4},
		{Request: egress.Request{Host: "api.anthropic.com", Method: "POST"}, Decision: egress.Allow, RuleSource: "policy:allowlist",
			Scan: &egress.ScanSummary{Scanned: true, Coverage: "inspectable", Mode: "block", Action: "block", Channel: "anthropic.messages"}},
		{Request: egress.Request{Host: "api.anthropic.com", Method: "CONNECT"}, Decision: egress.Allow,
			Scan: &egress.ScanSummary{Coverage: "tunneled-opaque", Action: "bypass"}},
		{Request: egress.Request{Host: "flow.example.com", Method: "CONNECT"}, Decision: egress.Allow, RuleSource: "policy:allowlist", Flow: "vmflow-7"},
		{Request: egress.Request{Host: "bedrock-runtime.us-east-1.amazonaws.com", Method: "POST"}, Decision: egress.Allow, RuleSource: "policy:allowlist",
			UpstreamFault: "throttled"},
		{Request: egress.Request{Host: "gw.example.com", Method: "POST"}, Decision: egress.Allow, RuleSource: "policy:allowlist",
			GatewayCallID: "call-1", GatewayRequestID: "req-1", ApprovalID: &approval},
		{Request: egress.Request{Host: "portal.sso.us-east-1.amazonaws.com", Method: "CONNECT"}, Decision: egress.Deny,
			RuleSource: ruleSourceCredentialReauthTimeout},
	}
}

func batchBody(t testing.TB, at time.Time, dls ...egress.DecisionLog) (string, []uuid.UUID) {
	t.Helper()
	var b proxy.DecisionBatch
	ids := make([]uuid.UUID, len(dls))
	for i, dl := range dls {
		ids[i] = uuid.New()
		b.Records = append(b.Records, proxy.DecisionRecord{ID: ids[i], Time: at, Decision: dl})
	}
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw), ids
}

func batchHeaders(final bool) map[string]string {
	h := map[string]string{"Content-Type": proxy.DecisionBatchContentType}
	if final {
		h[proxy.DecisionFinalHeader] = "1"
	}
	return h
}

// decisionCounters is the deny counter and the reauth-timeout counter.
func decisionCounters(s *Server) (denies, reauth int64) {
	s.metrics.mu.Lock()
	defer s.metrics.mu.Unlock()
	return s.metrics.egressDenies, s.metrics.credentialReauthOutcomes[credentialReauthOutcomeTimeout]
}

// comparable is a row without what differs between two posts of one decision: id and time.
func comparableRows(evs []types.AuditEvent) []types.AuditEvent {
	out := make([]types.AuditEvent, len(evs))
	for i, ev := range evs {
		ev.ID, ev.Time = uuid.Nil, time.Time{}
		out[i] = ev
	}
	return out
}

func newBatchServer(h *harness, st store.Store) (*Server, *recRecorder) {
	rec := &recRecorder{}
	cfg := baseTestConfig(h, st)
	cfg.Audit = rec
	return New(cfg), rec
}

// TestDecisionBatch_RowsMatchSinglePosts: a batch of every kind of decision records the rows, counts and
// touches that the same decisions posted one by one record; a DNS record touches nothing.
func TestDecisionBatch_RowsMatchSinglePosts(t *testing.T) {
	h := newHarness(t)
	runID := uuid.New()
	tok := h.mintRunToken(t, runID)
	stA, stB := &batchDoorStore{}, &batchDoorStore{}
	single, recA := newBatchServer(h, stA)
	batched, recB := newBatchServer(h, stB)
	const path = "/api/v1/internal/decisions"
	for _, dl := range batchCases() {
		raw, _ := json.Marshal(dl)
		if w := doWithHeaders(t, single, http.MethodPost, path, tok, string(raw), nil); w.Code != http.StatusAccepted {
			t.Fatalf("single post %s = %d %s", dl.Request.Host, w.Code, w.Body.String())
		}
	}
	body, _ := batchBody(t, time.Now().UTC(), batchCases()...)
	if w := doWithHeaders(t, batched, http.MethodPost, path, tok, body, batchHeaders(false)); w.Code != http.StatusAccepted {
		t.Fatalf("batch post = %d %s", w.Code, w.Body.String())
	}
	a, b := comparableRows(recA.snapshot()), comparableRows(recB.snapshot())
	if len(a) != len(b) {
		t.Fatalf("single posts wrote %d rows, the batch %d", len(a), len(b))
	}
	for i := range a {
		ja, _ := json.Marshal(a[i])
		jb, _ := json.Marshal(b[i])
		if !bytes.Equal(ja, jb) {
			t.Errorf("row %d differs:\n single %s\n batch  %s", i, ja, jb)
		}
	}
	da, ra := decisionCounters(single)
	db, rb := decisionCounters(batched)
	if da != db || ra != rb || da == 0 || ra == 0 {
		t.Errorf("counters: single denies %d reauth %d, batch denies %d reauth %d; want equal and moved", da, ra, db, rb)
	}
	if len(stA.touched) != len(stB.touched) || len(stA.stamped) != len(stB.stamped) || len(stA.touched) == 0 {
		t.Errorf("touches %d/%d and stamps %d/%d differ, want equal and some", len(stA.touched), len(stB.touched), len(stA.stamped), len(stB.stamped))
	}

	// A DNS record alone touches nothing and stamps nothing.
	dnsOnly, _ := batchBody(t, time.Now().UTC(), batchCases()[2])
	st := &batchDoorStore{}
	srv, _ := newBatchServer(h, st)
	if w := doWithHeaders(t, srv, http.MethodPost, path, tok, dnsOnly, batchHeaders(false)); w.Code != http.StatusAccepted {
		t.Fatalf("DNS batch = %d", w.Code)
	}
	if len(st.touched) != 0 || len(st.stamped) != 0 {
		t.Fatalf("a DNS record touched %v, stamped %v; want neither", st.touched, st.stamped)
	}
}

// TestDecisionBatch_RelayedBatchMatchesRelayedSinglePosts: a batch marked relayed, on a via_org engine's call for
// a VM run, records every row relayed and touches nothing, as the same three relayed single posts do.
func TestDecisionBatch_RelayedBatchMatchesRelayedSinglePosts(t *testing.T) {
	h := newHarness(t)
	runID := uuid.New()
	allow := egress.DecisionLog{Request: egress.Request{Host: "relay.example.com", Method: "POST"}, Decision: egress.Allow, RuleSource: "policy:allowlist"}
	post := func(srv *Server, body string, batch bool) {
		t.Helper()
		ctx := context.WithValue(context.Background(), claimsCtxKey{}, &identity.Claims{RunID: runID, SPIFFEID: "spiffe://wardyn.local/run/" + runID.String()})
		ctx = contextWithLiveRun(ctx, &types.AgentRun{ID: runID, State: types.RunRunning, MachineKind: types.MachineLinuxVM})
		ctx = context.WithValue(ctx, viaOrgCallKey{}, true)
		r := httptest.NewRequest(http.MethodPost, decisionsDoor, strings.NewReader(body)).WithContext(ctx)
		r.Header.Set(proxy.DecisionRelayedHeader, "1")
		if batch {
			r.Header.Set("Content-Type", proxy.DecisionBatchContentType)
		}
		w := httptest.NewRecorder()
		srv.handlePostDecision(w, r)
		if w.Code != http.StatusAccepted {
			t.Fatalf("relayed post = %d %s", w.Code, w.Body.String())
		}
	}
	stA, stB := &batchDoorStore{}, &batchDoorStore{}
	single, recA := newBatchServer(h, stA)
	batched, recB := newBatchServer(h, stB)
	raw, _ := json.Marshal(allow)
	for range 3 {
		post(single, string(raw), false)
	}
	body, _ := batchBody(t, time.Now().UTC(), allow, allow, allow)
	post(batched, body, true)
	a, b := comparableRows(recA.snapshot()), comparableRows(recB.snapshot())
	if len(a) != 3 || len(b) != 3 {
		t.Fatalf("rows: single %d, batch %d; want 3 each", len(a), len(b))
	}
	for i := range a {
		ja, _ := json.Marshal(a[i])
		jb, _ := json.Marshal(b[i])
		if !bytes.Equal(ja, jb) {
			t.Errorf("row %d differs:\n single %s\n batch  %s", i, ja, jb)
		}
	}
	if len(stA.touched)+len(stB.touched)+len(stA.stamped)+len(stB.stamped) != 0 {
		t.Fatalf("relayed decisions touched (%v, %v) or stamped (%v, %v); want none", stA.touched, stB.touched, stA.stamped, stB.stamped)
	}
}

// TestDecisionBatch_ReadsRunOncePerRequest: a 10-record batch reads the run and the database clock no more
// than one single post does.
func TestDecisionBatch_ReadsRunOncePerRequest(t *testing.T) {
	h := newHarness(t)
	capAt := time.Now().Add(time.Hour)
	allow := egress.DecisionLog{Request: egress.Request{Host: "api.example.com", Method: "POST"}, Decision: egress.Allow, RuleSource: "policy:allowlist"}
	reads := func(batch bool) (int, int) {
		t.Helper()
		st := &batchDoorStore{run: types.AgentRun{State: types.RunRunning, UpdatedAt: time.Now().UTC(), LifetimeCapAt: &capAt}}
		srv, _ := newBatchServer(h, st)
		tok := h.mintRunToken(t, uuid.New())
		var w *httptest.ResponseRecorder
		if batch {
			dls := make([]egress.DecisionLog, 10)
			for i := range dls {
				dls[i] = allow
			}
			body, _ := batchBody(t, time.Now().UTC(), dls...)
			w = doWithHeaders(t, srv, http.MethodPost, decisionsDoor, tok, body, batchHeaders(false))
		} else {
			raw, _ := json.Marshal(allow)
			w = doWithHeaders(t, srv, http.MethodPost, decisionsDoor, tok, string(raw), nil)
		}
		if w.Code != http.StatusAccepted {
			t.Fatalf("post = %d %s", w.Code, w.Body.String())
		}
		return st.getRuns, st.dbNows
	}
	sr, sn := reads(false)
	br, bn := reads(true)
	if br != sr || bn != sn || sr == 0 {
		t.Fatalf("a 10-record batch read the run %d times and the clock %d; a single post %d and %d", br, bn, sr, sn)
	}
}

// TestBatchDecisions_OtherRunRefused: a batch writes rows on its token's run only; a run id in the body is
// ignored.
func TestBatchDecisions_OtherRunRefused(t *testing.T) {
	h := newHarness(t)
	runID, other := uuid.New(), uuid.New()
	tok := h.mintRunToken(t, runID)
	srv, rec := newBatchServer(h, &batchDoorStore{})
	body := `{"run_id":"` + other.String() + `","records":[{"id":"` + uuid.NewString() + `","time":"` + time.Now().UTC().Format(time.RFC3339Nano) +
		`","run_id":"` + other.String() + `","decision":{"run_id":"` + other.String() + `","request":{"host":"a.example.com","method":"GET"},"decision":"allow","rule_source":"policy:allowlist"}}]}`
	if w := doWithHeaders(t, srv, http.MethodPost, decisionsDoor, tok, body, batchHeaders(false)); w.Code != http.StatusAccepted {
		t.Fatalf("batch = %d %s", w.Code, w.Body.String())
	}
	rows := rec.snapshot()
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	for _, ev := range rows {
		if ev.RunID == nil || *ev.RunID != runID {
			t.Fatalf("a row on run %v, want only the token's %s", ev.RunID, runID)
		}
	}
}

// TestDecisionDoor_AdvertisesBatchOnEveryAnswer: every answer the decision door gives once the run token was
// read says it takes batches, and how many records.
func TestDecisionDoor_AdvertisesBatchOnEveryAnswer(t *testing.T) {
	h := newHarness(t)
	want := strconv.Itoa(proxy.MaxDecisionBatch)
	allow := `{"request":{"host":"a.example.com","method":"GET"},"decision":"allow","rule_source":"policy:allowlist"}`
	batch, _ := batchBody(t, time.Now().UTC(), egress.DecisionLog{Request: egress.Request{Host: "a.example.com"}, Decision: egress.Allow})
	check := func(name string, w *httptest.ResponseRecorder, code int) {
		t.Helper()
		if w.Code != code {
			t.Errorf("%s = %d %s, want %d", name, w.Code, w.Body.String(), code)
		}
		if got := w.Header().Get(proxy.DecisionBatchHeader); got != want {
			t.Errorf("%s: %s = %q, want %q", name, proxy.DecisionBatchHeader, got, want)
		}
	}
	srv, _ := newBatchServer(h, &batchDoorStore{})
	tok := h.mintRunToken(t, uuid.New())
	check("single 202", doWithHeaders(t, srv, http.MethodPost, decisionsDoor, tok, allow, nil), http.StatusAccepted)
	check("batch 202", doWithHeaders(t, srv, http.MethodPost, decisionsDoor, tok, batch, batchHeaders(false)), http.StatusAccepted)
	check("400", doWithHeaders(t, srv, http.MethodPost, decisionsDoor, tok, `{"records":[]}`, batchHeaders(false)), http.StatusBadRequest)

	cfg := baseTestConfig(h, &batchDoorStore{})
	cfg.Audit = failingAudit{errors.New("store down")}
	down := New(cfg)
	check("handler 503", doWithHeaders(t, down, http.MethodPost, decisionsDoor, tok, allow, nil), http.StatusServiceUnavailable)

	gate, _ := newBatchServer(h, &batchDoorStore{getRunErr: errors.New("db down")})
	check("gate 503", doWithHeaders(t, gate, http.MethodPost, decisionsDoor, tok, allow, nil), http.StatusServiceUnavailable)

	cfg = baseTestConfig(h, &batchDoorStore{})
	cfg.Identity = &expiredIdentity{token: expiredIdentityToken, runID: uuid.New(), expiry: time.Now().Add(-time.Minute)}
	expired := New(cfg)
	check("expired 401", doWithHeaders(t, expired, http.MethodPost, decisionsDoor, expiredIdentityToken, allow, nil), http.StatusUnauthorized)
}

// reportDrops counts the run.report.drop rows on runID.
func reportDrops(rec *recRecorder, runID uuid.UUID) int {
	n := 0
	for _, ev := range rec.snapshot() {
		if ev.Action == runReportLostAction && ev.RunID != nil && *ev.RunID == runID {
			n++
		}
	}
	return n
}

// TestDecisionBatch_ReportDropOnlyOnFinalAttempt: a retryable 503 to a batch the proxy will send again is not
// a lost report; the batch's last attempt, a refused batch and a single post are.
func TestDecisionBatch_ReportDropOnlyOnFinalAttempt(t *testing.T) {
	h := newHarness(t)
	allow := `{"request":{"host":"a.example.com","method":"GET"},"decision":"allow","rule_source":"policy:allowlist"}`
	batch, _ := batchBody(t, time.Now().UTC(), egress.DecisionLog{Request: egress.Request{Host: "a.example.com"}, Decision: egress.Allow})
	for _, tc := range []struct {
		name    string
		body    string
		hdr     map[string]string
		gateErr bool
		code    int
		drops   int
	}{
		{"batch gate 503, not final", batch, batchHeaders(false), true, http.StatusServiceUnavailable, 0},
		{"batch gate 503, final", batch, batchHeaders(true), true, http.StatusServiceUnavailable, 1},
		{"batch 400", `{"records":[{"id":"` + uuid.Nil.String() + `"}]}`, batchHeaders(false), false, http.StatusBadRequest, 1},
		{"single gate 503", allow, nil, true, http.StatusServiceUnavailable, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &batchDoorStore{}
			if tc.gateErr {
				st.getRunErr = errors.New("db down")
			}
			srv, rec := newBatchServer(h, st)
			runID := uuid.New()
			if w := doWithHeaders(t, srv, http.MethodPost, decisionsDoor, h.mintRunToken(t, runID), tc.body, tc.hdr); w.Code != tc.code {
				t.Fatalf("answer = %d %s, want %d", w.Code, w.Body.String(), tc.code)
			}
			if got := reportDrops(rec, runID); got != tc.drops {
				t.Fatalf("run.report.drop rows = %d, want %d", got, tc.drops)
			}
		})
	}
}

// TestDecisionBatchDoor_LogsNoSecrets: the batch door's log lines, for a kept batch, a refused one and one the
// audit chain could not store, carry no token, request data or body.
func TestDecisionBatchDoor_LogsNoSecrets(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	h := newHarness(t)
	runID := uuid.New()
	tok := h.mintRunToken(t, runID)
	const planted = "ghp_plantedsecretinapath12345"
	dl := egress.DecisionLog{Request: egress.Request{Host: "secret.example.com", Method: "POST", Path: "/x?token=" + planted}, Decision: egress.Deny,
		RuleSource: "policy:denied", Scan: &egress.ScanSummary{Scanned: true, Coverage: "inspectable", Action: "alert"}}
	body, _ := batchBody(t, time.Now().UTC(), dl, dl)
	srv, _ := newBatchServer(h, &batchDoorStore{})
	doWithHeaders(t, srv, http.MethodPost, decisionsDoor, tok, body, batchHeaders(false))
	doWithHeaders(t, srv, http.MethodPost, decisionsDoor, tok, `{"records":[{"decision":{"request":{"path":"`+planted+`"}}}]}`, batchHeaders(false))
	cfg := baseTestConfig(h, &batchDoorStore{})
	cfg.Audit = failingAudit{errors.New("store down")}
	doWithHeaders(t, New(cfg), http.MethodPost, decisionsDoor, tok, body, batchHeaders(true))
	if !strings.Contains(logs.String(), "not wholly stored") {
		t.Fatalf("no log line for the batch the chain could not store: %s", logs.String())
	}
	for _, s := range []string{planted, tok, "secret.example.com"} {
		if strings.Contains(logs.String(), s) {
			t.Fatalf("the log carries %q: %s", s, logs.String())
		}
	}
}

// TestDecisionBatchDoc_ContactKeysOnEveryLabelledRow: the door labels the primary row of a batch record decided
// while the proxy could not reach the control plane, whichever row that is (decisionEvents: the DNS row, the
// bypass row or the egress row), so docs/AUDIT-ACTIONS.md names cp_contact and contact_age_s on each of them.
func TestDecisionBatchDoc_ContactKeysOnEveryLabelledRow(t *testing.T) {
	raw, err := os.ReadFile("../../docs/AUDIT-ACTIONS.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"run.dns.query", "llm.scan.*", "egress.*"} {
		prefix := "| `" + action + "` |"
		var row string
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(line, prefix) {
				row = line
				break
			}
		}
		if row == "" {
			t.Fatalf("docs/AUDIT-ACTIONS.md has no %s row", action)
		}
		cols := strings.Split(row, " | ")
		if len(cols) < 3 || !strings.Contains(cols[2], "`cp_contact`") || !strings.Contains(cols[2], "`contact_age_s`") {
			t.Errorf("the %s row's keys do not name cp_contact and contact_age_s, which the door adds to it", action)
		}
	}
}

// TestDecisionBatchDoc_StatesWhereRetriesMiss: docs/AUDIT-ACTIONS.md says that a proxy whose clock is outside the
// queue-time window loses the batch's dedupe (each retry adds the rows again), and that a row spooled on a 503'd
// attempt and drained before the retry reaches no sink and no counter.
func TestDecisionBatchDoc_StatesWhereRetriesMiss(t *testing.T) {
	raw, err := os.ReadFile("../../docs/AUDIT-ACTIONS.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, sentence := range []string{
		"A proxy whose clock is outside that window gets the control plane's time on every row, so each retry of its batch adds the rows again.",
		"stored by the spool's drain before the retry, reaches no sink and no counter",
	} {
		if !strings.Contains(string(raw), sentence) {
			t.Errorf("docs/AUDIT-ACTIONS.md does not state %q", sentence)
		}
	}
}
