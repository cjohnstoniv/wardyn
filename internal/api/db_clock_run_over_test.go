// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// capClockRun holds store.Store one level below capClockStore's own methods and its RunPauser, so those win any
// name the two interfaces share.
type capClockRun struct{ store.Store }

// capClockStore serves one RUNNING run with a lifetime cap, on a fake database clock off from the server's: DatabaseNow
// answers time.Now().Add(off), and fails from its failFrom-th read on (0: never). It counts its clock reads and what
// would move the run's idle and presence clocks and its failure hint.
type capClockStore struct {
	capClockRun
	store.RunPauser
	mu                                    sync.Mutex
	run                                   types.AgentRun
	off                                   time.Duration
	failFrom, reads                       int
	touched, stamped, hinted, endedHinted int
}

// newCapClockStore is run runID, capped capFromDB from the fake database's now, which is off from the server's.
func newCapClockStore(runID uuid.UUID, off, capFromDB time.Duration) *capClockStore {
	now := time.Now().UTC()
	capAt := now.Add(off + capFromDB)
	return &capClockStore{off: off, run: types.AgentRun{ID: runID, State: types.RunRunning, CreatedAt: now.Add(-time.Hour),
		UpdatedAt: now, LifetimeCapAt: &capAt}}
}

func (s *capClockStore) GetRun(_ context.Context, id uuid.UUID) (types.AgentRun, error) {
	if id != s.run.ID {
		return types.AgentRun{}, store.ErrNotFound
	}
	return s.run, nil
}

func (s *capClockStore) DatabaseNow(context.Context) (time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	if s.failFrom > 0 && s.reads >= s.failFrom {
		return time.Time{}, errClockRead
	}
	return time.Now().Add(s.off).UTC(), nil
}

func (s *capClockStore) TouchRun(context.Context, uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.touched++
	return nil
}

func (s *capClockStore) StampRunActive(context.Context, uuid.UUID) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stamped++
	return false, nil
}

func (s *capClockStore) SetRunFailureHint(context.Context, uuid.UUID, string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hinted++
	return nil
}

func (s *capClockStore) SetEndedRunFailureHint(context.Context, uuid.UUID, string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.endedHinted++
	return nil
}

func (s *capClockStore) counts() (reads, touched, stamped, hinted, endedHinted int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads, s.touched, s.stamped, s.hinted, s.endedHinted
}

// reportLostRows counts runID's run.report.drop rows that carry reason.
func reportLostRows(h *harness, runID uuid.UUID, reason string) int {
	n := 0
	for _, ev := range h.audit.snapshot() {
		if ev.Action == runReportLostAction && ev.RunID != nil && *ev.RunID == runID && strings.Contains(string(ev.Data), reason) {
			n++
		}
	}
	return n
}

const allowDecision = `{"request":{"host":"pypi.org","method":"GET"},"decision":"allow"}`

// runIsOverNow judges a live run's lifetime cap on the database's clock, whatever the server's says, and reads that
// clock only for that: a terminal or kept run is over and a live run with no cap is not, with no read, so a clock that
// cannot be read changes neither. A capped live run whose clock cannot be read is an error, never an answer.
func TestRunIsOverNow_JudgesTheCapOnTheDatabaseClock(t *testing.T) {
	db := time.Now().UTC()
	before, after, lost := db.Add(-time.Minute), db.Add(time.Minute), db.Add(-time.Hour)
	for _, tc := range []struct {
		name        string
		run         types.AgentRun
		serverOff   time.Duration
		clockErr    error
		over, fails bool
	}{
		{"past the cap on the database's clock, the server's behind", types.AgentRun{State: types.RunRunning, LifetimeCapAt: &before}, -2 * time.Minute, nil, true, false},
		{"inside the cap on the database's clock, the server's ahead", types.AgentRun{State: types.RunRunning, LifetimeCapAt: &after}, 2 * time.Minute, nil, false, false},
		{"a terminal run, the clock failing", types.AgentRun{State: types.RunCompleted, LifetimeCapAt: &after}, 0, errClockRead, true, false},
		{"a kept run, the clock failing", types.AgentRun{State: types.RunRunning, LostAt: &lost, LifetimeCapAt: &after}, 0, errClockRead, true, false},
		{"a live run with no cap, the clock failing", types.AgentRun{State: types.RunRunning}, 0, errClockRead, false, false},
		{"a capped live run, the clock failing", types.AgentRun{State: types.RunRunning, LifetimeCapAt: &after}, 0, errClockRead, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			off := tc.serverOff
			s := &Server{cfg: Config{Store: &dispatchTestStore{dbNow: func() time.Time { return db }, clockErr: tc.clockErr},
				Now: func() time.Time { return db.Add(off) }}}
			over, err := s.runIsOverNow(context.Background(), tc.run)
			if over != tc.over || (err != nil) != tc.fails {
				t.Fatalf("over %v, err %v; want over %v, an error %v", over, err, tc.over, tc.fails)
			}
		})
	}
}

// A decision on a live run with a lifetime cap moves its idle and presence clocks only while the run is inside its
// cap on the database's clock, whatever the server's says. A clock that cannot be read after the liveness gate's own
// read moves neither. The run is judged only when a touch or a stamp is due: inside the debounce, a second post reads
// the clock once fewer than the first. Catches: the cap judged on the server's clock, so a replica ahead of the
// database stops counting a live run's activity and one behind counts a capped run's; and a clock read on every post.
func TestInternalDecision_ActivityIsJudgedOnTheDatabaseClock(t *testing.T) {
	for _, off := range oneClockSkews {
		for _, tc := range []struct {
			name             string
			capFromDB        time.Duration
			failFrom         int
			touched, stamped int
		}{
			{"past its cap", -time.Second, 0, 0, 0},
			{"inside its cap", time.Minute, 0, 1, 1},
			{"the clock fails after the gate's read", time.Minute, 2, 0, 0},
		} {
			t.Run(fmt.Sprintf("%s, server off %v", tc.name, off), func(t *testing.T) {
				h := newHarness(t)
				runID := uuid.New()
				st := newCapClockStore(runID, off, tc.capFromDB)
				st.failFrom = tc.failFrom
				srv := New(baseTestConfig(h, st))
				tok := h.mintRunToken(t, runID)
				post := func() {
					t.Helper()
					if w := do(t, srv, http.MethodPost, "/api/v1/internal/decisions", tok, allowDecision); w.Code != http.StatusAccepted {
						t.Fatalf("code = %d, want 202; body=%s", w.Code, w.Body.String())
					}
				}
				post()
				first, touched, stamped, _, _ := st.counts()
				if touched != tc.touched || stamped != tc.stamped {
					t.Fatalf("touched %d, stamped %d; want %d, %d", touched, stamped, tc.touched, tc.stamped)
				}
				if tc.touched == 0 {
					return
				}
				post()
				if reads, touched, stamped, _, _ := st.counts(); reads-first != first-1 || touched != 1 || stamped != 1 {
					t.Errorf("the second post read the clock %d times (the first %d), touched %d, stamped %d; want one read fewer, and no new touch or stamp",
						reads-first, first, touched, stamped)
				}
			})
		}
	}
}

// A revoked run token reaches the decision door only for a run that is over, and a live run's lifetime cap is judged
// on the database's clock, whatever the server's says. A clock that cannot be read is a retryable refusal noted on
// the run's trail, as a run that cannot be read is. Catches: the cap judged on the server's clock, which lets a
// skewed replica take a live run's revoked token, or refuse a capped run's last reports.
func TestVerifyRunToken_JudgesARevokedTokensRunOnTheDatabaseClock(t *testing.T) {
	for _, off := range oneClockSkews {
		for _, tc := range []struct {
			name       string
			capFromDB  time.Duration
			failFrom   int
			code, rows int
		}{
			{"past its cap", -time.Second, 0, http.StatusAccepted, 0},
			{"inside its cap", time.Minute, 0, http.StatusUnauthorized, 0},
			{"the clock fails", time.Minute, 1, http.StatusServiceUnavailable, 1},
		} {
			t.Run(fmt.Sprintf("%s, server off %v", tc.name, off), func(t *testing.T) {
				h := newHarness(t)
				runID := uuid.New()
				st := newCapClockStore(runID, off, tc.capFromDB)
				st.failFrom = tc.failFrom
				srv := New(baseTestConfig(h, st))
				tok := h.mintRunToken(t, runID)
				if err := h.idp.RevokeRun(context.Background(), runID); err != nil {
					t.Fatal(err)
				}
				w := do(t, srv, http.MethodPost, "/api/v1/internal/decisions", tok, allowDecision)
				if rows := reportLostRows(h, runID, reportLostUnreadable); w.Code != tc.code || rows != tc.rows {
					t.Fatalf("code = %d, %d unreadable run.report.drop rows; want %d, %d; body=%s", w.Code, rows, tc.code, tc.rows, w.Body.String())
				}
			})
		}
	}
}

// A late Bedrock report on a live run with a lifetime cap takes the hint write that leaves updated_at only when the
// run is past its cap on the database's clock, whatever the server's says, and a clock that cannot be read writes no
// hint. Catches: the cap judged on the server's clock, which opens a capped run's tail grace again on a skewed replica.
func TestNoteBedrockDataPlaneFault_JudgesTheRunOnTheDatabaseClock(t *testing.T) {
	for _, off := range oneClockSkews {
		for _, tc := range []struct {
			name                string
			capFromDB           time.Duration
			failFrom            int
			hinted, endedHinted int
		}{
			{"past its cap", -time.Second, 0, 0, 1},
			{"inside its cap", time.Minute, 0, 1, 0},
			{"the clock fails", time.Minute, 1, 0, 0},
		} {
			t.Run(fmt.Sprintf("%s, server off %v", tc.name, off), func(t *testing.T) {
				runID := uuid.New()
				st := newCapClockStore(runID, off, tc.capFromDB)
				st.failFrom = tc.failFrom
				s := &Server{cfg: Config{Store: st, Now: time.Now}}
				s.noteBedrockDataPlaneFault(context.Background(), runID, "ThrottlingException")
				if _, _, _, hinted, ended := st.counts(); hinted != tc.hinted || ended != tc.endedHinted {
					t.Fatalf("hint %d, hint keeping updated_at %d; want %d, %d", hinted, ended, tc.hinted, tc.endedHinted)
				}
			})
		}
	}
}

// A post on a token that lapsed long ago is noted on its run's trail only while the run is inside its lifetime cap on
// the database's clock. A clock that cannot be read writes no row and gives the run's one read back, so the next post
// reads it again. Catches: the cap judged on the server's clock; and a read spent on a clock that failed, which writes
// no row for the rest of the span.
func TestNoteExpiredDecisionPost_JudgesTheRunOnTheDatabaseClock(t *testing.T) {
	for _, off := range oneClockSkews {
		for _, tc := range []struct {
			name      string
			capFromDB time.Duration
			failFrom  int
			rows      int
			given     bool // the run's read is given back
		}{
			{"past its cap", -time.Second, 0, 0, false},
			{"inside its cap", time.Minute, 0, 1, false},
			{"the clock fails", time.Minute, 1, 0, true},
		} {
			t.Run(fmt.Sprintf("%s, server off %v", tc.name, off), func(t *testing.T) {
				h := newHarness(t)
				runID := uuid.New()
				st := newCapClockStore(runID, off, tc.capFromDB)
				st.failFrom = tc.failFrom
				srv := New(baseTestConfig(h, st))
				r := httptest.NewRequest(http.MethodPost, decisionsDoor, nil)
				srv.noteExpiredDecisionPost(r, &identity.ExpiredTokenError{RunID: runID, Expiry: time.Now().Add(-2 * identity.RunTokenLapseAfter)})
				rows := reportLostRows(h, runID, reportLostExpired)
				if given := srv.claimDecisionRefusal(runID, expiredRunRead); rows != tc.rows || given != tc.given {
					t.Fatalf("%d expired-token rows, read given back %v; want %d, %v", rows, given, tc.rows, tc.given)
				}
			})
		}
	}
}
