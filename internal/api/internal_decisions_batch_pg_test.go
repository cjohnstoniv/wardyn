// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The batch decision door against a real Postgres: a retried batch adds no row twice and counts a row once, a
// refused or spooled row is kept and counted on the retry, the time window and the run scope bound the
// dedupe, and a spooled batch drains through the single replay. Guarded by WARDYN_TEST_PG.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// pgBatchChain stands in for the serving chain's lower stages over a real store: a seal stage that can refuse a
// row or hold it pending (to the spool) on its first attempt only, a store write under one lock, and a sink
// that, as the fanout does, gets each appended row with its hashes at once and a row the store failed only
// when the door releases it.
type pgBatchChain struct {
	pool *pgxpool.Pool
	mu   sync.Mutex
	// refuseOnce and pendingOnce name ids the seal stage refuses, or holds pending, on their first attempt.
	refuseOnce, pendingOnce map[uuid.UUID]bool
	failStore               bool
	spooled, sunk           []types.AuditEvent
	held                    [][]bool
}

func (c *pgBatchChain) Record(ctx context.Context, ev types.AuditEvent) error {
	return c.RecordBatch(ctx, []types.AuditEvent{ev})
}

func (c *pgBatchChain) RecordBatch(ctx context.Context, evs []types.AuditEvent) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var refused error
	below := make([]*types.AuditEvent, 0, len(evs))
	for _, ev := range evs {
		switch {
		case c.refuseOnce[ev.ID]:
			delete(c.refuseOnce, ev.ID)
			audit.NoteBatch(ctx, ev.ID, audit.BatchRefused)
			refused = errors.New("seal refused")
		case c.pendingOnce[ev.ID]:
			delete(c.pendingOnce, ev.ID)
			c.spooled = append(c.spooled, ev)
			audit.NoteBatch(ctx, ev.ID, audit.BatchSpooled)
		default:
			ev := ev
			below = append(below, &ev)
		}
	}
	if len(below) > 0 {
		if c.failStore {
			for _, ev := range below {
				c.spooled = append(c.spooled, *ev)
				audit.NoteBatch(ctx, ev.ID, audit.BatchSpooled)
				ev := *ev
				audit.DeferEmit(ctx, func(context.Context) { c.sunk = append(c.sunk, ev) })
			}
			return cmpFirst(refused, fmt.Errorf("%w: store down", audit.ErrSpooled))
		}
		held, err := store.InsertAuditEvents(ctx, c.pool, below)
		if err != nil {
			return err
		}
		c.held = append(c.held, held)
		for i, ev := range below {
			if held[i] {
				audit.NoteBatch(ctx, ev.ID, audit.BatchHeld)
				continue
			}
			audit.NoteBatch(ctx, ev.ID, audit.BatchAppended)
			c.sunk = append(c.sunk, *ev)
		}
	}
	return refused
}

func cmpFirst(a, b error) error {
	if a != nil {
		return a
	}
	return b
}

// pgDoor is a server whose decision door records into chain, and a token for run.
func pgDoor(t *testing.T, chain *pgBatchChain) (*Server, *recRecorder, string, uuid.UUID) {
	t.Helper()
	h := newHarness(t)
	runID := uuid.New()
	srv, rec := newBatchServer(h, &batchDoorStore{})
	srv.cfg.Audit = chain
	return srv, rec, h.mintRunToken(t, runID), runID
}

// rowsByID counts the stored rows of each id.
func rowsByID(t *testing.T, pool *pgxpool.Pool, ids ...uuid.UUID) map[uuid.UUID]int {
	t.Helper()
	out := map[uuid.UUID]int{}
	rows, err := pool.Query(context.Background(), `SELECT id FROM audit_events WHERE id = ANY($1)`, ids)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out[id]++
	}
	return out
}

func scanRowID(id uuid.UUID) uuid.UUID { return uuid.NewSHA1(id, []byte("llm.scan")) }

func postDecisionBatch(t *testing.T, srv *Server, tok, body string, final bool) int {
	t.Helper()
	return doWithHeaders(t, srv, http.MethodPost, decisionsDoor, tok, body, batchHeaders(final)).Code
}

func retryCases() []egress.DecisionLog {
	return []egress.DecisionLog{
		{Request: egress.Request{Host: "a.example.com", Method: "GET"}, Decision: egress.Allow, RuleSource: "policy:allowlist"},
		{Request: egress.Request{Host: "deny.example.com", Method: "GET"}, Decision: egress.Deny, RuleSource: "policy:denied"},
		{Request: egress.Request{Host: "api.anthropic.com", Method: "POST"}, Decision: egress.Allow, RuleSource: "policy:allowlist",
			Scan: &egress.ScanSummary{Scanned: true, Coverage: "inspectable", Action: "alert"}},
		{Request: egress.Request{Host: "portal.sso.us-east-1.amazonaws.com", Method: "CONNECT"}, Decision: egress.Deny,
			RuleSource: ruleSourceCredentialReauthTimeout},
	}
}

// TestBatchDecisions_RetryIsIdempotent_PG: the same batch, an allow, a policy deny and a scanned record, posted
// twice stores every row once, the scan row included, and the second post's rows are held with the hashes the
// first stored.
func TestBatchDecisions_RetryIsIdempotent_PG(t *testing.T) {
	chain := &pgBatchChain{pool: throwawayBarePGPool(t)}
	srv, _, tok, _ := pgDoor(t, chain)
	body, ids := batchBody(t, time.Now().UTC(), retryCases()...)
	for attempt := range 2 {
		if code := postDecisionBatch(t, srv, tok, body, false); code != http.StatusAccepted {
			t.Fatalf("attempt %d = %d", attempt+1, code)
		}
	}
	all := append(slices.Clone(ids), scanRowID(ids[2]))
	got := rowsByID(t, chain.pool, all...)
	for _, id := range all {
		if got[id] != 1 {
			t.Fatalf("id %s stored %d times, want once", id, got[id])
		}
	}
	if len(chain.held) != 2 || slices.Contains(chain.held[0], true) || slices.Contains(chain.held[1], false) {
		t.Fatalf("held = %v, want the first post appended and the second held", chain.held)
	}
	if len(chain.sunk) != len(all) {
		t.Fatalf("sink got %d rows, want each of %d once", len(chain.sunk), len(all))
	}
	var hashes int
	if err := chain.pool.QueryRow(t.Context(), `SELECT count(DISTINCT row_hash) FROM audit_events WHERE id = ANY($1)`, all).Scan(&hashes); err != nil || hashes != len(all) {
		t.Fatalf("distinct stored hashes = %d (%v), want %d", hashes, err, len(all))
	}
	if st, err := store.NewPG(chain.pool).VerifyAuditChain(t.Context()); err != nil || !st.OK {
		t.Fatalf("VerifyAuditChain = %+v, %v", st, err)
	}
}

// TestBatchDecisions_TimeWindowAndRunScope_PG: a queue time inside the window is the row's time and dedupes;
// one outside it is replaced by the arrival time and does not; a stored id of another run does not hold this
// run's row; and a spooled batch drains through the single replay, after which a retry holds every row.
func TestBatchDecisions_TimeWindowAndRunScope_PG(t *testing.T) {
	pool := throwawayBarePGPool(t)
	chain := &pgBatchChain{pool: pool}
	srv, _, tok, runID := pgDoor(t, chain)
	ctx := t.Context()
	allow := retryCases()[0]

	inside := time.Now().UTC().Add(-5 * time.Minute).Truncate(time.Microsecond)
	body, ids := batchBody(t, inside, allow)
	for range 2 {
		if code := postDecisionBatch(t, srv, tok, body, false); code != http.StatusAccepted {
			t.Fatalf("inside the window = %d", code)
		}
	}
	var at time.Time
	if err := pool.QueryRow(ctx, `SELECT time FROM audit_events WHERE id = $1`, ids[0]).Scan(&at); err != nil || !at.Equal(inside) {
		t.Fatalf("row time %v (%v), want the queue time %v", at, err, inside)
	}
	if n := rowsByID(t, pool, ids[0])[ids[0]]; n != 1 {
		t.Fatalf("a retried row inside the window stored %d times, want once", n)
	}

	for _, off := range []time.Duration{-11 * time.Minute, 6 * time.Minute} {
		body, ids := batchBody(t, time.Now().UTC().Add(off), allow)
		before := time.Now().UTC()
		for range 2 {
			if code := postDecisionBatch(t, srv, tok, body, false); code != http.StatusAccepted {
				t.Fatalf("outside the window (%s) = %d", off, code)
			}
		}
		if n := rowsByID(t, pool, ids[0])[ids[0]]; n != 2 {
			t.Fatalf("a queue time %s off: stored %d times, want twice (no dedupe outside the window)", off, n)
		}
		if err := pool.QueryRow(ctx, `SELECT min(time) FROM audit_events WHERE id = $1`, ids[0]).Scan(&at); err != nil || at.Before(before.Add(-time.Second)) {
			t.Fatalf("a queue time %s off: row time %v, want the arrival time", off, at)
		}
	}

	// A stored row of another run with this record's id and time holds nothing here.
	other := uuid.New()
	shared := uuid.New()
	foreign := types.AuditEvent{ID: shared, Time: inside, RunID: &other, ActorType: types.ActorAgent, Actor: "x", Action: "egress.allow", Outcome: "success"}
	if err := store.InsertAuditEvent(ctx, pool, &foreign); err != nil {
		t.Fatal(err)
	}
	scoped := fmt.Sprintf(`{"records":[{"id":%q,"time":%q,"decision":{"request":{"host":"a.example.com"},"decision":"allow"}}]}`, shared, inside.Format(time.RFC3339Nano))
	if code := postDecisionBatch(t, srv, tok, scoped, false); code != http.StatusAccepted {
		t.Fatalf("scoped = %d", code)
	}
	var mine int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE id = $1 AND run_id = $2`, shared, runID).Scan(&mine); err != nil || mine != 1 {
		t.Fatalf("this run's row = %d (%v), want appended despite the other run's", mine, err)
	}

	// A batch the store could not take is spooled; the spool drains it through the single replay, and the
	// retry then finds every row stored.
	spool, err := NewAuditSpool(filepath.Join(t.TempDir(), "audit-spool.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	chain.failStore = true
	body, ids = batchBody(t, time.Now().UTC(), retryCases()...)
	if code := postDecisionBatch(t, srv, tok, body, false); code != http.StatusAccepted {
		t.Fatalf("a spooled batch = %d, want 202", code)
	}
	for _, ev := range chain.spooled {
		if err := spool.Append(ev); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := spool.Drain(ctx, store.Recorder{Pool: pool}, 100); err != nil || n != len(chain.spooled) {
		t.Fatalf("drain = %d, %v; want %d", n, err, len(chain.spooled))
	}
	chain.failStore, chain.held = false, nil
	if code := postDecisionBatch(t, srv, tok, body, false); code != http.StatusAccepted {
		t.Fatalf("the retry after the drain = %d", code)
	}
	if len(chain.held) != 1 || slices.Contains(chain.held[0], false) {
		t.Fatalf("held after the drain = %v, want every row held", chain.held)
	}
	for id, n := range rowsByID(t, pool, append(ids, scanRowID(ids[2]))...) {
		if n != 1 {
			t.Fatalf("id %s stored %d times after drain and retry, want once", id, n)
		}
	}
}

// TestBatchDecisions_RefusedRowRetried_PG: a row the seal stage refuses once makes the first post a 503; the
// retry appends only that row, and every decision is counted once over both posts.
func TestBatchDecisions_RefusedRowRetried_PG(t *testing.T) {
	chain := &pgBatchChain{pool: throwawayBarePGPool(t)}
	srv, _, tok, _ := pgDoor(t, chain)
	deny := retryCases()[1]
	body, ids := batchBody(t, time.Now().UTC(), deny, deny, deny)
	chain.refuseOnce = map[uuid.UUID]bool{ids[1]: true}
	if code := postDecisionBatch(t, srv, tok, body, false); code != http.StatusServiceUnavailable {
		t.Fatalf("first post = %d, want 503", code)
	}
	if code := postDecisionBatch(t, srv, tok, body, false); code != http.StatusAccepted {
		t.Fatalf("retry = %d, want 202", code)
	}
	if len(chain.held) != 2 || !slices.Equal(chain.held[1], []bool{true, false, true}) {
		t.Fatalf("held = %v, want the retry to append only the refused row", chain.held)
	}
	for id, n := range rowsByID(t, chain.pool, ids...) {
		if n != 1 {
			t.Fatalf("id %s stored %d times, want once", id, n)
		}
	}
	if denies, _ := decisionCounters(srv); denies != 3 {
		t.Fatalf("deny counter = %d over both posts, want 3", denies)
	}
}

// TestBatchDecisions_RetryCountsOnce_PG: a batch whose 202 was lost and that is posted again counts each
// decision once, as one post would, and notes no lost report.
func TestBatchDecisions_RetryCountsOnce_PG(t *testing.T) {
	chain := &pgBatchChain{pool: throwawayBarePGPool(t)}
	srv, rec, tok, runID := pgDoor(t, chain)
	body, _ := batchBody(t, time.Now().UTC(), retryCases()...)
	if code := postDecisionBatch(t, srv, tok, body, false); code != http.StatusAccepted {
		t.Fatal(code)
	}
	d1, r1 := decisionCounters(srv)
	if code := postDecisionBatch(t, srv, tok, body, true); code != http.StatusAccepted {
		t.Fatal(code)
	}
	d2, r2 := decisionCounters(srv)
	if d1 != 1 || r1 != 1 || d2 != d1 || r2 != r1 {
		t.Fatalf("counters after one post (%d, %d) and after the retry (%d, %d), want (1, 1) both times", d1, r1, d2, r2)
	}
	if n := reportDrops(rec, runID); n != 0 {
		t.Fatalf("run.report.drop rows = %d, want none", n)
	}
	if n := reportDropsIn(t, chain.pool, runID); n != 0 {
		t.Fatalf("stored run.report.drop rows = %d, want none", n)
	}
}

func reportDropsIn(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE action = $1 AND run_id = $2`, runReportLostAction, runID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestBatchDecisions_SpooledAndRefusedRetryCountsOnce_PG: a row held pending and a row refused on the first post
// make it a 503 that counts and sends nothing; the retry answers 202, each decision is counted once, the sink
// gets each row once, the table holds each once and the spool the pending row's first copy.
func TestBatchDecisions_SpooledAndRefusedRetryCountsOnce_PG(t *testing.T) {
	chain := &pgBatchChain{pool: throwawayBarePGPool(t)}
	srv, _, tok, _ := pgDoor(t, chain)
	deny := retryCases()[1]
	body, ids := batchBody(t, time.Now().UTC(), deny, deny)
	chain.pendingOnce = map[uuid.UUID]bool{ids[0]: true}
	chain.refuseOnce = map[uuid.UUID]bool{ids[1]: true}
	if code := postDecisionBatch(t, srv, tok, body, false); code != http.StatusServiceUnavailable {
		t.Fatalf("first post = %d, want 503", code)
	}
	if d, _ := decisionCounters(srv); d != 0 || len(chain.sunk) != 0 {
		t.Fatalf("after the 503: deny counter %d, sink rows %d; want none", d, len(chain.sunk))
	}
	if code := postDecisionBatch(t, srv, tok, body, false); code != http.StatusAccepted {
		t.Fatalf("retry = %d, want 202", code)
	}
	if d, _ := decisionCounters(srv); d != 2 {
		t.Fatalf("deny counter = %d, want each decision once", d)
	}
	if len(chain.sunk) != 2 || chain.sunk[0].ID == chain.sunk[1].ID {
		t.Fatalf("sink rows = %d, want each row once", len(chain.sunk))
	}
	for id, n := range rowsByID(t, chain.pool, ids...) {
		if n != 1 {
			t.Fatalf("id %s stored %d times, want once", id, n)
		}
	}
	if len(chain.spooled) != 1 || chain.spooled[0].ID != ids[0] {
		t.Fatalf("spool = %d rows, want the pending row's first copy", len(chain.spooled))
	}
}

// benchPGPool is a migrated throwaway database for a benchmark.
func benchPGPool(tb testing.TB) *pgxpool.Pool {
	tb.Helper()
	dsn := os.Getenv("WARDYN_TEST_PG")
	if dsn == "" {
		tb.Skip("WARDYN_TEST_PG not set")
	}
	ctx := context.Background()
	admin, err := db.Connect(ctx, dsn)
	if err != nil {
		tb.Fatal(err)
	}
	name := "wardyn_bench_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(ctx, `CREATE DATABASE `+name); err != nil {
		admin.Close()
		tb.Fatal(err)
	}
	tb.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname=$1 AND pid <> pg_backend_pid()`, name)
		_, _ = admin.Exec(context.Background(), `DROP DATABASE IF EXISTS `+name)
		admin.Close()
	})
	u, err := url.Parse(dsn)
	if err != nil {
		tb.Fatal(err)
	}
	u.Path = "/" + name
	pool, err := db.Connect(ctx, u.String())
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(pool.Close)
	if err := db.Migrate(ctx, pool); err != nil {
		tb.Fatal(err)
	}
	return pool
}

// timedStore is the store recorder with each append call's duration kept: slot, lock wait, appends and commit.
type timedStore struct {
	rec store.Recorder
	mu  sync.Mutex
	d   []time.Duration
}

func (s *timedStore) note(start time.Time) {
	s.mu.Lock()
	s.d = append(s.d, time.Since(start))
	s.mu.Unlock()
}

func (s *timedStore) Record(ctx context.Context, ev types.AuditEvent) error {
	defer s.note(time.Now())
	return s.rec.Record(ctx, ev)
}

func (s *timedStore) RecordBatch(ctx context.Context, evs []types.AuditEvent) error {
	defer s.note(time.Now())
	return s.rec.RecordBatch(ctx, evs)
}

func p99(d []time.Duration) time.Duration {
	if len(d) == 0 {
		return 0
	}
	slices.Sort(d)
	return d[(len(d)*99)/100]
}

// BenchmarkDecisionDoor_SingleVsBatch_PG: 5,000 decisions from four proxies, posted one by one and in batches of
// 128. It reports rows/s, the p99 of one append call (which bounds the chain lock's wait), the pool's acquire
// time over the run, and the p99 of GET /api/v1/runs issued every 50 ms against the same pool.
func BenchmarkDecisionDoor_SingleVsBatch_PG(b *testing.B) {
	const total, proxies = 5000, 4
	dl := egress.DecisionLog{Request: egress.Request{Host: "api.example.com", Method: "POST", Path: "/v1"}, Decision: egress.Allow, RuleSource: "policy:allowlist"}
	for _, mode := range []string{"single", "batch"} {
		b.Run(mode, func(b *testing.B) {
			for range b.N {
				pool := benchPGPool(b)
				ts := &timedStore{rec: store.Recorder{Pool: pool}}
				srv := New(Config{Audit: ts, Store: store.NewPG(pool), AdminToken: adminToken, TrustDomain: "wardyn.local",
					ControlPlaneURL: "http://wardynd:8080", BaseCtx: context.Background()})
				listHandler := panicFails(b, srv.Handler())
				stop := make(chan struct{})
				var listD []time.Duration
				var lw sync.WaitGroup
				lw.Add(1)
				go func() {
					defer lw.Done()
					tick := time.NewTicker(50 * time.Millisecond)
					defer tick.Stop()
					for {
						select {
						case <-stop:
							return
						case <-tick.C:
							r := httptest.NewRequest(http.MethodGet, "/api/v1/runs", nil)
							r.Header.Set("Authorization", "Bearer "+adminToken)
							w := httptest.NewRecorder()
							start := time.Now()
							listHandler.ServeHTTP(w, r)
							listD = append(listD, time.Since(start))
						}
					}
				}()
				before := pool.Stat().AcquireDuration()
				start := time.Now()
				var wg sync.WaitGroup
				for p := range proxies {
					wg.Add(1)
					go func() {
						defer wg.Done()
						runID := uuid.New()
						ctx := context.WithValue(context.Background(), claimsCtxKey{}, &identity.Claims{RunID: runID, SPIFFEID: fmt.Sprintf("spiffe://wardyn.local/run/%s", runID)})
						n := total / proxies
						if p == proxies-1 {
							n = total - (proxies-1)*(total/proxies)
						}
						post := func(body string, batch bool) {
							r := httptest.NewRequest(http.MethodPost, decisionsDoor, strings.NewReader(body)).WithContext(ctx)
							if batch {
								r.Header.Set("Content-Type", proxy.DecisionBatchContentType)
							}
							w := httptest.NewRecorder()
							srv.handlePostDecision(w, r)
							if w.Code != http.StatusAccepted {
								b.Errorf("post = %d %s", w.Code, w.Body.String())
							}
						}
						if mode == "single" {
							raw := fmt.Sprintf(`{"request":{"host":%q,"method":"POST","path":"/v1"},"decision":"allow","rule_source":"policy:allowlist"}`, dl.Request.Host)
							for range n {
								post(raw, false)
							}
							return
						}
						for n > 0 {
							k := min(n, 128)
							dls := make([]egress.DecisionLog, k)
							for i := range dls {
								dls[i] = dl
							}
							body, _ := batchBody(b, time.Now().UTC(), dls...)
							post(body, true)
							n -= k
						}
					}()
				}
				wg.Wait()
				elapsed := time.Since(start)
				close(stop)
				lw.Wait()
				var rows int
				if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_events WHERE action = 'egress.allow'`).Scan(&rows); err != nil || rows != total {
					b.Fatalf("stored %d rows (%v), want %d", rows, err, total)
				}
				rps := float64(total) / elapsed.Seconds()
				b.ReportMetric(rps, "rows/s")
				b.ReportMetric(float64(p99(ts.d).Microseconds()), "append-p99-us")
				b.ReportMetric(float64((pool.Stat().AcquireDuration() - before).Milliseconds()), "acquire-ms")
				b.ReportMetric(float64(p99(listD).Microseconds()), "runs-get-p99-us")
				b.Logf("%s: %d rows in %s: %.0f rows/s; append p99 %s; pool acquire %s; GET /api/v1/runs p99 %s over %d reads",
					mode, total, elapsed, rps, p99(ts.d), pool.Stat().AcquireDuration()-before, p99(listD), len(listD))
			}
		})
	}
}
