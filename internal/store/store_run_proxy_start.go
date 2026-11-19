// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Proxy start modes (run_proxy_starts.mode).
const (
	// ProxyStartExpected is a start that must confirm: both hashes in and equal.
	ProxyStartExpected = "expected"
	// ProxyStartLegacy is a start on positive evidence of an older runner or proxy image.
	ProxyStartLegacy = "legacy"
)

// Reasons a start fails (ProxyStartChange.Reason).
const (
	ProxyStartHashMissing  = "hash_missing"
	ProxyStartHashMismatch = "hash_mismatch"
	ProxyStartDeadline     = "deadline"
)

// ProxyStart is one row of run_proxy_starts.
type ProxyStart struct {
	RunID      uuid.UUID
	Generation int
	JTI        string
	Mode       string
	// InPlace is a start a live egress edit applied by narrowing the run's running proxy, not by replacing it.
	InPlace        bool
	StartedAt      time.Time
	Deadline       *time.Time
	ExpectedSHA256 string
	ReportedSHA256 string
	State          types.ProxyStartState
	ConfirmedAt    *time.Time
	// ContainedAt is when a failed start's proxy stop was confirmed; ContainmentError is why it is not
	// yet (unresolved), empty before the first attempt.
	ContainedAt      *time.Time
	ContainmentError string
}

// ProxyStartChange is what one write did. Active says the write reached the run's active start (the
// newest one not abandoned) and, for a report, that the presenting token is in that start's lineage;
// false is a stale write that changed nothing. Settled is the state the write moved the start to,
// empty when it stayed where it was; Reason says why a failed start failed.
type ProxyStartChange struct {
	Start   ProxyStart
	Active  bool
	Settled types.ProxyStartState
	Reason  string
}

// RunProxyStarts records each proxy start's policy readback. Optional, as RunPolicyGenerations is: the
// api layer type-asserts, and production is always PG. Every time is the database's clock, the one
// token renewals are stamped with.
type RunProxyStarts interface {
	// BeginProxyStart records a pending start under generation gen on the fresh token jti, before its
	// proxy starts. A start already recorded under gen is reset to it. inPlace says a live egress edit
	// applies it by narrowing the running proxy in place.
	BeginProxyStart(ctx context.Context, runID uuid.UUID, gen int, jti, mode string, inPlace bool) (ProxyStart, error)
	// AnswerProxyStart records the driver's successful answer for gen: its hash (empty when it sent
	// none) and a report deadline grace from now. It lands only while gen is the active pending start;
	// older pending starts are then superseded.
	AnswerProxyStart(ctx context.Context, runID uuid.UUID, gen int, sha string, grace time.Duration) (ProxyStartChange, error)
	// ReportProxyStart records the proxy's hash under the token jti: it lands only on the active start,
	// and only when jti is that start's own or, renewed since the start, the run's current or previous one.
	ReportProxyStart(ctx context.Context, runID uuid.UUID, jti, sha string) (ProxyStartChange, error)
	// AbandonProxyStart marks gen abandoned while it is still pending: a revive that never replaced the old proxy.
	AbandonProxyStart(ctx context.Context, runID uuid.UUID, gen int) error
	// ExpireProxyStart fails gen when it is the active pending start and its deadline has passed.
	ExpireProxyStart(ctx context.Context, runID uuid.UUID, gen int) (ProxyStartChange, error)
	// OverdueProxyStarts lists the pending starts whose deadline has passed, and the failed starts
	// still each run's active one whose proxy stop is not yet confirmed.
	OverdueProxyStarts(ctx context.Context) ([]ProxyStart, error)
	// RecordProxyContainment records the proxy stop of failed start gen: confirmed when stopErr is
	// empty, unresolved (stopErr) otherwise. A start already contained keeps that.
	RecordProxyContainment(ctx context.Context, runID uuid.UUID, gen int, stopErr string) error
	// ActiveProxyStart reads the run's active start; found is false for a run with none.
	ActiveProxyStart(ctx context.Context, runID uuid.UUID) (start ProxyStart, found bool, err error)
	// SetRunProxyAddr records the address the run's agent is pinned to for its proxy.
	SetRunProxyAddr(ctx context.Context, runID uuid.UUID, addr string) error
}

var _ RunProxyStarts = PG{}

// SettleProxyStart is the one comparison: where an expected start stands at now. A passed deadline
// fails it; both hashes in and equal enforce it, different fail it; otherwise it stays pending.
func SettleProxyStart(p ProxyStart, now time.Time) (types.ProxyStartState, string) {
	switch {
	case p.State != types.ProxyStartPending || p.Mode != ProxyStartExpected:
		return p.State, ""
	case p.Deadline != nil && !now.Before(*p.Deadline):
		return types.ProxyStartFailed, ProxyStartDeadline
	case p.ExpectedSHA256 == "" || p.ReportedSHA256 == "":
		return types.ProxyStartPending, ""
	case p.ExpectedSHA256 == p.ReportedSHA256:
		return types.ProxyStartEnforced, ""
	default:
		return types.ProxyStartFailed, ProxyStartHashMismatch
	}
}

const proxyStartCols = `run_id, generation, jti, mode, in_place, started_at, deadline, COALESCE(expected_sha256, ''),
	COALESCE(reported_sha256, ''), state, confirmed_at, contained_at, COALESCE(containment_error, '')`

func scanProxyStart(row pgx.Row, extra ...any) (ProxyStart, error) {
	var p ProxyStart
	var state string
	err := row.Scan(append([]any{&p.RunID, &p.Generation, &p.JTI, &p.Mode, &p.InPlace, &p.StartedAt, &p.Deadline,
		&p.ExpectedSHA256, &p.ReportedSHA256, &state, &p.ConfirmedAt, &p.ContainedAt, &p.ContainmentError}, extra...)...)
	p.State = types.ProxyStartState(state)
	return p, err
}

// BeginProxyStart — see RunProxyStarts.
func (s PG) BeginProxyStart(ctx context.Context, runID uuid.UUID, gen int, jti, mode string, inPlace bool) (ProxyStart, error) {
	p, err := scanProxyStart(s.Pool.QueryRow(ctx, `
		INSERT INTO run_proxy_starts (run_id, generation, jti, mode, in_place, started_at, state)
		VALUES ($1, $2, $3, $4, $5, now(), 'pending')
		ON CONFLICT (run_id, generation) DO UPDATE SET jti = EXCLUDED.jti, mode = EXCLUDED.mode, in_place = EXCLUDED.in_place,
			started_at = now(), deadline = NULL, expected_sha256 = NULL, reported_sha256 = NULL,
			state = 'pending', confirmed_at = NULL, contained_at = NULL, containment_error = NULL
		RETURNING `+proxyStartCols, runID, gen, jti, mode, inPlace))
	if err != nil {
		return ProxyStart{}, fmt.Errorf("store: begin proxy start: %w", err)
	}
	return p, nil
}

// lockActiveProxyStart reads and locks the run's active start and the database's now.
func lockActiveProxyStart(ctx context.Context, tx pgx.Tx, runID uuid.UUID) (ProxyStart, time.Time, bool, error) {
	var now time.Time
	p, err := scanProxyStart(tx.QueryRow(ctx, `SELECT `+proxyStartCols+`, now() FROM run_proxy_starts
		WHERE run_id = $1 AND state <> 'abandoned' ORDER BY generation DESC LIMIT 1 FOR UPDATE`, runID), &now)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProxyStart{}, time.Time{}, false, nil
	}
	if err != nil {
		return ProxyStart{}, time.Time{}, false, fmt.Errorf("store: read active proxy start: %w", err)
	}
	return p, now, true, nil
}

// writeProxyStart stores p's outcome fields.
func writeProxyStart(ctx context.Context, tx pgx.Tx, p ProxyStart) error {
	_, err := tx.Exec(ctx, `UPDATE run_proxy_starts SET deadline = $3, expected_sha256 = NULLIF($4, ''),
		reported_sha256 = NULLIF($5, ''), state = $6, confirmed_at = $7 WHERE run_id = $1 AND generation = $2`,
		p.RunID, p.Generation, p.Deadline, p.ExpectedSHA256, p.ReportedSHA256, string(p.State), p.ConfirmedAt)
	if err != nil {
		return fmt.Errorf("store: write proxy start: %w", err)
	}
	return nil
}

// settle applies SettleProxyStart at now to ch.Start and records what moved.
func settle(ch *ProxyStartChange, now time.Time) {
	state, reason := SettleProxyStart(ch.Start, now)
	if state == ch.Start.State {
		return
	}
	ch.Start.State, ch.Settled, ch.Reason = state, state, reason
	if state == types.ProxyStartEnforced {
		ch.Start.ConfirmedAt = &now
	}
}

// AnswerProxyStart — see RunProxyStarts.
func (s PG) AnswerProxyStart(ctx context.Context, runID uuid.UUID, gen int, sha string, grace time.Duration) (ProxyStartChange, error) {
	var ch ProxyStartChange
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		p, now, found, err := lockActiveProxyStart(ctx, tx, runID)
		if err != nil || !found {
			return err
		}
		ch.Start = p
		if p.Generation != gen || p.State != types.ProxyStartPending {
			return nil
		}
		ch.Active = true
		switch {
		case p.Mode == ProxyStartLegacy:
			ch.Start.State, ch.Settled = types.ProxyStartUnconfirmed, types.ProxyStartUnconfirmed
		case sha == "":
			ch.Start.State, ch.Settled, ch.Reason = types.ProxyStartFailed, types.ProxyStartFailed, ProxyStartHashMissing
		default:
			deadline := now.Add(grace)
			ch.Start.ExpectedSHA256, ch.Start.Deadline = sha, &deadline
			settle(&ch, now)
		}
		if err := writeProxyStart(ctx, tx, ch.Start); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE run_proxy_starts SET state = 'superseded'
			WHERE run_id = $1 AND generation < $2 AND state = 'pending'`, runID, gen); err != nil {
			return fmt.Errorf("store: supersede older proxy starts: %w", err)
		}
		return nil
	})
	return ch, err
}

// ReportProxyStart — see RunProxyStarts. The run row is read FOR SHARE, so a renewal or a lineage
// retirement in flight lands wholly before or after the check.
func (s PG) ReportProxyStart(ctx context.Context, runID uuid.UUID, jti, sha string) (ProxyStartChange, error) {
	var ch ProxyStartChange
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		p, now, found, err := lockActiveProxyStart(ctx, tx, runID)
		if err != nil || !found {
			return err
		}
		ch.Start = p
		var inLineage bool
		err = tx.QueryRow(ctx, `SELECT $2 = $3 OR (token_renewed_at >= $4
			AND $2 IN (COALESCE(token_jti, ''), COALESCE(token_prev_jti, '')))
			FROM agent_runs WHERE id = $1 FOR SHARE`, runID, jti, p.JTI, p.StartedAt).Scan(&inLineage)
		if err != nil {
			return fmt.Errorf("store: read run token lineage: %w", err)
		}
		if !inLineage || jti == "" {
			return nil
		}
		ch.Active = true
		if p.State != types.ProxyStartPending {
			return nil
		}
		ch.Start.ReportedSHA256 = sha
		settle(&ch, now)
		return writeProxyStart(ctx, tx, ch.Start)
	})
	return ch, err
}

// AbandonProxyStart — see RunProxyStarts.
func (s PG) AbandonProxyStart(ctx context.Context, runID uuid.UUID, gen int) error {
	if _, err := s.Pool.Exec(ctx, `UPDATE run_proxy_starts SET state = 'abandoned'
		WHERE run_id = $1 AND generation = $2 AND state = 'pending'`, runID, gen); err != nil {
		return fmt.Errorf("store: abandon proxy start: %w", err)
	}
	return nil
}

// ExpireProxyStart — see RunProxyStarts.
func (s PG) ExpireProxyStart(ctx context.Context, runID uuid.UUID, gen int) (ProxyStartChange, error) {
	var ch ProxyStartChange
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		p, now, found, err := lockActiveProxyStart(ctx, tx, runID)
		if err != nil || !found {
			return err
		}
		ch.Start = p
		if p.Generation != gen || p.State != types.ProxyStartPending || p.Deadline == nil || now.Before(*p.Deadline) {
			return nil
		}
		ch.Active = true
		settle(&ch, now)
		return writeProxyStart(ctx, tx, ch.Start)
	})
	return ch, err
}

// OverdueProxyStarts — see RunProxyStarts.
func (s PG) OverdueProxyStarts(ctx context.Context) ([]ProxyStart, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+proxyStartCols+` FROM run_proxy_starts
		WHERE (state = 'pending' AND deadline IS NOT NULL AND deadline <= now())
			OR (state = 'failed' AND contained_at IS NULL AND generation = (SELECT max(o.generation)
				FROM run_proxy_starts o WHERE o.run_id = run_proxy_starts.run_id AND o.state <> 'abandoned'))`)
	if err != nil {
		return nil, fmt.Errorf("store: list overdue proxy starts: %w", err)
	}
	defer rows.Close()
	var out []ProxyStart
	for rows.Next() {
		p, err := scanProxyStart(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan proxy start: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ActiveProxyStart — see RunProxyStarts.
func (s PG) ActiveProxyStart(ctx context.Context, runID uuid.UUID) (ProxyStart, bool, error) {
	p, err := scanProxyStart(s.Pool.QueryRow(ctx, `SELECT `+proxyStartCols+` FROM run_proxy_starts
		WHERE run_id = $1 AND state <> 'abandoned' ORDER BY generation DESC LIMIT 1`, runID))
	if errors.Is(err, pgx.ErrNoRows) {
		return ProxyStart{}, false, nil
	}
	if err != nil {
		return ProxyStart{}, false, fmt.Errorf("store: read active proxy start: %w", err)
	}
	return p, true, nil
}

// RecordProxyContainment — see RunProxyStarts.
func (s PG) RecordProxyContainment(ctx context.Context, runID uuid.UUID, gen int, stopErr string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE run_proxy_starts
		SET contained_at = CASE WHEN $3 = '' THEN now() END, containment_error = NULLIF($3, '')
		WHERE run_id = $1 AND generation = $2 AND state = 'failed' AND contained_at IS NULL`, runID, gen, stopErr)
	if err != nil {
		return fmt.Errorf("store: record proxy containment: %w", err)
	}
	return nil
}

// SetRunProxyAddr — see RunProxyStarts.
func (s PG) SetRunProxyAddr(ctx context.Context, runID uuid.UUID, addr string) error {
	return s.execRun(ctx, "set run proxy address",
		`UPDATE agent_runs SET proxy_addr = NULLIF($1, ''), updated_at = now() WHERE id = $2`, addr, runID)
}
