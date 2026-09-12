// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	if ev != nil {
		c.srv.recordAudit(c.srv.cfg.BaseCtx, *ev)
	}
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

// TestAuthFailedCoalesce_DistinctPrincipalsAreNotFolded is the bound that keeps
// the fold from hiding an attack: a burst from many peers is many keys, so every
// one of them keeps its own row. (SourceIP is in the key and does NOT separate
// principals behind a Kubernetes ingress — THREAT-MODEL.md says so, and the
// window+count bound is what covers that case.)
func TestAuthFailedCoalesce_DistinctPrincipalsAreNotFolded(t *testing.T) {
	c := newCoalesceHarness(t, 5*time.Minute)
	const principals = 500
	for i := range principals {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/internal/approvals", nil)
		r.RemoteAddr = "198.51.100." + itoa3(i/256) + ":" + itoa3(10000+i%256)
		// One second of clock per attempt so the ~1/sec limiter admits them all:
		// this test is about the COALESCER, and a limiter drop would mask it.
		c.advance(time.Second)
		c.srv.auditAuthFailedAs(r, internalAuthActor, "invalid_run_token")
	}
	c.flush()

	rows := authFailedEvents(c.harness)
	if len(rows) < principals {
		t.Errorf("auth.failed rows = %d for %d DISTINCT peers, want at least one each — a credential-stuffing "+
			"run must not collapse into a single row", len(rows), principals)
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
