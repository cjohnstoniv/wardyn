// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	tickEvery    = 15 * time.Second
	claimBatch   = 50
	leaseFor     = 60 * time.Second // longer than sendTimeout, so a live send never loses its lease
	sendTimeout  = 10 * time.Second
	maxAttempts  = 5
	sendExpiry   = time.Hour // a reminder that arrives hours late is noise
	purgeBatch   = 500
	purgeOlderOf = "30 days"
)

// retryBackoff is the wait after the nth failed attempt (index n-1); the fifth failure is dead.
var retryBackoff = [maxAttempts - 1]time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute}

// Worker delivers due outbox rows. Every replica runs one: SKIP LOCKED keeps replicas off each other's
// rows and the lease is what recovers a row whose replica died mid-send.
type Worker struct {
	pool     *pgxpool.Pool
	channels map[string]Channel
	console  string
	masks    *secretmask.Registry
	client   *http.Client
}

// Deps are the worker's collaborators. Client is optional (tests inject one that trusts their server).
type Deps struct {
	Pool   *pgxpool.Pool
	Config *Config
	Masks  *secretmask.Registry
	Client *http.Client
}

// NewWorker builds a worker from a validated config. Call it after boot has applied the trusted CA, so
// the cloned transport carries it.
func NewWorker(d Deps) *Worker {
	w := &Worker{pool: d.Pool, channels: map[string]Channel{}, console: d.Config.ConsoleURL, masks: d.Masks, client: d.Client}
	for _, ch := range d.Config.Channels {
		w.channels[ch.ID] = ch
	}
	if w.client == nil {
		w.client = newClient()
	}
	return w
}

// Run ticks until ctx ends.
func (w *Worker) Run(ctx context.Context) {
	t := time.NewTicker(tickEvery)
	defer t.Stop()
	for {
		if _, err := w.Tick(ctx); err != nil && ctx.Err() == nil {
			slog.ErrorContext(ctx, "approval notify: tick failed", slog.String("error", classStore))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// claimed is one row this tick holds the lease on.
type claimed struct {
	id         uuid.UUID
	approvalID uuid.UUID
	tier       int16
	channel    string
	attempts   int16
	lease      time.Time
	expired    bool
}

// claimSQL takes at most claimBatch due rows (pending, or sending with a lapsed lease) in one
// statement, so no row is claimed without its lease being written.
const claimSQL = `
WITH due AS (
	SELECT id FROM approval_notifications
	 WHERE next_attempt_at <= now()
	   AND (state = 'pending' OR (state = 'sending' AND lease_until < now()))
	 ORDER BY next_attempt_at
	 LIMIT $1
	 FOR UPDATE SKIP LOCKED
)
UPDATE approval_notifications n
   SET state = 'sending', lease_until = now() + make_interval(secs => $2), attempts = n.attempts + 1
  FROM due
 WHERE n.id = due.id
RETURNING n.id, n.approval_id, n.tier, n.channel, n.attempts, n.lease_until,
          now() > n.due_at + make_interval(secs => $3)`

// Tick claims and delivers one batch, then purges old terminal rows. It returns how many rows it
// claimed.
func (w *Worker) Tick(ctx context.Context) (int, error) {
	rows, err := w.pool.Query(ctx, claimSQL, claimBatch, leaseFor.Seconds(), sendExpiry.Seconds())
	if err != nil {
		return 0, err
	}
	batch, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (c claimed, err error) {
		err = r.Scan(&c.id, &c.approvalID, &c.tier, &c.channel, &c.attempts, &c.lease, &c.expired)
		return c, err
	})
	if err != nil {
		return 0, err
	}
	var wg sync.WaitGroup
	for _, c := range batch {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.process(ctx, c)
		}()
	}
	wg.Wait()
	_, err = w.pool.Exec(ctx, `
		DELETE FROM approval_notifications WHERE id IN (
			SELECT id FROM approval_notifications
			 WHERE state IN ('sent','cancelled','dead') AND last_attempt_at < now() - interval '`+purgeOlderOf+`'
			 LIMIT $1)`, purgeBatch)
	return len(batch), err
}

const factsSQL = `
SELECT a.state, a.kind, a.requested_at, a.run_id, r.created_by, COALESCE(p.email, ''),
       r.governance_profile_id, COALESCE(gp.name, '')
  FROM approvals a
  JOIN agent_runs r ON r.id = a.run_id
  LEFT JOIN people p ON p.principal = r.created_by
  LEFT JOIN governance_profiles gp ON gp.id = r.governance_profile_id
 WHERE a.id = $1`

func (w *Worker) loadFacts(ctx context.Context, approvalID uuid.UUID) (approvalFacts, error) {
	f := approvalFacts{ApprovalID: approvalID}
	err := w.pool.QueryRow(ctx, factsSQL, approvalID).Scan(
		&f.State, &f.Kind, &f.RequestedAt, &f.RunID, &f.Principal, &f.Email, &f.ProfileID, &f.ProfileName)
	return f, err
}

// outcome is how a claimed row ends this attempt.
type outcome struct {
	state types.ApprovalNotifyState
	class string
	wait  time.Duration // next attempt delay, for state pending
}

func (w *Worker) process(ctx context.Context, c claimed) {
	facts, err := w.loadFacts(ctx, c.approvalID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		w.finalize(ctx, c, nil, outcome{state: types.NotifyCancelled})
		return
	case err != nil:
		w.finalize(ctx, c, nil, w.failed(c, result{class: classStore, retryable: true}))
		return
	case facts.State != types.ApprovalPending:
		w.finalize(ctx, c, &facts, outcome{state: types.NotifyCancelled})
		return
	case c.expired:
		w.finalize(ctx, c, &facts, outcome{state: types.NotifyDead, class: classExpired})
		return
	}
	ch, ok := w.channels[c.channel]
	if !ok {
		w.finalize(ctx, c, &facts, outcome{state: types.NotifyDead, class: classUnknownChannel})
		return
	}
	body, err := buildPayload(c.id, c.tier, facts, w.console, w.masks.Masker(facts.RunID))
	if err != nil {
		w.finalize(ctx, c, &facts, outcome{state: types.NotifyDead, class: classStore})
		return
	}
	sendCtx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	res := ch.send(sendCtx, w.client, c.id.String(), body, time.Now())
	if res.class == "" {
		w.finalize(ctx, c, &facts, outcome{state: types.NotifySent})
		return
	}
	w.finalize(ctx, c, &facts, w.failed(c, res))
}

// failed turns a failed send into a retry or a dead row.
func (w *Worker) failed(c claimed, res result) outcome {
	if !res.retryable || int(c.attempts) >= maxAttempts {
		return outcome{state: types.NotifyDead, class: res.class}
	}
	return outcome{state: types.NotifyPending, class: res.class, wait: retryBackoff[c.attempts-1]}
}

// finalizeSQL is a compare-and-set on (id, lease_until): a replica whose lease was reclaimed cannot
// overwrite the new claimant's result. Every finalise stamps last_attempt_at.
const finalizeSQL = `
UPDATE approval_notifications
   SET state = $3::text, last_error = $4, last_attempt_at = now(), lease_until = NULL,
       sent_at = CASE WHEN $3::text = 'sent' THEN now() ELSE sent_at END,
       next_attempt_at = CASE WHEN $3::text = 'pending' THEN now() + make_interval(secs => $5) ELSE next_attempt_at END
 WHERE id = $1 AND lease_until = $2 AND state = 'sending'`

func (w *Worker) finalize(ctx context.Context, c claimed, facts *approvalFacts, o outcome) {
	// A canceled parent context (shutdown) must not strand a finished send as "sending" until the lease lapses.
	ctx = context.WithoutCancel(ctx)
	tag, err := w.pool.Exec(ctx, finalizeSQL, c.id, c.lease, string(o.state), o.class, o.wait.Seconds())
	if err != nil || tag.RowsAffected() == 0 {
		return // the lease was reclaimed: the new claimant's result stands
	}
	if o.class != "" {
		slog.WarnContext(ctx, "approval notify: delivery did not succeed",
			slog.String("channel", c.channel), slog.Int("tier", int(c.tier)), slog.Int("attempts", int(c.attempts)),
			slog.String("state", string(o.state)), slog.String("error", o.class))
	}
	if o.state != types.NotifyDead {
		return
	}
	counters.addFailed(c.channel)
	ev := types.AuditEvent{
		ID: uuid.New(), Time: time.Now().UTC(), ActorType: types.ActorSystem, Actor: auditActor,
		Action: "approval.notify.failed", Target: c.approvalID.String(), Outcome: "failure",
	}
	if facts != nil {
		ev.RunID = &facts.RunID
	}
	ev.Data, _ = json.Marshal(map[string]any{
		"approval_id": c.approvalID, "channel": c.channel, "tier": c.tier, "attempts": c.attempts, "error": o.class,
	})
	emit(ctx, ev)
}
