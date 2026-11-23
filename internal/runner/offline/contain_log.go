// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
)

// The bounded logging of containment failures and retries. Split out of manager.go for
// scripts/check-file-size.sh.

// forgetLeftContainLogs drops the containment log of each run that is gone or retired: it left the loop
// without a success, so nothing will log it again.
func (m *Manager) forgetLeftContainLogs() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id := range m.containLogs {
		if r, ok := m.state.Runs[id]; !ok || r.Retired {
			delete(m.containLogs, id)
		}
	}
}

// errContainmentSuperseded is a containment pass whose run changed under it (a concurrent exec or start):
// nothing was confirmed, and the next tick decides again on the current state. It is logged at WARN, not
// ERROR, and names what changed and why the run was selected.
type errContainmentSuperseded struct {
	run                 uuid.UUID
	changed, selectedBy string
}

func (e errContainmentSuperseded) Error() string {
	return fmt.Sprintf("contain run %s: mutation changed during containment (%s changed; selected by %s)", e.run, e.changed, e.selectedBy)
}

// errContain is one run's failed containment attempt, kept typed so the containment loop logs it per run
// (logContainFailure). Its text and its Unwrap are those of fmt.Errorf("contain run %s: %w").
type errContain struct {
	run uuid.UUID
	err error
}

func (e errContain) Error() string { return fmt.Sprintf("contain run %s: %v", e.run, e.err) }
func (e errContain) Unwrap() error { return e.err }

// containSummaryEvery spaces out the repeats of one run's identical containment failure.
const containSummaryEvery = time.Minute

// containTextsPerRun bounds the error texts remembered for one run; past it the run starts over, so a
// text that carries a changing value cannot grow the map.
const containTextsPerRun = 16

// containLog is one logged failure text of one run, and its repeats not logged since.
type containLog struct {
	next     time.Time
	repeated int
}

// logContainFailure logs one run's failed containment attempt at ERROR: a text not yet seen for the run
// at once; a seen one, however the texts interleave, as one summary line at most once per
// containSummaryEvery, saying how many went unlogged. A success logs at once (CheckLapses). Only the line
// is spaced out: every attempt still runs, and still fails its pass.
func (m *Manager) logContainFailure(ctx context.Context, c errContain) {
	m.mu.Lock()
	now, text := m.now(), c.err.Error()
	if m.containLogs == nil {
		m.containLogs = map[uuid.UUID]map[string]*containLog{}
	}
	texts := m.containLogs[c.run]
	if texts == nil || (texts[text] == nil && len(texts) >= containTextsPerRun) {
		texts = map[string]*containLog{}
		m.containLogs[c.run] = texts
	}
	l := texts[text]
	log, repeated := true, 0
	switch {
	case l == nil:
		texts[text] = &containLog{next: now.Add(containSummaryEvery)}
	case now.Before(l.next):
		l.repeated++
		log = false
	default:
		repeated, l.repeated, l.next = l.repeated, 0, now.Add(containSummaryEvery)
	}
	m.mu.Unlock()
	if log {
		slog.ErrorContext(ctx, "runner containment will retry", "run_id", c.run, "error", c, "repeated", repeated)
	}
}

// changedContainmentField is the first thing sameContainmentTarget saw differ.
func changedContainmentField(a, b localRun) string {
	switch {
	case a.Ref != b.Ref:
		return "ref"
	case a.Generation != b.Generation:
		return "generation"
	}
	return "mutation_failure"
}

// selectedBy names the CheckLapses condition that matched, in that condition's order. The caller holds m.mu.
func (m *Manager) selectedBy(id uuid.UUID, r localRun) string {
	switch {
	case m.state.Revoked:
		return "revoked"
	case r.FailedCreation:
		return "failed_creation"
	case r.Containment != nil && !r.Containment.Contained:
		return "containment_open"
	case r.Quarantine != nil && r.ProxyStoppedAt == nil:
		return "quarantine"
	case r.MutationFailure != nil && !r.MutationFailure.Contained:
		return "mutation_failure_open"
	case r.ProxyExited != nil && r.ProxyStoppedAt == nil:
		return "proxy_exited"
	}
	if _, held := m.deadlines[id]; !held {
		return "no_deadline_held"
	}
	return "lease_lapsed"
}

// logContainmentErrors logs a tick's errors: a superseded pass at WARN, one line each, a run's failed
// containment per run (logContainFailure), and everything else at ERROR as one line, through logRetry.
func (m *Manager) logContainmentErrors(ctx context.Context, err error) {
	var other []error
	var walk func(error)
	walk = func(e error) {
		if multi, ok := e.(interface{ Unwrap() []error }); ok {
			for _, inner := range multi.Unwrap() {
				walk(inner)
			}
			return
		}
		var sup errContainmentSuperseded
		if errors.As(e, &sup) {
			slog.WarnContext(ctx, "runner containment superseded, re-checking", "run_id", sup.run, "changed", sup.changed, "selected_by", sup.selectedBy)
			return
		}
		var c errContain
		if errors.As(e, &c) {
			m.logContainFailure(ctx, c)
			return
		}
		other = append(other, e)
	}
	walk(err)
	if len(other) > 0 {
		m.logRetry(ctx, slog.LevelError, "runner containment will retry", errors.Join(other...))
	}
}
