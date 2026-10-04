// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// RunBudget is how many tier-0 outbox rows one run may create per hour. The sandbox agent decides how
// many distinct approvals its run raises, so without a bound a flood of chat cards could bury the one
// real approval. Counted in rows, so a deployment with N channels admits 25/N raises. The console
// still lists every approval.
const RunBudget = 25

// ProfileSQL reads the run's leaf governance profile inside the approval's own transaction, so a
// concurrent profile change cannot route this approval by stale data.
const ProfileSQL = `SELECT governance_profile_id FROM agent_runs WHERE id = $1`

var (
	active atomic.Pointer[Config]
	rec    atomic.Pointer[audit.Recorder]
)

// SetActive installs the boot config and the audit recorder the worker and the budget use. A nil
// config turns notifications off: Plan returns nothing and no row is ever written.
func SetActive(c *Config, r audit.Recorder) {
	active.Store(c)
	if r == nil {
		rec.Store(nil)
		return
	}
	rec.Store(&r)
}

// Enabled reports whether a config is installed.
func Enabled() bool { return active.Load() != nil }

// Planned is one outbox row a new approval will get.
type Planned struct {
	Tier    int16
	Channel string
	After   time.Duration
}

// Plan returns the rows to enqueue for a new approval, a pure function of the boot config. It is the
// one planner both insertion seams call, so they cannot drift. In this build every channel is planned
// at tier 0; kind and profileID are the inputs routing will read.
func Plan(kind types.ApprovalKind, profileID *uuid.UUID) []Planned {
	c := active.Load()
	if c == nil {
		return nil
	}
	out := make([]Planned, 0, len(c.Channels))
	for _, ch := range c.Channels {
		out = append(out, Planned{Tier: 0, Channel: ch.ID})
	}
	return out
}

// Enqueue is the notification half of one approval insert. The zero value is "notifications off".
type Enqueue struct{ planned []Planned }

// NewEnqueue plans the rows for a new approval of this kind under this profile.
func NewEnqueue(kind types.ApprovalKind, profileID *uuid.UUID) Enqueue {
	return Enqueue{planned: Plan(kind, profileID)}
}

// On reports whether the statement should carry the outbox insert.
func (e Enqueue) On() bool { return len(e.planned) > 0 }

// queueTail is appended to a statement that defines the CTE `ins` (the approval row: id, run_id,
// requested_at). Inserting from `ins` is what makes a dedup loser enqueue nothing, and the budget is
// part of the same statement so it cannot be read apart from the insert.
const queueTail = `,
queued AS (
	INSERT INTO approval_notifications (id, approval_id, tier, channel, due_at, next_attempt_at)
	SELECT gen_random_uuid(), ins.id, p.tier, p.channel,
	       ins.requested_at + p.after_s * interval '1 second', ins.requested_at + p.after_s * interval '1 second'
	  FROM ins, unnest($%d::smallint[], $%d::text[], $%d::int[]) AS p(tier, channel, after_s)
	 WHERE (SELECT count(*) FROM approval_notifications b JOIN approvals a ON a.id = b.approval_id
	         WHERE a.run_id = ins.run_id AND b.tier = 0 AND b.created_at > now() - interval '1 hour') < %d
	RETURNING 1
)
SELECT (SELECT count(*) FROM ins), (SELECT count(*) FROM queued)`

// Tail returns the text to append after a `WITH ins AS (...)` clause, numbering its three parameters
// from firstArg; Args returns their values. The statement then yields (approvals inserted, outbox rows
// inserted).
func (e Enqueue) Tail(firstArg int) string {
	return fmt.Sprintf(queueTail, firstArg, firstArg+1, firstArg+2, RunBudget)
}

// Args are the three array parameters Tail refers to.
func (e Enqueue) Args() []any {
	tiers := make([]int16, len(e.planned))
	chans := make([]string, len(e.planned))
	afters := make([]int32, len(e.planned))
	for i, p := range e.planned {
		tiers[i], chans[i], afters[i] = p.Tier, p.Channel, int32(p.After.Seconds())
	}
	return []any{tiers, chans, afters}
}

// Done is called after the approval's transaction COMMITTED, with the counts the Tail statement
// returned. A raise that inserted an approval but no outbox row was suppressed by the run budget.
func (e Enqueue) Done(ctx context.Context, approvalID, runID uuid.UUID, raised, queued int64) {
	if len(e.planned) == 0 || raised == 0 || queued > 0 {
		return
	}
	for _, p := range e.planned {
		if p.Tier == 0 {
			counters.addSuppressed(p.Channel)
		}
	}
	if !suppressedOnce.first(runID, time.Now()) {
		return
	}
	data, _ := json.Marshal(map[string]any{"approval_id": approvalID, "limit": RunBudget})
	emit(ctx, types.AuditEvent{
		ID: uuid.New(), Time: time.Now().UTC(), RunID: &runID,
		ActorType: types.ActorSystem, Actor: auditActor,
		Action: "approval.notify.suppressed", Target: approvalID.String(), Outcome: "success",
		Data: data,
	})
}

// auditActor is the component name on every row this package writes.
const auditActor = "wardyn/approval-notify"

// emit records one audit row, loud on failure like every other audit writer.
func emit(ctx context.Context, ev types.AuditEvent) {
	r := rec.Load()
	if r == nil {
		return
	}
	if err := (*r).Record(ctx, ev); err != nil {
		audit.LogWriteFailure(ctx, ev, err)
	}
}

// hourly remembers when each run last got a suppression audit row, so a flood writes one row per run
// per hour rather than one per raise.
//
// ponytail: per replica and in memory, so N replicas may write up to N rows per run per hour; a
// database-backed window if that ever matters. Pruned on write, so it is bounded by the runs that
// were suppressed within the hour.
type hourly struct {
	mu   sync.Mutex
	last map[uuid.UUID]time.Time
}

var suppressedOnce = &hourly{last: map[uuid.UUID]time.Time{}}

// first reports whether runID has had no row in the last hour, and stamps it when so.
func (h *hourly) first(runID uuid.UUID, now time.Time) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, at := range h.last {
		if now.Sub(at) >= time.Hour {
			delete(h.last, id)
		}
	}
	if _, seen := h.last[runID]; seen {
		return false
	}
	h.last[runID] = now
	return true
}
