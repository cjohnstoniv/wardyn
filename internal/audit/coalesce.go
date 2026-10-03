// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"cmp"
	"context"
	"encoding/json"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A member editing against a closed door re-resolves Review on every
// keystroke, and each refused dry run writes the gate's authz.denied row.
// DenialCoalescer keeps the first row in full, counts the identical repeats
// inside a window, then APPENDS one summary row when the window closes. Nothing
// already recorded is ever changed: the chain is append-only.

const (
	// DenialAction and DenialOutcome are the only rows the coalescer acts on.
	DenialAction  = authz.AuditAction
	DenialOutcome = "denied"
	// CoalesceAction is the summary row's action. It is never authz.denied, so
	// a SIEM rule over denials does not count it twice.
	CoalesceAction = "preflight.denial.coalesce"

	// DefaultCoalesceWindow is how long repeats are counted before the summary.
	DefaultCoalesceWindow = 10 * time.Minute
	// DefaultCoalesceMaxKeys bounds the open windows.
	DefaultCoalesceMaxKeys = 1024
)

type denialKey struct{ actor, action, target, reason, outcome string }

type denialWindow struct {
	first  types.AuditEvent
	reason string
	seq    uint64
	timer  *time.Timer
	// count includes first; both are guarded by DenialCoalescer.mu.
	count  int
	lastAt time.Time
}

// DenialCoalescer summarises repeated refused dry runs. It is per replica: each
// keeps its own windows. It acts only on an authz.denied/denied event recorded
// under a dry-run context (DryRunFrom), never on what the event's data says;
// every other event, a launch refusal included, passes through untouched.
type DenialCoalescer struct {
	// Inner receives every event, summaries included. Set before first use.
	Inner Recorder
	// Window and MaxKeys default to DefaultCoalesceWindow and DefaultCoalesceMaxKeys.
	Window  time.Duration
	MaxKeys int

	mu   sync.Mutex
	open map[denialKey]*denialWindow
	seq  uint64
}

// Record passes ev on, or counts it when an identical refused dry run is
// already inside its window.
func (c *DenialCoalescer) Record(ctx context.Context, ev types.AuditEvent) error {
	if !DryRunFrom(ctx) || ev.Action != DenialAction || ev.Outcome != DenialOutcome {
		return c.Inner.Record(ctx, ev)
	}
	var d struct {
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(ev.Data, &d)
	k := denialKey{ev.Actor, ev.Action, ev.Target, d.Reason, ev.Outcome}

	c.mu.Lock()
	if w, ok := c.open[k]; ok {
		w.count++
		w.lastAt = ev.Time
		c.mu.Unlock()
		return nil
	}
	var evicted *denialWindow
	if len(c.open) >= cmp.Or(c.MaxKeys, DefaultCoalesceMaxKeys) {
		evicted = c.evictOldestLocked()
	}
	if c.open == nil {
		c.open = map[denialKey]*denialWindow{}
	}
	c.seq++
	w := &denialWindow{first: ev, reason: d.Reason, seq: c.seq, count: 1, lastAt: ev.Time}
	w.timer = time.AfterFunc(cmp.Or(c.Window, DefaultCoalesceWindow), func() { c.closeWindow(k, w) })
	c.open[k] = w
	c.mu.Unlock()

	// An early close owes its summary now, and the caller's cancellation must
	// not lose it.
	c.summarise(context.WithoutCancel(ctx), evicted)
	err := c.Inner.Record(ctx, ev)
	if err != nil {
		// The first row did not land: let the next identical event try again
		// rather than be counted against a row that does not exist.
		c.mu.Lock()
		if c.open[k] == w && w.count == 1 {
			w.timer.Stop()
			delete(c.open, k)
		}
		c.mu.Unlock()
	}
	return err
}

// evictOldestLocked removes and returns the oldest open window. c.mu is held.
func (c *DenialCoalescer) evictOldestLocked() *denialWindow {
	var oldestKey denialKey
	var oldest *denialWindow
	for k, w := range c.open {
		if oldest == nil || w.seq < oldest.seq {
			oldestKey, oldest = k, w
		}
	}
	if oldest != nil {
		oldest.timer.Stop()
		delete(c.open, oldestKey)
	}
	return oldest
}

// closeWindow ends w's window when its timer fires.
func (c *DenialCoalescer) closeWindow(k denialKey, w *denialWindow) {
	c.mu.Lock()
	if c.open[k] != w {
		c.mu.Unlock()
		return
	}
	delete(c.open, k)
	c.mu.Unlock()
	c.summarise(context.Background(), w)
}

// Flush closes every open window and writes its summary through Inner. Run it
// on graceful shutdown, before the audit sinks and the pool are closed.
func (c *DenialCoalescer) Flush(ctx context.Context) {
	c.mu.Lock()
	ws := make([]*denialWindow, 0, len(c.open))
	for _, w := range c.open {
		w.timer.Stop()
		ws = append(ws, w)
	}
	c.open = nil
	c.mu.Unlock()
	// Oldest first, so the summaries append in the order the denials began.
	slices.SortFunc(ws, func(a, b *denialWindow) int { return cmp.Compare(a.seq, b.seq) })
	ctx = context.WithoutCancel(ctx)
	for _, w := range ws {
		c.summarise(ctx, w)
	}
}

// summarise appends w's summary row when the window saw a repeat. count
// includes the first row, which was written in full.
func (c *DenialCoalescer) summarise(ctx context.Context, w *denialWindow) {
	if w == nil {
		return
	}
	c.mu.Lock()
	count, last := w.count, w.lastAt
	c.mu.Unlock()
	if count < 2 {
		return
	}
	// A fixed shape, so a SIEM rule can rely on it.
	data, _ := json.Marshal(struct {
		DryRun     bool      `json:"dry_run"`
		Reason     string    `json:"reason"`
		Count      int       `json:"count"`
		Suppressed int       `json:"suppressed"`
		FirstAt    time.Time `json:"first_at"`
		LastAt     time.Time `json:"last_at"`
	}{true, w.reason, count, count - 1, w.first.Time, last})
	ev := types.AuditEvent{
		ID:        uuid.New(),
		Time:      time.Now().UTC(),
		RunID:     w.first.RunID,
		ActorType: w.first.ActorType,
		Actor:     w.first.Actor,
		Action:    CoalesceAction,
		Target:    w.first.Target,
		Outcome:   DenialOutcome,
		SourceIP:  w.first.SourceIP,
		Data:      data,
	}
	if err := c.Inner.Record(ctx, ev); err != nil {
		LogWriteFailure(ctx, ev, err)
	}
}
