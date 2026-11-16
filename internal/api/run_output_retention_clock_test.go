// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// retentionClockChunks is the orphan-chunk fixture's store on a fake database clock: DatabaseNow answers at, or
// fails with err. The fixture's own store carries no clock, so dbNow would read the server's.
type retentionClockChunks struct {
	*miscCovOrphanStore
	at  time.Time
	err error
}

func (s retentionClockChunks) DatabaseNow(context.Context) (time.Time, error) { return s.at, s.err }

// retentionClockOutputs is the recording-output fixture's store on a fake database clock, as above.
type retentionClockOutputs struct {
	*memRunOutputs
	at  time.Time
	err error
}

func (s retentionClockOutputs) DatabaseNow(context.Context) (time.Time, error) { return s.at, s.err }

const retentionWindow = 30 * 24 * time.Hour

// A read of an ended run's output judges the retention window on the database's clock, which stamps ended_at,
// whatever the server's clock says: chunks a fault left are served only inside it, and with none the answer is
// expired past it and not kept inside it. A clock that cannot be read is a server error and serves no chunk, and a
// run with no end never reads it. Catches: the window judged on the server's clock, which a skewed replica moves.
func TestRunOutput_RetentionIsJudgedOnTheDatabaseClock(t *testing.T) {
	const day = 24 * time.Hour
	for _, c := range []struct {
		name     string
		endedAgo time.Duration // on the server's clock; 0 is no end
		dbOff    time.Duration
		clockErr error
		chunks   bool
		code     int
		reason   string
		reads    int
	}{
		{"past the window on the database's clock, inside it on the server's", retentionWindow - time.Hour, 5 * day, nil, true, http.StatusGone, reasonRunOutputExpired, 0},
		{"inside the window on the database's clock, past it on the server's: the chunks are served", retentionWindow + time.Hour, -5 * day, nil, true, http.StatusOK, "", 1},
		{"inside the window on the database's clock, past it on the server's, no chunks", retentionWindow + time.Hour, -5 * day, nil, false, http.StatusConflict, reasonRunOutputNotKept, 1},
		{"the database's clock cannot be read", retentionWindow + time.Hour, 0, errClockRead, true, http.StatusInternalServerError, "", 0},
		{"a run with no end never reads the clock", 0, 0, errClockRead, false, http.StatusConflict, reasonRunOutputNotKept, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, orph := miscCovOrphanFixture(t, c.endedAgo, func(cfg *Config) { cfg.RunOutputRetention = retentionWindow })
			if c.chunks {
				orph.readRes = store.RunOutputChunks{Found: true, Bytes: []byte("left by a fault")}
			}
			if c.endedAgo == 0 {
				f.st.mu.Lock()
				f.st.run.EndedAt = nil
				f.st.mu.Unlock()
			}
			f.srv.cfg.Store = retentionClockChunks{miscCovOrphanStore: orph, at: time.Now().Add(c.dbOff), err: c.clockErr}
			w := do(t, f.srv, http.MethodGet, "/api/v1/runs/"+f.run.ID.String()+"/output", adminToken, "")
			orph.mu.Lock()
			reads := len(orph.reads)
			orph.mu.Unlock()
			if w.Code != c.code || !strings.Contains(w.Body.String(), c.reason) || reads != c.reads {
				t.Fatalf("read = %d %s after %d chunk reads, want %d %s after %d", w.Code, w.Body, reads, c.code, c.reason, c.reads)
			}
			if c.code == http.StatusOK && !strings.Contains(w.Body.String(), "left by a fault") {
				t.Fatalf("read = %s, want the chunks", w.Body)
			}
		})
	}
}

// recordingRetentionFixture is the recording-output fixture with a 30-day window, its run COMPLETED endedAgo ago on
// the server's clock and its store on a database clock dbOff from the server's, or failing with clockErr. It
// returns the run as the store answers it.
func recordingRetentionFixture(t *testing.T, endedAgo, dbOff time.Duration, clockErr error) (*outputFixture, *outputRecordingStore, types.AgentRun) {
	t.Helper()
	f, rs := newRecordingOutputFixture(t, func(cfg *Config) { cfg.RunOutputRetention = retentionWindow })
	ended := time.Now().Add(-endedAgo)
	f.st.mu.Lock()
	f.st.run.EndedAt = &ended
	f.st.mu.Unlock()
	f.srv.cfg.Store = retentionClockOutputs{memRunOutputs: f.mem, at: time.Now().Add(dbOff), err: clockErr}
	run, err := f.srv.cfg.Store.GetRun(t.Context(), f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return f, rs, run
}

// Whether an ended run's output is read from its recording judges the window on the database's clock, and a clock
// that cannot be read keeps the run on its recording path. Catches: the window judged on the server's clock; and a
// failed clock read sending the run's end and the stale sweep to the path that writes a gap.
func TestRecordingOutputEnabled_TheWindowIsTheDatabasesClock(t *testing.T) {
	const day = 24 * time.Hour
	for _, c := range []struct {
		name     string
		endedAgo time.Duration
		dbOff    time.Duration
		clockErr error
		want     bool
	}{
		{"past the window on the database's clock", retentionWindow - time.Hour, 5 * day, nil, false},
		{"inside the window on the database's clock", retentionWindow + time.Hour, -5 * day, nil, true},
		{"the clock cannot be read: the run stays on its recording path", retentionWindow + time.Hour, 0, errClockRead, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, _, run := recordingRetentionFixture(t, c.endedAgo, c.dbOff, c.clockErr)
			if got := f.srv.recordingOutputEnabled(t.Context(), run); got != c.want {
				t.Fatalf("recordingOutputEnabled = %v, want %v", got, c.want)
			}
		})
	}
}

// The read door's repair rebuilds no output for a run past the window on the database's clock, and a clock that
// cannot be read is the read's error, with no recording opened. Catches: a repair on the server's clock, and a
// rebuild of output the window may have expired on a guess.
func TestRepairRecordingOutput_TheWindowIsTheDatabasesClock(t *testing.T) {
	t.Run("past the window on the database's clock", func(t *testing.T) {
		f, rs, run := recordingRetentionFixture(t, retentionWindow-time.Hour, 5*24*time.Hour, nil)
		if _, found, err := f.srv.repairRecordingOutput(t.Context(), run, store.RunOutput{}, false); err != nil || found || rs.opens.Load() != 0 {
			t.Fatalf("repair found=%v err=%v after %d opens, want nothing and no open", found, err, rs.opens.Load())
		}
	})
	t.Run("the clock cannot be read", func(t *testing.T) {
		f, rs, run := recordingRetentionFixture(t, retentionWindow+time.Hour, 0, errClockRead)
		if _, _, err := f.srv.repairRecordingOutput(t.Context(), run, store.RunOutput{}, false); !errors.Is(err, errClockRead) || rs.opens.Load() != 0 {
			t.Fatalf("repair err=%v after %d opens, want the clock's error and no open", err, rs.opens.Load())
		}
	})
}

// A cast upload owes no recording output for a run past the window on the database's clock, and a clock that cannot
// be read is the upload's error, as a run that cannot be read is. Catches: the upload's window on the server's clock.
func TestUploadedRecordingOutput_TheWindowIsTheDatabasesClock(t *testing.T) {
	t.Run("past the window on the database's clock", func(t *testing.T) {
		f, rs, _ := recordingRetentionFixture(t, retentionWindow-time.Hour, 5*24*time.Hour, nil)
		if err := f.srv.uploadedRecordingOutput(t.Context(), f.run.ID); err != nil || rs.opens.Load() != 0 {
			t.Fatalf("upload err=%v after %d opens, want nil and no open", err, rs.opens.Load())
		}
	})
	t.Run("the clock cannot be read", func(t *testing.T) {
		f, rs, _ := recordingRetentionFixture(t, retentionWindow+time.Hour, 0, errClockRead)
		if err := f.srv.uploadedRecordingOutput(t.Context(), f.run.ID); !errors.Is(err, errClockRead) || rs.opens.Load() != 0 {
			t.Fatalf("upload err=%v after %d opens, want the clock's error and no open", err, rs.opens.Load())
		}
	})
}
