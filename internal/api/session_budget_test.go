// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// testBudget is a session budget of n with retry between asks, empty windows and no site read.
func testBudget(s *Server, n, retry time.Duration) *sessionBudget {
	return &sessionBudget{s: s, ctx: context.Background(), id: sessionWatchID{Run: uuid.New(), Actor: types.ActorHuman, Principal: "alice", Surface: sessionSurfaceTerminal},
		n: n, retry: retry, failing: map[string]error{}, seen: map[string]bool{}}
}

// auditSnapshotter is a recorder a test reads the rows of (recRecorder, sshTestRecorder).
type auditSnapshotter interface{ snapshot() []types.AuditEvent }

// unconfirmedRows are the session.unconfirmed rows recorded so far, with their decoded data.
func unconfirmedRows(rec auditSnapshotter) (rows []types.AuditEvent, data []map[string]any) {
	for _, ev := range rec.snapshot() {
		if ev.Action != "session.unconfirmed" {
			continue
		}
		var d map[string]any
		_ = json.Unmarshal(ev.Data, &d)
		rows, data = append(rows, ev), append(data, d)
	}
	return rows, data
}

// wantUnconfirmedRows waits for n session.unconfirmed rows and returns them.
func wantUnconfirmedRows(t *testing.T, rec auditSnapshotter, n int) ([]types.AuditEvent, []map[string]any) {
	t.Helper()
	waitFor(t, "the session.unconfirmed rows", func() bool { r, _ := unconfirmedRows(rec); return len(r) >= n })
	rows, data := unconfirmedRows(rec)
	if len(rows) != n {
		t.Fatalf("session.unconfirmed rows = %d, want %d: %v", len(rows), n, data)
	}
	return rows, data
}

// noUnconfirmedRows holds that no session.unconfirmed row is written, after the off-watch writes had time to land.
func noUnconfirmedRows(t *testing.T, rec auditSnapshotter) {
	t.Helper()
	time.Sleep(50 * time.Millisecond)
	if rows, data := unconfirmedRows(rec); len(rows) != 0 {
		t.Fatalf("session.unconfirmed rows = %v, want none", data)
	}
}

func TestSessionBudget_ObserveWindow(t *testing.T) {
	soft := unconfirmed(errors.New("store down"))
	t.Run("a nil budget holds and has no retry clock", func(t *testing.T) {
		var b *sessionBudget
		if v := b.observe("authority", nil); v != budgetHolds || b.retryC() != nil {
			t.Fatalf("verdict %v, retryC %v", v, b.retryC())
		}
	})
	t.Run("an unanswered check retries and the retry clock fires", func(t *testing.T) {
		h := newHarness(t)
		b := testBudget(h.srv, 60*time.Millisecond, 5*time.Millisecond)
		if v := b.observe("authority", soft); v != budgetRetry {
			t.Fatalf("verdict %v, want budgetRetry", v)
		}
		select {
		case <-b.retryC():
		case <-time.After(3 * time.Second):
			t.Fatal("the retry clock did not fire within 3 s")
		}
	})
	t.Run("an unanswered check past the budget is spent", func(t *testing.T) {
		h := newHarness(t)
		b := testBudget(h.srv, 60*time.Millisecond, 5*time.Millisecond)
		b.observe("authority", soft)
		time.Sleep(70 * time.Millisecond)
		if v := b.observe("authority", soft); v != budgetSpent {
			t.Fatalf("verdict %v, want budgetSpent", v)
		}
	})
	t.Run("a refusal with no window open ends and writes no row", func(t *testing.T) {
		h := newHarness(t)
		b := testBudget(h.srv, time.Hour, 5*time.Millisecond)
		if v := b.observe("authority", errAttachAuthorityEnded); v != budgetRefused {
			t.Fatalf("verdict %v, want budgetRefused", v)
		}
		noUnconfirmedRows(t, h.audit)
	})
	t.Run("a refusal inside a window writes one refused row and end writes nothing more", func(t *testing.T) {
		h := newHarness(t)
		b := testBudget(h.srv, time.Hour, 5*time.Millisecond)
		b.observe("authority", soft)
		if v := b.observe("authority", errAttachAuthorityEnded); v != budgetRefused {
			t.Fatalf("verdict %v, want budgetRefused", v)
		}
		b.end()
		rows, data := wantUnconfirmedRows(t, h.audit, 1)
		if data[0]["result"] != "refused" || rows[0].Outcome != "failure" {
			t.Fatalf("row %v outcome %q, want refused/failure", data[0], rows[0].Outcome)
		}
	})
	t.Run("a later answer closes the window with one rode_through row", func(t *testing.T) {
		h := newHarness(t)
		b := testBudget(h.srv, time.Hour, 5*time.Millisecond)
		b.observe("authority", soft)
		if v := b.observe("authority", nil); v != budgetHolds {
			t.Fatalf("verdict %v, want budgetHolds", v)
		}
		rows, data := wantUnconfirmedRows(t, h.audit, 1)
		if data[0]["result"] != "rode_through" || rows[0].Outcome != "success" || !slices.Equal(auditCheckNames(data[0]["checks"]), []string{"authority"}) {
			t.Fatalf("row %v outcome %q, want rode_through/success over [authority]", data[0], rows[0].Outcome)
		}
	})
	t.Run("two checks that each failed once give one row naming both, sorted", func(t *testing.T) {
		h := newHarness(t)
		b := testBudget(h.srv, time.Hour, 5*time.Millisecond)
		b.observe("closed", soft)
		b.observe("run", soft)
		b.observe("closed", nil)
		if v := b.observe("run", nil); v != budgetHolds {
			t.Fatalf("verdict %v, want budgetHolds", v)
		}
		_, data := wantUnconfirmedRows(t, h.audit, 1)
		if !slices.Equal(auditCheckNames(data[0]["checks"]), []string{"closed", "run"}) {
			t.Fatalf("checks = %v, want [closed run]", data[0]["checks"])
		}
	})
}

func auditCheckNames(v any) []string {
	var out []string
	if xs, ok := v.([]any); ok {
		for _, x := range xs {
			out = append(out, x.(string))
		}
	}
	return out
}

func TestSessionBudget_UnmarkedErrorFailsClosed(t *testing.T) {
	for name, err := range map[string]error{"a plain error": errors.New("x"), "an unwrapped deadline": context.DeadlineExceeded} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			b := testBudget(h.srv, time.Hour, 5*time.Millisecond)
			if v := b.observe("authority", err); v != budgetRefused {
				t.Fatalf("verdict %v, want budgetRefused", v)
			}
			if b.timer != nil || b.retryC() != nil {
				t.Fatal("a refusal armed a retry timer")
			}
			noUnconfirmedRows(t, h.audit)
		})
	}
}

func TestSessionBudget_ZeroEndsAtTheFirstUnansweredCheck(t *testing.T) {
	h := newHarness(t)
	b := testBudget(h.srv, 0, 5*time.Millisecond)
	if v := b.observe("authority", unconfirmed(errors.New("x"))); v != budgetSpent {
		t.Fatalf("verdict %v, want budgetSpent", v)
	}
	b.end()
	_, data := wantUnconfirmedRows(t, h.audit, 1)
	if data[0]["result"] != "ended" || data[0]["budget_s"] != float64(0) {
		t.Fatalf("row %v, want ended with budget_s 0", data[0])
	}
}

func TestSessionBudget_TwoChecksConfirmOnlyWhenBothAnswer(t *testing.T) {
	h := newHarness(t)
	b := testBudget(h.srv, time.Hour, 5*time.Millisecond)
	soft := unconfirmed(errors.New("x"))
	b.observe("closed", soft)
	if v := b.observe("run", nil); v != budgetRetry || !b.retrying("closed") {
		t.Fatalf("verdict %v retrying(closed) %v, want budgetRetry with closed still unanswered", v, b.retrying("closed"))
	}
	if v := b.observe("closed", nil); v != budgetHolds {
		t.Fatalf("verdict %v, want budgetHolds", v)
	}
	wantUnconfirmedRows(t, h.audit, 1)
}

func TestSessionBudget_FenceOpensAndCloses(t *testing.T) {
	h := newHarness(t)
	var calls []bool
	b := testBudget(h.srv, time.Hour, 5*time.Millisecond)
	b.fence = func(on bool) { calls = append(calls, on) }
	soft := unconfirmed(errors.New("x"))
	b.observe("authority", soft)
	b.observe("authority", soft)
	b.observe("authority", nil)
	if !slices.Equal(calls, []bool{true, false}) {
		t.Fatalf("fence calls = %v, want [true false]", calls)
	}
	b.observe("authority", soft)
	b.observe("authority", errAttachAuthorityEnded)
	if !slices.Equal(calls, []bool{true, false, true}) {
		t.Fatalf("a refusal called the fence: %v", calls)
	}
}

// siteReadStore answers GetSiteConfig and counts the reads.
type siteReadStore struct {
	store.Store
	cfg   types.SiteConfig
	err   error
	reads atomic.Int32
}

func (s *siteReadStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	s.reads.Add(1)
	return s.cfg, s.err
}

func TestSessionUnconfirmedBudget_SiteValueCachedAndKeptOnFailure(t *testing.T) {
	newSrv := func(t *testing.T, st store.Store) *Server {
		t.Helper()
		h := newHarness(t)
		return New(baseTestConfig(h, st))
	}
	ctx := context.Background()
	t.Run("no block reads the default", func(t *testing.T) {
		srv := newSrv(t, &siteReadStore{})
		if got := srv.sessionUnconfirmedBudget(ctx); got != 30*time.Second {
			t.Fatalf("budget = %v, want 30s", got)
		}
	})
	t.Run("a set value is cached for the minute and kept when a later read fails", func(t *testing.T) {
		st := &siteReadStore{cfg: types.SiteConfig{Sessions: &types.SiteSessions{UnconfirmedBudgetSec: ptr(45)}}}
		srv := newSrv(t, st)
		if got := srv.sessionUnconfirmedBudget(ctx); got != 45*time.Second {
			t.Fatalf("budget = %v, want 45s", got)
		}
		if got := srv.sessionUnconfirmedBudget(ctx); got != 45*time.Second || st.reads.Load() != 1 {
			t.Fatalf("second call: budget %v after %d reads, want 45s from 1 read", got, st.reads.Load())
		}
		st.err = errors.New("store down")
		srv.sessionBudgets.mu.Lock()
		srv.sessionBudgets.readAt = time.Now().Add(-2 * sessionBudgetReadEvery)
		srv.sessionBudgets.mu.Unlock()
		if got := srv.sessionUnconfirmedBudget(ctx); got != 45*time.Second || st.reads.Load() != 2 {
			t.Fatalf("after a failed read: budget %v after %d reads, want 45s kept from 2 reads", got, st.reads.Load())
		}
	})
	t.Run("a first read that fails reads the default", func(t *testing.T) {
		srv := newSrv(t, &siteReadStore{err: errors.New("store down")})
		if got := srv.sessionUnconfirmedBudget(ctx); got != 30*time.Second {
			t.Fatalf("budget = %v, want 30s", got)
		}
	})
	t.Run("a stored value outside 0 to 120 is clamped", func(t *testing.T) {
		for stored, want := range map[int]time.Duration{-5: 0, 0: 0, 120: 120 * time.Second, 121: 120 * time.Second, 3600: 120 * time.Second} {
			srv := newSrv(t, &siteReadStore{cfg: types.SiteConfig{Sessions: &types.SiteSessions{UnconfirmedBudgetSec: ptr(stored)}}})
			if got := srv.sessionUnconfirmedBudget(ctx); got != want {
				t.Errorf("stored %d: budget = %v, want %v", stored, got, want)
			}
		}
	})
	t.Run("the test override reads nothing", func(t *testing.T) {
		st := &siteReadStore{}
		srv := newSrv(t, st)
		srv.sessionBudgets.test = ptr(time.Duration(0))
		if got := srv.sessionUnconfirmedBudget(ctx); got != 0 || st.reads.Load() != 0 {
			t.Fatalf("budget %v after %d reads, want 0 with none", got, st.reads.Load())
		}
	})
}

func TestSessionBudget_RefusalAfterUnansweredWritesRefusedRow(t *testing.T) {
	h := newHarness(t)
	b := testBudget(h.srv, 90*time.Second, 5*time.Millisecond)
	b.observe("authority", unconfirmed(errors.New("x")))
	b.observe("authority", errAttachAuthorityEnded)
	b.observe("authority", errAttachAuthorityEnded) // a second refusal writes nothing
	rows, data := wantUnconfirmedRows(t, h.audit, 1)
	if rows[0].Outcome != "failure" || data[0]["result"] != "refused" || data[0]["budget_s"] != float64(90) ||
		!slices.Equal(auditCheckNames(data[0]["checks"]), []string{"authority"}) {
		t.Fatalf("row %v outcome %q, want refused/failure with budget_s 90 over [authority]", data[0], rows[0].Outcome)
	}
}

func TestSessionBudget_LeaveWritesClosedInsideAWindow(t *testing.T) {
	soft := unconfirmed(errors.New("x"))
	t.Run("leave inside a window writes one closed row, once", func(t *testing.T) {
		h := newHarness(t)
		b := testBudget(h.srv, 90*time.Second, 5*time.Millisecond)
		b.observe("authority", soft)
		b.leave()
		b.leave()
		rows, data := wantUnconfirmedRows(t, h.audit, 1)
		if rows[0].Outcome != "failure" || data[0]["result"] != "closed" || data[0]["budget_s"] != float64(90) ||
			!slices.Equal(auditCheckNames(data[0]["checks"]), []string{"authority"}) {
			t.Fatalf("row %v outcome %q, want closed/failure with budget_s 90 over [authority]", data[0], rows[0].Outcome)
		}
	})
	t.Run("end then leave writes only the ended row", func(t *testing.T) {
		h := newHarness(t)
		b := testBudget(h.srv, 90*time.Second, 5*time.Millisecond)
		b.observe("authority", soft)
		b.end()
		b.leave()
		_, data := wantUnconfirmedRows(t, h.audit, 1)
		if data[0]["result"] != "ended" {
			t.Fatalf("row %v, want ended", data[0])
		}
	})
	t.Run("a refusal then leave writes only the refused row", func(t *testing.T) {
		h := newHarness(t)
		b := testBudget(h.srv, 90*time.Second, 5*time.Millisecond)
		b.observe("authority", soft)
		b.observe("authority", errAttachAuthorityEnded)
		b.leave()
		_, data := wantUnconfirmedRows(t, h.audit, 1)
		if data[0]["result"] != "refused" {
			t.Fatalf("row %v, want refused", data[0])
		}
	})
	t.Run("leave with no window, and on a nil budget, writes nothing", func(t *testing.T) {
		h := newHarness(t)
		testBudget(h.srv, time.Hour, time.Second).leave()
		var b *sessionBudget
		b.leave()
		noUnconfirmedRows(t, h.audit)
	})
}
