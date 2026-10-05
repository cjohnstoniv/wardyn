// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Revive and restart with current limits (long-holds design rev 4, RL-10 and
// RL-11; migration 0084): the claim a revive makes before it replaces a run's
// proxy, and the read the admin version-window listing makes.
package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// RunReviver is the revive surface. Optional for RunLeaser's reason: the test
// doubles that embed store.Store would route these calls to a nil interface.
// The api layer type-asserts; production is always PG.
type RunReviver interface {
	// MarkRunRevived claims run id for a new proxy: while it is RUNNING and still as the revive read
	// it, live (from "") or lost to an outage or a reboot (from that reason), it clears the lost
	// mark (and any unresolved containment error — the old proxy it was about is replaced), stamps
	// the token as just renewed (the revive mints a fresh one, and the lapsed-token sweep must not
	// read the old stamp), and refreshes the watcher lease (a stopped agent starts only after the
	// new proxy, and the watcher sweep must not probe it before then).
	//
	// A run kept by its own end (from LostEnded) is claimed only with ended set: its exact mark,
	// its files grace still live, and its end after ended.Now. It also clears a pause ONLY when
	// from is a reboot: that revive restarts the agent container (docker start), so a stale pause
	// mark would 409 the run's files/resources reads over an agent that's actually running. A live
	// restart (from "") or an outage revive never touch the agent's own process, frozen or not, so
	// their pause mark, if any, must survive for the thaw an eventual resume still needs to
	// perform (a lease end already cleared the pause of a run kept by its own end).
	//
	// false means the run went terminal, ended, was lost or revived since, or its grace or end ran
	// out (an outage-kept run's end too, as revive admission requires) — it gets no proxy.
	//
	// startsAgent is a revive that starts the run's stopped agent (a reboot, an end, or an
	// outage whose agent was stopped). With limit > 0 its claim takes a slot under the
	// deployment cap like a create does: under CreateRunUnderCap's lock,
	// ErrRunCapReached when every OTHER run that holds a sandbox already reaches limit,
	// the run left as it was. The row's own count is not trusted for this: an outage-kept
	// run whose end was moved later reads as holding a sandbox though its agent is
	// stopped. A revive that starts no agent (a live restart, an outage revive whose agent
	// still runs) adds nothing and is never refused.
	MarkRunRevived(ctx context.Context, id uuid.UUID, from types.LostReason, ended *EndedKept, limit int, startsAgent bool) (bool, error)
	// SetRunProxyRelease records release as the one that started run id's
	// proxy, once a revive's new proxy runs.
	SetRunProxyRelease(ctx context.Context, id uuid.UUID, release string) error
	// ListRunProxyReleases returns every non-terminal run that has a sandbox,
	// with the release that started its proxy ("" before migration 0084).
	ListRunProxyReleases(ctx context.Context) ([]RunProxyRelease, error)
}

// RunProxyRelease is one row of the version-window listing.
type RunProxyRelease struct {
	RunID      uuid.UUID        `json:"run_id"`
	CreatedBy  string           `json:"created_by"`
	State      types.RunState   `json:"state"`
	LostReason types.LostReason `json:"lost_reason,omitempty"`
	Release    string           `json:"proxy_release"`
}

var _ RunReviver = PG{}

// MarkRunRevived — see RunReviver.
func (s PG) MarkRunRevived(ctx context.Context, id uuid.UUID, from types.LostReason, ended *EndedKept, limit int, startsAgent bool) (bool, error) {
	switch from {
	case "", types.LostOutage, types.LostReboot:
	case types.LostEnded:
		if ended == nil {
			return false, nil
		}
	default:
		return false, nil
	}
	if limit > 0 && startsAgent {
		return s.markRunRevivedUnderCap(ctx, id, from, ended, limit)
	}
	return markRunRevived(ctx, s.Pool, id, from, ended)
}

// markRunRevivedUnderCap is the claim of a revive that starts an agent, under the
// deployment cap: the count and the claim share CreateRunUnderCap's transaction-scoped
// advisory lock and its counting predicate, so a revive and a create racing at the cap
// admit exactly the cap. It counts the OTHER runs in one statement: the run's own
// row is never read for admission, so an extension of it landing mid-claim (which
// takes neither lock) cannot make it look counted to one read and not to another.
//
// The claim does not pin the outage run's end either: admission does not depend on
// it, and an extension the owner makes meanwhile is legitimate, not a change that
// should fail the revive.
func (s PG) markRunRevivedUnderCap(ctx context.Context, id uuid.UUID, from types.LostReason, ended *EndedKept, limit int) (claimed bool, err error) {
	err = s.inTx(ctx, func(q Querier) error {
		others, err := lockAndCountActiveRuns(ctx, q, id)
		if err != nil {
			return err
		}
		if others >= limit {
			return ErrRunCapReached
		}
		claimed, err = markRunRevived(ctx, q, id, from, ended)
		return err
	})
	return claimed && err == nil, err
}

func markRunRevived(ctx context.Context, q Querier, id uuid.UUID, from types.LostReason, ended *EndedKept) (bool, error) {
	lostAt, keptAfter, now := endedArgs(ended)
	tag, err := q.Exec(ctx, `
		UPDATE agent_runs SET lost_at=NULL, lost_reason='', containment_error=NULL, containment_error_at=NULL,
			paused_at=CASE WHEN $3=$8 THEN NULL ELSE paused_at END,
			paused_reason=CASE WHEN $3=$8 THEN '' ELSE paused_reason END,
			token_renewed_at=now(), watcher_heartbeat=now(), updated_at=now()
		WHERE id=$1 AND state=$2 AND (lost_at IS NOT NULL) = ($3 <> '') AND lost_reason=$3
		  AND ($3 <> $4 OR (lost_at = $5 AND lost_at > $6::timestamptz AND (ends_at IS NULL OR ends_at > $7::timestamptz)))
		  AND ($3 <> $9 OR ends_at IS NULL OR ends_at > now())`,
		id, string(types.RunRunning), string(from), string(types.LostEnded), lostAt, keptAfter, now, string(types.LostReboot), string(types.LostOutage))
	if err != nil {
		return false, fmt.Errorf("store: mark run revived: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// SetRunProxyRelease — see RunReviver.
func (s PG) SetRunProxyRelease(ctx context.Context, id uuid.UUID, release string) error {
	if _, err := s.Pool.Exec(ctx, `UPDATE agent_runs SET proxy_release=$2, updated_at=now() WHERE id=$1`, id, release); err != nil {
		return fmt.Errorf("store: set run proxy release: %w", err)
	}
	return nil
}

// ListRunProxyReleases — see RunReviver.
func (s PG) ListRunProxyReleases(ctx context.Context) ([]RunProxyRelease, error) {
	states := make([]string, 0, len(types.NonTerminalRunStates))
	for _, st := range types.NonTerminalRunStates {
		states = append(states, string(st))
	}
	q := `SELECT id, created_by, state, lost_reason, proxy_release FROM agent_runs
		WHERE state = ANY($1) AND sandbox_ref <> '' ORDER BY created_at`
	return collect(ctx, s.Pool, "list", "run proxy releases", q, []any{states},
		func(row pgx.Row) (RunProxyRelease, error) {
			var r RunProxyRelease
			var state, reason string
			err := row.Scan(&r.RunID, &r.CreatedBy, &state, &reason, &r.Release)
			r.State, r.LostReason = types.RunState(state), types.LostReason(reason)
			return r, err
		})
}
