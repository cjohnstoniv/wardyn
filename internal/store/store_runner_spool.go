// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/runnerspool"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ErrRunnerSpoolRefused does not distinguish an unknown run from a foreign or
// revoked runner. A failed checked stream event must not receive an ACK.
var ErrRunnerSpoolRefused = errors.New("runner audit replay refused")

// IngestRunnerDecision atomically appends audit history and advances its replay
// receipt. Terminal-run acceptance is audit only: this transaction never touches
// lifecycle, idle clocks, credentials, grants, approvals or token renewal.
func (s PG) IngestRunnerDecision(ctx context.Context, runnerID uuid.UUID, row runnerspool.Record, prepare func(context.Context, types.AuditEvent) (types.AuditEvent, error)) (bool, error) {
	if runnerID == uuid.Nil || prepare == nil || !validRunnerDecision(row) {
		return false, ErrRunnerSpoolRefused
	}
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after a successful commit is harmless
	if _, err := tx.Exec(ctx, db.AuditChainLockTimeoutSQL()); err != nil {
		return false, err
	}
	run, err := runnerSpoolRun(ctx, tx, runnerID, row.Decision.Request.RunID)
	if err != nil {
		return false, err
	}
	cursor, err := lockRunnerSpool(ctx, tx, runnerID, row)
	if err != nil {
		return false, err
	}
	duplicate, err := cursor.accept(row)
	if err != nil {
		return false, err
	}
	if duplicate {
		return false, tx.Commit(ctx)
	}
	events, err := runnerDecisionEvents(ctx, tx, runnerID, run.ID, row)
	if err != nil {
		return false, err
	}
	for i, ev := range events {
		ev, err = prepare(ctx, ev)
		if err != nil {
			return false, err
		}
		if err := InsertAuditEventTx(ctx, tx, &ev); err != nil {
			return false, err
		}
		events[i] = ev
	}
	hashes, err := json.Marshal(cursor.hashes)
	if err != nil {
		return false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE runner_spool_cursors SET last_seq=$2,recent_hashes=$3,overflow_end=$4::text::numeric,overflow_hash=$5 WHERE run_id=$1`, run.ID, cursor.last, hashes, strconv.FormatUint(cursor.overflow, 10), cursor.overflowHash); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	if s.SIEM != nil {
		for _, ev := range events {
			_ = s.SIEM.Emit(context.WithoutCancel(ctx), ev)
		}
	}
	return true, nil
}

func validRunnerDecision(r runnerspool.Record) bool {
	if !r.ValidHash() || r.Decision.Request.RunID == uuid.Nil || r.Seq < 0 {
		return false
	}
	if _, err := r.Marshal(); err != nil {
		return false
	}
	switch r.Decision.Decision {
	case egress.Allow, egress.Deny, egress.Pending:
	default:
		return false
	}
	if r.Seq == 0 {
		return r.Count > 0 && r.DropEnd >= r.Count && r.Decision.Decision == egress.Deny && r.Decision.RuleSource == "builtin:spool-full"
	}
	return r.Count == 0 && r.DropEnd == 0
}

// runnerSpoolRun's FOR SHARE on the runner row can starve RevokeRunner only if ingest for one runner ever
// runs concurrently: its 5 s revoke bound rests on one session's events being ingested in order on its read loop.
func runnerSpoolRun(ctx context.Context, tx pgx.Tx, runnerID, runID uuid.UUID) (types.AgentRun, error) {
	var kind, owner, state string
	var revoked *time.Time
	err := tx.QueryRow(ctx, `SELECT owner_kind,owner,state,revoked_at FROM runners WHERE id=$1 FOR SHARE`, runnerID).Scan(&kind, &owner, &state, &revoked)
	if errors.Is(err, pgx.ErrNoRows) || state != string(types.RunnerClaimed) || revoked != nil {
		return types.AgentRun{}, ErrRunnerSpoolRefused
	}
	if err != nil {
		return types.AgentRun{}, err
	}
	run, err := scanRun(tx.QueryRow(ctx, `SELECT `+runCols+` FROM agent_runs WHERE id=$1 FOR SHARE`, runID))
	if errors.Is(err, ErrNotFound) {
		return run, ErrRunnerSpoolRefused
	}
	if err != nil {
		return run, err
	}
	if run.RunnerID == nil || *run.RunnerID != runnerID || run.Placement != types.PlacementLocal || !runnerReportsOn(kind, owner, run.CreatedBy) {
		return run, ErrRunnerSpoolRefused
	}
	if run.State != types.RunRunning && !run.State.IsTerminal() {
		return run, ErrRunnerSpoolRefused
	}
	return run, nil
}

type spoolReceipt struct {
	last         int64
	hashes       map[string]string
	overflow     uint64
	overflowHash string
}

func lockRunnerSpool(ctx context.Context, tx pgx.Tx, runnerID uuid.UUID, row runnerspool.Record) (spoolReceipt, error) {
	var c spoolReceipt
	if _, err := tx.Exec(ctx, `INSERT INTO runner_spool_cursors(run_id,runner_id,spool_id) VALUES($1,$2,$3) ON CONFLICT(run_id) DO NOTHING`, row.Decision.Request.RunID, runnerID, row.SpoolID); err != nil {
		return c, err
	}
	var storedRunner, spool uuid.UUID
	var b []byte
	var overflow string
	err := tx.QueryRow(ctx, `SELECT runner_id,spool_id,last_seq,recent_hashes,overflow_end::text,overflow_hash FROM runner_spool_cursors WHERE run_id=$1 FOR UPDATE`, row.Decision.Request.RunID).Scan(&storedRunner, &spool, &c.last, &b, &overflow, &c.overflowHash)
	if err != nil {
		return c, err
	}
	if storedRunner != runnerID || spool != row.SpoolID {
		return c, ErrRunnerSpoolRefused
	}
	if err := json.Unmarshal(b, &c.hashes); err != nil {
		return c, err
	}
	c.overflow, err = strconv.ParseUint(overflow, 10, 64)
	if err != nil {
		return c, err
	}
	if c.hashes == nil {
		return c, ErrRunnerSpoolRefused
	}
	return c, nil
}
func (c *spoolReceipt) accept(r runnerspool.Record) (bool, error) {
	if r.Seq == 0 {
		if r.DropEnd == c.overflow && r.Hash == c.overflowHash {
			return true, nil
		}
		if r.DropEnd <= c.overflow || r.Count != r.DropEnd-c.overflow {
			return false, ErrRunnerSpoolRefused
		}
		c.overflow, c.overflowHash = r.DropEnd, r.Hash
		return false, nil
	}
	key := strconv.FormatInt(r.Seq, 10)
	if r.Seq <= c.last {
		if c.hashes[key] == r.Hash {
			return true, nil
		}
		return false, ErrRunnerSpoolRefused
	}
	if r.Seq != c.last+1 {
		return false, ErrRunnerSpoolRefused
	}
	c.last = r.Seq
	c.hashes[key] = r.Hash
	// A sender commits its local cursor after each shared batch. Retain exactly
	// the largest uncommitted batch even after the corresponding audit is erased.
	for k := range c.hashes {
		seq, err := strconv.ParseInt(k, 10, 64)
		if err != nil {
			return false, err
		}
		if seq <= c.last-runnerspool.ReplayBatch {
			delete(c.hashes, k)
		}
	}
	return false, nil
}

func runnerDecisionEvents(ctx context.Context, tx pgx.Tx, runnerID, runID uuid.UUID, r runnerspool.Record) ([]types.AuditEvent, error) {
	var at time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&at); err != nil {
		return nil, err
	}
	d := r.Decision
	data := map[string]any{"host": d.Request.Host, "port": d.Request.Port, "method": d.Request.Method, "path": d.Request.Path, "rule_source": d.RuleSource, "approval_id": d.ApprovalID, "observed_at": d.Request.Time, "spooled": true, "evidence_source": types.RunEvidenceRunnerAsserted, "spool_id": r.SpoolID, "spool_seq": r.Seq, "spool_hash": r.Hash}
	if r.Count > 0 {
		data["count"] = r.Count
	}
	if d.Repeat > 0 {
		data["repeat"] = d.Repeat
	}
	if d.Cause != "" {
		data["cause"] = d.Cause
	}
	if d.Via != "" {
		data["via"] = d.Via
	}
	if d.UpstreamFault != "" {
		data["upstream_fault"] = d.UpstreamFault
	}
	b, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	outcome := "success"
	if d.Decision == egress.Deny {
		outcome = "denied"
	}

	base := types.AuditEvent{Time: at, RunID: &runID, ActorType: types.ActorAgent, Actor: "runner:" + runnerID.String(), Target: d.Request.Host}
	var events []types.AuditEvent
	if d.Scan == nil || d.Scan.Action != "bypass" {
		ev := base
		ev.ID = uuid.New()
		ev.Action = "egress." + string(d.Decision)
		ev.Outcome = outcome
		ev.Data = b
		events = append(events, ev)
	}
	if d.Scan != nil {
		action, result, fields := egress.ScanAudit(d.Scan, d.Request.Host)
		var scan map[string]any
		if err := json.Unmarshal(fields, &scan); err != nil {
			return nil, err
		}
		for _, key := range []string{"observed_at", "spooled", "evidence_source", "spool_id", "spool_seq", "spool_hash"} {
			scan[key] = data[key]
		}
		fields, err := json.Marshal(scan)
		if err != nil {
			return nil, err
		}
		ev := base
		ev.ID = uuid.New()
		ev.Action = action
		ev.Outcome = result
		ev.Data = fields
		events = append(events, ev)
	}
	return events, nil
}
