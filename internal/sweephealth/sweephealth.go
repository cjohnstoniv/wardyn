// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package sweephealth records when each background sweep last tried and last
// finished, in one row per sweep that every replica shares, so a sweep that has
// stopped doing its work shows on a gauge and on /setup/status instead of only
// in the data it was supposed to clean up.
//
// The record is durable and shared because the sweeps that must run once run on
// one replica (db.SweeperLeader): a follower that kept its own in-memory tick
// times would report a healthy leader's sweeps as dead, and one that watched
// only the sweeps it started would not see a stopped leader at all. So every
// replica registers every sweep that is configured on this install, whichever
// replica runs it, and reads the same rows.
package sweephealth

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"time"
)

// The closed set of sweep names. A name is a metric label, so a new sweep is a
// deliberate addition here, never a free-form string.
const (
	IdleReaper         = "idle_reaper"
	TerminalSandbox    = "terminal_sandbox"
	ApprovalExpiry     = "approval_expiry"
	RunSecret          = "run_secret"
	CredentialExpiry   = "credential_expiry"
	RecordingRetention = "recording_retention"
	RunWatcher         = "run_watcher"
	OrphanedBuild      = "orphaned_build"
)

// Names lists every sweep, in the order the status output uses.
var Names = []string{
	IdleReaper, TerminalSandbox, ApprovalExpiry, RunSecret,
	CredentialExpiry, RecordingRetention, RunWatcher, OrphanedBuild,
}

// StaleAfterIntervals is how many of its own intervals a sweep may go without a
// success before it reads as stale: one missed tick is a busy store, three is a
// sweep that is not running.
const StaleAfterIntervals = 3

// markTimeout bounds one tick write, so a stalled store delays a sweep by at
// most this much per write and never stops it.
const markTimeout = 3 * time.Second

// Sweep is one registered sweep: its name and how often it ticks.
type Sweep struct {
	Name     string
	Interval time.Duration
}

// Tick is the shared record of one sweep. A zero time means never.
type Tick struct {
	AttemptedAt time.Time
	SucceededAt time.Time
	// Replica is the process that wrote the newest attempt (forensics only).
	Replica string
}

// Store is the shared record. RecordTick must never move a time backwards: a
// write older than the stored one leaves it as it was.
type Store interface {
	RecordTick(ctx context.Context, sweep, replica string, success bool, at time.Time) error
	Ticks(ctx context.Context) (map[string]Tick, error)
}

// Status is one registered sweep as the gauges and the setup row read it.
type Status struct {
	Sweep
	AttemptedAt time.Time
	SucceededAt time.Time
	// Stale is true when the sweep has gone StaleAfterIntervals of its own
	// intervals without a success, counted from the later of its last success
	// and this process's start.
	Stale bool
}

// Tracker registers the sweeps this install runs and records their ticks. The
// zero value is not usable; a nil *Tracker is, and records nothing.
type Tracker struct {
	store   Store
	replica string
	now     func() time.Time
	started time.Time

	mu     sync.Mutex
	sweeps map[string]time.Duration
}

// New returns a Tracker writing to store as replica. now is the clock (nil is
// time.Now); the process start the staleness grace runs from is read from it
// here.
func New(store Store, replica string, now func() time.Time) *Tracker {
	if now == nil {
		now = time.Now
	}
	return &Tracker{store: store, replica: replica, now: now, started: now(), sweeps: map[string]time.Duration{}}
}

// Register declares sweeps this install runs, whichever replica holds the lock
// for them. A sweep that is never registered is never reported and never stale.
// Safe on a nil Tracker.
func (t *Tracker) Register(sweeps ...Sweep) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, s := range sweeps {
		t.sweeps[s.Name] = s.Interval
	}
}

// Tick runs one sweep tick: it records the attempt, runs body, and records the
// success only when body returned nil. Both writes are best-effort, logged and
// never allowed to stop the body. A body that panics or errors leaves the
// success time where it was (the success write follows the call, it is not in
// a defer). A nil Tracker just runs body.
func (t *Tracker) Tick(ctx context.Context, sweep string, body func(context.Context) error) error {
	if t == nil {
		return body(ctx)
	}
	t.mark(ctx, sweep, false)
	err := body(ctx)
	if err == nil {
		t.mark(ctx, sweep, true)
	}
	return err
}

func (t *Tracker) mark(ctx context.Context, sweep string, success bool) {
	// Detached from ctx: a tick that finished cleanly as the process was told to
	// stop is still a success worth recording.
	mctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), markTimeout)
	defer cancel()
	if err := t.store.RecordTick(mctx, sweep, t.replica, success, t.now()); err != nil {
		slog.WarnContext(ctx, "wardynd: sweep tick not recorded",
			slog.String("sweep", sweep), slog.Bool("success", success), slog.Any("err", err))
	}
}

// Registered returns the registered sweeps in Names order.
func (t *Tracker) Registered() []Sweep {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Sweep, 0, len(t.sweeps))
	for name, iv := range t.sweeps {
		out = append(out, Sweep{Name: name, Interval: iv})
	}
	slices.SortFunc(out, func(a, b Sweep) int {
		return slices.Index(Names, a.Name) - slices.Index(Names, b.Name)
	})
	return out
}

// Status reads the shared record and reports each registered sweep. It returns
// an error when the record cannot be read: that is "cannot tell", never stale,
// so the caller leaves the sweep series and the row out rather than guess.
func (t *Tracker) Status(ctx context.Context) ([]Status, error) {
	sweeps := t.Registered()
	if len(sweeps) == 0 {
		return nil, nil
	}
	ticks, err := t.store.Ticks(ctx)
	if err != nil {
		return nil, err
	}
	now := t.now()
	out := make([]Status, 0, len(sweeps))
	for _, s := range sweeps {
		tk := ticks[s.Name]
		ref := tk.SucceededAt
		if t.started.After(ref) {
			ref = t.started
		}
		out = append(out, Status{
			Sweep: s, AttemptedAt: tk.AttemptedAt, SucceededAt: tk.SucceededAt,
			Stale: s.Interval > 0 && now.Sub(ref) >= StaleAfterIntervals*s.Interval,
		})
	}
	return out, nil
}
