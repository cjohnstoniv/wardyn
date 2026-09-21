// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// B5 — the audit trail flooded itself out. On a real deployment 999 of the last
// 1000 audit rows were `auth.failed` from ONE sidecar retrying a renew the
// control plane would never grant, once a minute, forever; every real security
// event had aged out of the console's window mid-investigation into who held
// admin. The rate limiter did not help and could not: it caps ~1 row/sec, and a
// 1/min drip never trips it. These tests pin the structural half — identical
// consecutive rows fold into their first row plus ONE summary row carrying the
// count — and, just as importantly, the two bounds that stop the fold from
// hiding a real burst.

// coalesceHarness is a server with the coalescer ON and a clock the test drives.
// The window is long enough that the AfterFunc never fires mid-test: every close
// here is explicit, so the assertions are about the fold, not about timing.
type coalesceHarness struct {
	*harness
	now *time.Time
}

func newCoalesceHarness(t *testing.T, window time.Duration) *coalesceHarness {
	t.Helper()
	h := newHarness(t)
	clock := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	h.srv.cfg.AuditCoalesceWindow = window
	h.srv.cfg.Now = func() time.Time { return clock }
	return &coalesceHarness{harness: h, now: &clock}
}

func (c *coalesceHarness) advance(d time.Duration) { *c.now = c.now.Add(d) }

// flush closes the open streak the way the window timer does — same function,
// same BaseCtx rule (a streak closed on the timer has no request in flight, so a
// request context would be cancelled by then and the summary row lost).
func (c *coalesceHarness) flush() {
	c.srv.authFailedStreakMu.Lock()
	ev := c.srv.closeAuthFailedStreakLocked()
	c.srv.authFailedStreakMu.Unlock()
	// recordAuthFailedSummary, not recordAudit: the timer's own close charges the
	// rate limiter (V1-r2-lensS #1), so a harness that bypassed it would be
	// testing an emit path the daemon no longer has.
	c.srv.recordAuthFailedSummary(c.srv.cfg.BaseCtx, ev)
}

// authFailedEvents returns the raw auth.failed events, so a test can read the
// summary row's data fields and its identity (a summary is a NEW row).
func authFailedEvents(h *harness) []types.AuditEvent {
	var out []types.AuditEvent
	for _, ev := range h.audit.events {
		if ev.Action == "auth.failed" {
			out = append(out, ev)
		}
	}
	return out
}

func coalesceData(t *testing.T, ev types.AuditEvent) (reason string, count int, first, last string) {
	t.Helper()
	var d struct {
		Reason    string `json:"reason"`
		Count     int    `json:"count"`
		FirstSeen string `json:"first_seen"`
		LastSeen  string `json:"last_seen"`
	}
	if err := json.Unmarshal(ev.Data, &d); err != nil {
		t.Fatalf("decode auth.failed data: %v", err)
	}
	return d.Reason, d.Count, d.FirstSeen, d.LastSeen
}

// TestAuthFailedCoalesce_SixtyIdenticalRefusalsBecomeTwoRows is the shape of the
// fix: the first refusal is recorded verbatim, the rest are folded, and closing
// the streak appends ONE summary row whose count is the TRUE number of refusals —
// not the number that happened to survive the rate limiter, which is why the
// coalescer sits in front of it.
func TestAuthFailedCoalesce_SixtyIdenticalRefusalsBecomeTwoRows(t *testing.T) {
	c := newCoalesceHarness(t, 5*time.Minute)
	const refusals = 60
	for range refusals {
		do(t, c.srv, http.MethodPost, "/api/v1/internal/approvals", "not-a-real-token", "")
	}
	c.flush()

	rows := authFailedEvents(c.harness)
	if len(rows) != 2 {
		t.Fatalf("auth.failed rows = %d for %d identical refusals, want 2 (the first + one summary)", len(rows), refusals)
	}
	if _, count, _, _ := coalesceData(t, rows[0]); count != 0 {
		t.Errorf("the FIRST row carries count=%d; it must stay the plain row it always was", count)
	}
	reason, count, first, last := coalesceData(t, rows[1])
	if count != refusals {
		t.Errorf("summary count = %d, want %d — the count is the real volume, not the rate-limited remainder", count, refusals)
	}
	if reason != "invalid_run_token" {
		t.Errorf("summary reason = %q, want invalid_run_token", reason)
	}
	if first == "" || last == "" {
		t.Errorf("summary carries first_seen=%q last_seen=%q; both are required to read a streak", first, last)
	}
	// A NEW row, never a mutation of the first: audit_events is append-only and
	// hash-chained (0047), so "update the first row's count" is not an operation
	// this table has.
	if rows[0].ID == rows[1].ID {
		t.Error("the summary reused the first row's id — it must be an append, not an update")
	}
	if rows[1].Actor != rows[0].Actor || rows[1].Target != rows[0].Target || rows[1].SourceIP != rows[0].SourceIP {
		t.Errorf("summary identity drifted from the streak it summarizes: %+v vs %+v", rows[1], rows[0])
	}
}

// TestAuthFailedCoalesce_OneRowPerMinuteStillFolds is the pin that a 30s window
// would fail, and it is the report's own case: the renew loop wrote one row a
// MINUTE (renewRetry = time.Minute), so a window shorter than that closes every
// streak at count:1 and the structural guard never fires on the flood it was
// built for. The window is the maximum GAP between two identical rows.
func TestAuthFailedCoalesce_OneRowPerMinuteStillFolds(t *testing.T) {
	c := newCoalesceHarness(t, 5*time.Minute)
	const refusals = 10
	for i := range refusals {
		if i > 0 {
			c.advance(time.Minute) // the renewer's own cadence
		}
		do(t, c.srv, http.MethodPost, "/api/v1/internal/approvals", "not-a-real-token", "")
	}
	c.flush()

	rows := authFailedEvents(c.harness)
	if len(rows) != 2 {
		t.Fatalf("auth.failed rows = %d for %d refusals a minute apart, want 2 — a window shorter than the "+
			"drip's period folds nothing at all", len(rows), refusals)
	}
	if _, count, _, _ := coalesceData(t, rows[1]); count != refusals {
		t.Errorf("summary count = %d, want %d", count, refusals)
	}
}

// TestAuthFailedCoalesce_DistinctPeersFoldIntoOneCountedStreak: a burst from
// many peers on one path and reason is ONE streak since #347 — the peer is no
// longer in the key, because a caller that rotates its address otherwise wrote a
// row per request. What keeps the burst readable is the summary: its count is
// the real volume, and its peers field says how many addresses it came from,
// saturating at maxAuthFailedPeers so the set a streak holds stays bounded.
func TestAuthFailedCoalesce_DistinctPeersFoldIntoOneCountedStreak(t *testing.T) {
	c := newCoalesceHarness(t, 5*time.Minute)
	const principals = 500
	for i := range principals {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/internal/approvals", nil)
		// One address PER PRINCIPAL; the port varies too, and must not count.
		r.RemoteAddr = "198.51." + itoa3(i/256) + "." + itoa3(i%256) + ":" + itoa3(10000+i%256)
		c.advance(time.Second)
		c.srv.auditAuthFailedAs(r, internalAuthActor, "invalid_run_token")
	}
	c.flush()

	rows := authFailedEvents(c.harness)
	if len(rows) != 2 {
		t.Fatalf("auth.failed rows = %d for %d peers on one path and reason, want 2 (the first + one summary)",
			len(rows), principals)
	}
	if _, count, _, _ := coalesceData(t, rows[1]); count != principals {
		t.Errorf("summary count = %d, want %d", count, principals)
	}
	if peers := coalescePeers(t, rows[1]); peers != maxAuthFailedPeers {
		t.Errorf("summary peers = %d for %d distinct peers, want it saturated at %d", peers, principals, maxAuthFailedPeers)
	}
}

// coalescePeers reads a summary row's distinct-peer count.
func coalescePeers(t *testing.T, ev types.AuditEvent) int {
	t.Helper()
	var d struct {
		Peers int `json:"peers"`
	}
	if err := json.Unmarshal(ev.Data, &d); err != nil {
		t.Fatalf("decode auth.failed data: %v", err)
	}
	return d.Peers
}

// meFromPeer sends a bad-token GET /api/v1/me from peer and requires the 401.
func meFromPeer(t *testing.T, srv *Server, peer string) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	r.RemoteAddr = peer
	r.Header.Set("Authorization", "Bearer not-the-admin-token")
	w := httptest.NewRecorder()
	srv.router.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("bad token from %s = %d, want 401", peer, w.Code)
	}
}

// TestAuthFailedCoalesce_RotatingPeerDripFoldsIntoOneStreak is #347: a drip of
// bad-token requests, one a second for two minutes, spread over ten rotating
// source addresses. Keyed on the peer, every request opened a new streak and
// wrote its own row — 120 rows, as many as the rate limiter allows. Keyed on
// (reason, path), it is one streak: the opening row plus one summary.
func TestAuthFailedCoalesce_RotatingPeerDripFoldsIntoOneStreak(t *testing.T) {
	c := newCoalesceHarness(t, 5*time.Minute)
	const refusals, peers = 120, 10
	for i := range refusals {
		c.advance(time.Second)
		meFromPeer(t, c.srv, "198.51.100."+itoa3(i%peers)+":4000")
	}
	c.flush()

	rows := authFailedEvents(c.harness)
	if len(rows) != 2 {
		t.Fatalf("auth.failed rows = %d for a %d-request drip over %d rotating peers, want 2 (the first + one summary)",
			len(rows), refusals, peers)
	}
	if _, count, _, _ := coalesceData(t, rows[1]); count != refusals {
		t.Errorf("summary count = %d, want %d", count, refusals)
	}
	if got := coalescePeers(t, rows[1]); got != peers {
		t.Errorf("summary peers = %d, want %d", got, peers)
	}
	if rows[1].SourceIP != rows[0].SourceIP {
		t.Errorf("summary source_ip = %q, want the opening peer %q", rows[1].SourceIP, rows[0].SourceIP)
	}
}

// TestAuthFailedCoalesce_BurstFromANewPeerDuringADripIsCounted: folding on
// (reason, path) must not hide a genuine burst that lands inside a slow drip.
// The burst's refusals are in the summary's count and its peer is in the
// summary's peer count.
func TestAuthFailedCoalesce_BurstFromANewPeerDuringADripIsCounted(t *testing.T) {
	c := newCoalesceHarness(t, 5*time.Minute)
	const drip, burst = 10, 20
	for i := range drip {
		c.advance(time.Minute)
		meFromPeer(t, c.srv, "10.0.0.9:5555")
		if i == drip/2 {
			for j := range burst {
				meFromPeer(t, c.srv, "203.0.113.50:"+itoa3(40000+j))
			}
		}
	}
	c.advance(time.Minute) // refill the limiter, so the summary is not refused
	c.flush()

	rows := authFailedEvents(c.harness)
	if len(rows) != 2 {
		t.Fatalf("auth.failed rows = %d, want 2 (the first + one summary)", len(rows))
	}
	if _, count, _, _ := coalesceData(t, rows[1]); count != drip+burst {
		t.Errorf("summary count = %d, want %d — the burst's refusals belong in it", count, drip+burst)
	}
	if got := coalescePeers(t, rows[1]); got != 2 {
		t.Errorf("summary peers = %d, want 2 — the burst's peer must show in the summary", got)
	}
}

// TestAuthFailedCoalesce_StreakClosesAtTheMaxCount: the second bound. A tight
// loop must produce a row every maxAuthFailedStreak refusals rather than one row
// whenever it happens to stop, so no single row can stand for unbounded volume.
func TestAuthFailedCoalesce_StreakClosesAtTheMaxCount(t *testing.T) {
	c := newCoalesceHarness(t, 5*time.Minute)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/internal/approvals", nil)
	for range maxAuthFailedStreak + 5 {
		c.srv.auditAuthFailedAs(r, internalAuthActor, "invalid_run_token")
	}
	rows := authFailedEvents(c.harness)
	var summaries int
	for _, ev := range rows {
		if _, count, _, _ := coalesceData(t, ev); count > 0 {
			summaries++
			if count > maxAuthFailedStreak {
				t.Errorf("a summary row claims count=%d, past the %d cap", count, maxAuthFailedStreak)
			}
		}
	}
	if summaries == 0 {
		t.Errorf("%d refusals in one tight loop produced no summary row at all; the streak must close at the cap "+
			"rather than wait for the loop to stop", maxAuthFailedStreak+5)
	}
}

// TestAuthFailedCoalesce_ExactlyTwoRowsAtTheStreakCap is the cap's arithmetic,
// and it is the pin an if/else at the emit site fails. The capping refusal does
// TWO things at once — it closes the streak (so a summary comes back) and it is
// itself already counted inside that summary — so treating "a summary closed" and
// "this row was absorbed" as alternatives wrote the 1000th refusal verbatim as
// well: counted twice, once as a row and once in the count, and not counted in
// the suppressed series at all.
func TestAuthFailedCoalesce_ExactlyTwoRowsAtTheStreakCap(t *testing.T) {
	c := newCoalesceHarness(t, 5*time.Minute)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/internal/approvals", nil)
	for range maxAuthFailedStreak {
		c.srv.auditAuthFailedAs(r, internalAuthActor, "invalid_run_token")
	}

	rows := authFailedEvents(c.harness)
	if len(rows) != 2 {
		t.Fatalf("auth.failed rows = %d for exactly %d identical refusals, want 2 (the first + the capping "+
			"summary)", len(rows), maxAuthFailedStreak)
	}
	if _, count, _, _ := coalesceData(t, rows[0]); count != 0 {
		t.Errorf("the first row carries count=%d; it is the plain row", count)
	}
	_, count, _, _ := coalesceData(t, rows[1])
	if count != maxAuthFailedStreak {
		t.Errorf("summary count = %d, want %d — the refusal that CLOSED the streak is inside the count, not a "+
			"row of its own", count, maxAuthFailedStreak)
	}
	// The streak is closed, not left open holding the capping row.
	c.srv.authFailedStreakMu.Lock()
	open := c.srv.authFailedStreak
	c.srv.authFailedStreakMu.Unlock()
	if open != nil {
		t.Errorf("a streak is still open at the cap (count=%d); the cap must close it", open.count)
	}
	// And the absorbed refusals are countable, the capping one included.
	body := do(t, c.srv, http.MethodGet, "/metrics", adminToken, "").Body.String()
	if strings.Contains(body, "wardyn_auth_failed_suppressed_total 0\n") {
		t.Error("wardyn_auth_failed_suppressed_total is still 0 after 999 folded refusals; a dropped row must be " +
			"countable whichever mechanism dropped it")
	}
}

// TestAuthFailedCoalesce_DisabledKeepsARowPerRefusal: 0 is off, and off means
// byte-for-byte the pre-0.7.2 behaviour (the rate limiter alone). An operator who
// wants row-per-attempt gets exactly that.
func TestAuthFailedCoalesce_DisabledKeepsARowPerRefusal(t *testing.T) {
	c := newCoalesceHarness(t, 0)
	const refusals = 10
	r := httptest.NewRequest(http.MethodPost, "/api/v1/internal/approvals", nil)
	for range refusals {
		c.advance(time.Second) // keep the limiter out of it
		c.srv.auditAuthFailedAs(r, internalAuthActor, "invalid_run_token")
	}
	rows := authFailedEvents(c.harness)
	if len(rows) != refusals {
		t.Errorf("auth.failed rows = %d with coalescing OFF, want %d — one per refusal", len(rows), refusals)
	}
	for _, ev := range rows {
		if _, count, _, _ := coalesceData(t, ev); count != 0 {
			t.Errorf("a row carries count=%d with coalescing off", count)
		}
	}
}

// itoa3 is a tiny decimal helper (the package's itoa is a single-digit one in the
// proxy package, not here) — kept local so the test needs no strconv import rule
// argument.
func itoa3(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TestAuthFailedCoalesce_ShutdownFlushesTheOpenStreak is V1-D4. A streak closes
// on a DIFFERENT key, on the 1000-cap, or on the 5m gap timer — and on none of
// those at shutdown: httpSrv.Shutdown ran, the sinks were closed, and up to 999
// refusals' count went with the process. The timer could not have saved it
// either, because it records under BaseCtx and BaseCtx is already cancelled by
// then. That is the wardynd rollout B5 exists to instrument losing its volume
// precisely when a deployment restarts.
//
// The CANCELLED BaseCtx here is the whole point of the case: the flush must
// record under context.WithoutCancel(BaseCtx), so the row survives the very
// cancellation that made the timer useless. The summary is a NEW row through the
// ordinary recordAudit path (the append the hash chain requires), never an
// update of the row that opened the streak.
func TestAuthFailedCoalesce_ShutdownFlushesTheOpenStreak(t *testing.T) {
	c := newCoalesceHarness(t, 5*time.Minute)
	baseCtx, cancel := context.WithCancel(context.Background())
	c.srv.cfg.BaseCtx = baseCtx
	const refusals = 30
	for range refusals {
		do(t, c.srv, http.MethodPost, "/api/v1/internal/approvals", "not-a-real-token", "")
	}
	// Shutdown order: the HTTP server has stopped accepting requests and rootCtx
	// (i.e. BaseCtx) is cancelled by the time serveAndShutdown reaches the flush.
	cancel()
	c.srv.FlushAuthFailedStreak()

	rows := authFailedEvents(c.harness)
	if len(rows) != 2 {
		t.Fatalf("auth.failed rows = %d after a shutdown flush of %d identical refusals, want 2 "+
			"(the first + one summary) — a missing summary is %d lost refusals", len(rows), refusals, refusals-1)
	}
	_, count, first, last := coalesceData(t, rows[1])
	if count != refusals {
		t.Errorf("summary count = %d, want %d", count, refusals)
	}
	if first == "" || last == "" {
		t.Errorf("summary carries first_seen=%q last_seen=%q; both are required to read a streak", first, last)
	}
	if rows[0].ID == rows[1].ID {
		t.Error("the summary reused the first row's id — the chain is append-only, so it must be a new row")
	}
	// Idempotent: a second flush has nothing left to close, so a shutdown path
	// that runs it twice cannot double-count.
	c.srv.FlushAuthFailedStreak()
	if got := len(authFailedEvents(c.harness)); got != 2 {
		t.Errorf("a second flush wrote another row (%d total) — closing an already-closed streak must be a no-op", got)
	}
}

// ─── V1-r2-lensS: the summary row is rate-bound, and the key is the peer IP ───

// TestAuthFailedCoalesce_SummaryRowsPayTheRateLimiter is the HIGH of lens S round
// 2, and it is the audit-flood vector the coalescer itself introduced: a streak
// closes on every KEY CHANGE, a closed streak of two or more emits a summary, and
// that summary went straight to recordAudit. So an unauthenticated client on ONE
// keep-alive connection alternating two paths (A,A,B,B,…) minted one UNMETERED
// row per two requests — measured on HEAD, 400 refusals at a single frozen
// instant recorded 204 rows against the limiter's ceiling of 5. A structural
// bound that adds an unbounded emit path is not a bound.
func TestAuthFailedCoalesce_SummaryRowsPayTheRateLimiter(t *testing.T) {
	c := newCoalesceHarness(t, 5*time.Minute) // frozen clock: the bucket never refills
	const refusals = 400
	for i := range refusals {
		path := "/api/v1/runs/a"
		if (i/2)%2 == 1 {
			path = "/api/v1/runs/b" // flip the key every two requests
		}
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.RemoteAddr = "10.0.0.9:5555"
		c.srv.auditAuthFailedAs(r, adminAuthActor, "invalid_admin_token")
	}
	rows := authFailedEvents(c.harness)
	if len(rows) > int(authFailedBurst) {
		t.Fatalf("auth.failed rows = %d for %d refusals at one frozen instant; the limiter's ceiling is %v — "+
			"a summary row must be charged to it exactly like a first row", len(rows), refusals, authFailedBurst)
	}
}

// TestAuthFailedCoalesce_ARefusedSummaryIsDroppedAndCounted is the other half of
// the same fix: a summary the limiter refuses is DROPPED, never recorded, and it
// lands in the same suppressed series every other dropped auth.failed row does —
// so the volume the summary would have carried is still visible to an operator
// alerting on the series, which is what OPERATIONS.md tells them to do.
func TestAuthFailedCoalesce_ARefusedSummaryIsDroppedAndCounted(t *testing.T) {
	c := newCoalesceHarness(t, 5*time.Minute)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/internal/approvals", nil)
	r.RemoteAddr = "10.0.0.9:5555"
	for range 3 {
		c.srv.auditAuthFailedAs(r, internalAuthActor, "invalid_run_token") // 1 row + 2 folded
	}
	// Drain the bucket at the frozen instant, so the streak's close has nothing
	// left to spend.
	for c.srv.authFailedLimiter.allow(*c.now) {
	}
	before := suppressedTotal(t, c.harness)

	c.flush()

	if rows := authFailedEvents(c.harness); len(rows) != 1 {
		t.Fatalf("auth.failed rows = %d after a summary the limiter refused, want 1 (the opening row only) — "+
			"a refused summary must be dropped, not recorded", len(rows))
	}
	if got := suppressedTotal(t, c.harness); got != before+1 {
		t.Errorf("wardyn_auth_failed_suppressed_total = %d after a refused summary, want %d — a dropped row is a "+
			"countable fact whichever mechanism dropped it", got, before+1)
	}
}

// TestAuthFailedCoalesce_OneConnectionPerRequestStillFolds is lens S #3: the key
// held r.RemoteAddr, PORT INCLUDED, and an ephemeral port is a fresh number on
// every TCP connection — so a client without keep-alive (a scanner, curl in a
// loop, an ingress opening a connection per request) landed every refusal under
// its own key and the fold was a no-op on exactly the estates B5's own residual
// paragraph names. Same peer, same everything else, N connections: still the
// first row plus one summary.
func TestAuthFailedCoalesce_OneConnectionPerRequestStillFolds(t *testing.T) {
	c := newCoalesceHarness(t, 5*time.Minute)
	const refusals = 20
	for i := range refusals {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/internal/approvals", nil)
		r.RemoteAddr = "203.0.113.7:" + itoa3(40000+i) // a new connection each time
		c.advance(time.Second)                         // keep the limiter out of it
		c.srv.auditAuthFailedAs(r, internalAuthActor, "invalid_run_token")
	}
	c.flush()

	rows := authFailedEvents(c.harness)
	if len(rows) != 2 {
		t.Fatalf("auth.failed rows = %d for %d refusals from %d connections on ONE peer IP, want 2 "+
			"(the first + one summary) — the ephemeral port must not be in the coalescing key",
			len(rows), refusals, refusals)
	}
	if _, count, _, _ := coalesceData(t, rows[1]); count != refusals {
		t.Errorf("summary count = %d, want %d", count, refusals)
	}
	// The ROW still carries host:port, like every other audit row in the package:
	// only the KEY is port-less.
	if rows[1].SourceIP != rows[0].SourceIP {
		t.Errorf("summary source_ip = %q, the opening row's = %q; the summary must carry the streak's own "+
			"peer address", rows[1].SourceIP, rows[0].SourceIP)
	}
}

// suppressedTotal reads wardyn_auth_failed_suppressed_total off /metrics — the
// only surface the counter has, and the one an operator alerts on.
func suppressedTotal(t *testing.T, h *harness) int {
	t.Helper()
	w := do(t, h.srv, http.MethodGet, "/metrics", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("/metrics = %d, want 200", w.Code)
	}
	m := regexp.MustCompile(`wardyn_auth_failed_suppressed_total (\d+)`).FindStringSubmatch(w.Body.String())
	if m == nil {
		t.Fatalf("/metrics carries no wardyn_auth_failed_suppressed_total value:\n%s", w.Body.String())
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("parse wardyn_auth_failed_suppressed_total %q: %v", m[1], err)
	}
	return n
}
