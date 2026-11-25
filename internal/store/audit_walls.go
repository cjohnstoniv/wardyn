// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The Walls view reads egress decision rows only through the accessors in this file: DecodeDecisionRow is the
// one reader of their data keys, and the SQL here names columns, never a data key.

// DecisionRow is one egress.* audit row, decoded once. DecodeDecisionRow is the only reader of its data keys.
type DecisionRow struct {
	ID            uuid.UUID // audit_events.id (types.AuditEvent.ID)
	RunID         uuid.UUID // audit_events.run_id, zero when NULL (types.AuditEvent.RunID)
	Seq           int64     // audit_events.seq, commit order; never sent to a client
	RecordedAt    time.Time // audit_events.recorded_at, the append time; never behind seq
	Time          time.Time // audit_events.time, the writer's clock (types.AuditEvent.Time)
	Action        string    // audit_events.action: egress.allow, egress.deny or egress.hold; egress.pending reads as egress.hold
	Actor         string    // audit_events.actor, the agent's SPIFFE id on a proxy post (handlePostDecision)
	Host          string    // data.host (handlePostDecision; runnerDecisionEvents)
	Port          int       // data.port (handlePostDecision)
	RuleSource    string    // data.rule_source (egress.DecisionLog.RuleSource)
	ApprovalID    string    // data.approval_id, "" when null (egress.DecisionLog.ApprovalID)
	By            string    // data.by, whose refusal (egress.Refusal.By)
	Reason        string    // data.refusal_reason (egress.Refusal.AuditFields)
	LayerName     string    // data.layer_name, the refusing layer's name (egress.Refusal.LayerName)
	MatchedRule   string    // data.matched_rule, the tool_rules key (egress.DecisionLog.MatchedRule)
	Weight        int64     // the row's multiplicity, at least 1: data.repeat (egress.DecisionLog.Repeat), else data.count (runnerDecisionEvents), else the <n> of egress:dropped-decisions-<n> (decisionSink.reportDropped), else the leading count in Cause on a builtin:spool-lost row (runnerspool.Spool.EmitLoss), else 1
	DecidedAt     time.Time // data.observed_at (runnerDecisionEvents), else Time
	Cause         string    // data.cause (egress.DecisionLog.Cause)
	Flow          string    // data.flow, a desktop VM relay flow id (egress.DecisionLog.Flow)
	UpstreamFault string    // data.upstream_fault, the destination's error class (egress.DecisionLog.UpstreamFault)
	Via           string    // data.via, the hop class: "upstream-proxy" or "direct", never an address (egress.DecisionLog.Via)
	// ContactLost, ContactAge and Owner have no writer yet, so they decode to their zero values.
	ContactLost bool          // data.cp_contact == "lost"
	ContactAge  time.Duration // data.contact_age_s, in seconds
	Owner       string        // data.run_owner, never a bare data.owner
	Sponsor     string        // data.sponsor (audit.StampSponsor)
	Workload    string        // data.run_workload, else Actor on an agent row (handlePostDecision)
	Format      int           // the key set's version: 1
}

// Late is a row recorded more than 45 s after it was decided: RecordedAt - DecidedAt > 45 s.
func (r DecisionRow) Late() bool { return r.RecordedAt.Sub(r.DecidedAt) > auditWallsLateAfter }

// DecisionCursor is the last row a read scanned. It is never sent to a client.
type DecisionCursor struct{ Seq int64 }

// DecisionQuery is one AuditWallsRows read:
// action = ANY(Actions) AND recorded_at >= Since AND seq > After.Seq [AND run_id = ANY(RunIDs)] ORDER BY seq LIMIT Limit.
type DecisionQuery struct {
	Actions []string       // egress.hold also reads egress.pending
	Since   time.Time      // bounds recorded_at, the control plane's append time
	After   DecisionCursor // the zero cursor reads from the start
	RunIDs  []uuid.UUID    // empty: every run
	Limit   int            // 1..5000
}

// DecodeDecisionRow decodes one stored row; seq and recordedAt come from the row's columns.
func DecodeDecisionRow(ev types.AuditEvent, recordedAt time.Time, seq int64) (DecisionRow, error) {
	var d auditWallsData
	if len(ev.Data) > 0 {
		if err := json.Unmarshal(ev.Data, &d); err != nil {
			return DecisionRow{}, fmt.Errorf("store: decode egress row %s: %w", ev.ID, err)
		}
	}
	r := DecisionRow{
		ID: ev.ID, Seq: seq, RecordedAt: recordedAt, Time: ev.Time, Action: ev.Action, Actor: ev.Actor,
		Host: d.Host, Port: d.Port, RuleSource: d.RuleSource, ApprovalID: d.ApprovalID, By: d.By,
		Reason: d.RefusalReason, LayerName: d.LayerName, MatchedRule: d.MatchedRule, DecidedAt: d.ObservedAt,
		Cause: d.Cause, Flow: d.Flow, UpstreamFault: d.UpstreamFault, Via: d.Via, Sponsor: d.Sponsor,
		Workload: d.RunWorkload, Format: 1,
	}
	if ev.RunID != nil {
		r.RunID = *ev.RunID
	}
	if r.Action == auditWallsPending {
		r.Action = auditWallsHold
	}
	if r.DecidedAt.IsZero() {
		r.DecidedAt = ev.Time
	}
	if r.Workload == "" && ev.ActorType == types.ActorAgent {
		r.Workload = ev.Actor
	}
	r.Weight = auditWallsWeight(d)
	return r, nil
}

// auditWallsLateAfter is how long after it was decided a row may be recorded and still count as live.
const auditWallsLateAfter = 45 * time.Second

// The hold action and the older name some rows still carry for it.
const (
	auditWallsHold    = "egress.hold"
	auditWallsPending = "egress.pending"
)

// auditWallsDroppedPrefix is the rule_source of the proxy's summary of decisions it could not post; the
// count follows it (decisionSink.reportDropped).
const auditWallsDroppedPrefix = "egress:dropped-decisions-"

// auditWallsSpoolLost is the rule_source of a runner spool's loss row, whose Cause opens with the number
// of decisions it lost (runnerspool.Spool.EmitLoss).
const auditWallsSpoolLost = "builtin:spool-lost"

// auditWallsData is the data keys DecodeDecisionRow reads. Every other key is ignored. The contact and
// owner keys are not here: no writer sets them yet, so they decode to their zero values.
type auditWallsData struct {
	Host          string    `json:"host"`
	Port          int       `json:"port"`
	RuleSource    string    `json:"rule_source"`
	ApprovalID    string    `json:"approval_id"`
	By            string    `json:"by"`
	RefusalReason string    `json:"refusal_reason"`
	LayerName     string    `json:"layer_name"`
	MatchedRule   string    `json:"matched_rule"`
	Repeat        int64     `json:"repeat"`
	Count         int64     `json:"count"`
	ObservedAt    time.Time `json:"observed_at"`
	Cause         string    `json:"cause"`
	Flow          string    `json:"flow"`
	UpstreamFault string    `json:"upstream_fault"`
	Via           string    `json:"via"`
	Sponsor       string    `json:"sponsor"`
	RunWorkload   string    `json:"run_workload"`
}

// auditWallsWeight is how many decisions one row stands for, never below 1.
func auditWallsWeight(d auditWallsData) int64 {
	switch {
	case d.Repeat > 0:
		return d.Repeat
	case d.Count > 0:
		return d.Count
	}
	if n, ok := strings.CutPrefix(d.RuleSource, auditWallsDroppedPrefix); ok {
		if v, err := strconv.ParseInt(n, 10, 64); err == nil && v > 0 {
			return v
		}
	}
	if d.RuleSource == auditWallsSpoolLost {
		lead, _, _ := strings.Cut(d.Cause, " ")
		if v, err := strconv.ParseInt(lead, 10, 64); err == nil && v > 0 {
			return v
		}
	}
	return 1
}

// WallsAuditReader is the Walls view's read of egress decision rows, oldest first by seq. Optional like
// Pager; the api serves no Walls read from a store without it.
type WallsAuditReader interface {
	AuditWallsRows(ctx context.Context, q DecisionQuery) ([]DecisionRow, DecisionCursor, error)
}

var _ WallsAuditReader = PG{}

// WallsCount is one group of an AuditWallsCounts read. Action is "" on the group that totals every action.
type WallsCount struct {
	Action  string
	InScope bool  // the row's run has a unit among the given units; a NULL run_id or unit_id is out of scope
	Rows    int64 // rows, never weighted
	Runs    int64 // distinct runs
	Live    int64 // distinct runs whose state is not terminal
}

// WallsAuditCounter is the count-only read behind a unit admin's totals for the runs outside their units.
type WallsAuditCounter interface {
	// AuditWallsCounts counts the newest limit rows with action = ANY(actions) (egress.hold also counts
	// egress.pending) and recorded_at >= since, grouped by action and in scope. It returns no row.
	AuditWallsCounts(ctx context.Context, actions []string, since time.Time, units []uuid.UUID, limit int) ([]WallsCount, error)
}

var _ WallsAuditCounter = PG{}

// WallsStartReader reads the first allow rows of a set of runs, where a run's proxy records its upstream.
type WallsStartReader interface {
	// AuditWallsFirstAllows returns each given run's first perRun egress.allow rows (seq order) recorded at or
	// after that run's own since[i], decoded, in one statement. It reads no data key.
	AuditWallsFirstAllows(ctx context.Context, runIDs []uuid.UUID, since []time.Time, perRun int) ([]DecisionRow, error)
}

var _ WallsStartReader = PG{}

// The read bounds of the three accessors.
const (
	auditWallsMaxLimit     = 5000
	auditWallsMaxCountRows = 250000
	auditWallsMaxPerRun    = 64
	auditWallsMaxStartRuns = 5000
	auditWallsStartAction  = "egress.allow"
	auditWallsScanNoun     = "walls audit rows"
	auditWallsCountNoun    = "walls audit counts"
	auditWallsFirstNoun    = "walls first allow rows"
)

// auditWallsActions is actions with egress.pending added beside egress.hold, each once.
func auditWallsActions(actions []string) []string {
	out := make([]string, 0, len(actions)+1)
	for _, a := range actions {
		if !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	if slices.Contains(out, auditWallsHold) && !slices.Contains(out, auditWallsPending) {
		out = append(out, auditWallsPending)
	}
	return out
}

// auditWallsScanned is one scanned row with the two columns scanAuditEvent does not read.
type auditWallsScanned struct {
	ev         types.AuditEvent
	recordedAt time.Time
	seq        int64
}

// auditWallsRow is a pgx.Row whose Scan reads the store's audit columns, then recorded_at and seq.
type auditWallsRow struct {
	pgx.Row
	recordedAt *time.Time
	seq        *int64
}

func (r auditWallsRow) Scan(dest ...any) error {
	return r.Row.Scan(append(dest, r.recordedAt, r.seq)...)
}

// auditWallsScan scans auditCols, recorded_at and seq.
func auditWallsScan(row pgx.Row) (auditWallsScanned, error) {
	var out auditWallsScanned
	ev, err := scanAuditEvent(auditWallsRow{Row: row, recordedAt: &out.recordedAt, seq: &out.seq})
	out.ev = ev
	return out, err
}

// auditWallsDecode decodes scanned rows, skipping and logging a row that does not decode.
func auditWallsDecode(ctx context.Context, scanned []auditWallsScanned) []DecisionRow {
	out := make([]DecisionRow, 0, len(scanned))
	for _, sc := range scanned {
		row, err := DecodeDecisionRow(sc.ev, sc.recordedAt, sc.seq)
		if err != nil {
			slog.WarnContext(ctx, "wardyn: skipped an egress audit row the Walls view cannot decode",
				slog.String("id", sc.ev.ID.String()), slog.Any("err", err))
			continue
		}
		out = append(out, row)
	}
	return out
}

// AuditWallsRows reads one page of decision rows in seq order. The cursor it returns is the last row it
// scanned, decoded or not, so a page of rows that do not decode still moves it; with no row scanned it is
// q.After.
func (s PG) AuditWallsRows(ctx context.Context, q DecisionQuery) ([]DecisionRow, DecisionCursor, error) {
	if q.Limit < 1 || q.Limit > auditWallsMaxLimit {
		return nil, q.After, fmt.Errorf("store: walls audit read limit %d is outside 1..%d", q.Limit, auditWallsMaxLimit)
	}
	args := []any{auditWallsActions(q.Actions), q.Since, q.After.Seq}
	sql := `SELECT ` + auditCols + `, recorded_at, seq FROM audit_events
		WHERE action = ANY($1) AND recorded_at >= $2 AND seq > $3`
	if len(q.RunIDs) > 0 {
		args = append(args, q.RunIDs)
		sql += ` AND run_id = ANY($4)`
	}
	args = append(args, q.Limit)
	sql += fmt.Sprintf(` ORDER BY seq LIMIT $%d`, len(args))
	scanned, err := collect(ctx, s.Pool, "query", auditWallsScanNoun, sql, args, auditWallsScan)
	if err != nil {
		return nil, q.After, err
	}
	cursor := q.After
	if n := len(scanned); n > 0 {
		cursor = DecisionCursor{Seq: scanned[n-1].seq}
	}
	return auditWallsDecode(ctx, scanned), cursor, nil
}

// AuditWallsCounts — see WallsAuditCounter. The newest limit rows are taken first, in a subquery, so the
// grouping only ever sees them; a row with no run, or whose run has no unit, counts out of scope.
func (s PG) AuditWallsCounts(ctx context.Context, actions []string, since time.Time, units []uuid.UUID, limit int) ([]WallsCount, error) {
	if limit < 1 || limit > auditWallsMaxCountRows {
		return nil, fmt.Errorf("store: walls audit count limit %d is outside 1..%d", limit, auditWallsMaxCountRows)
	}
	if units == nil {
		units = []uuid.UUID{}
	}
	sql := `SELECT CASE WHEN GROUPING(t.act) = 1 THEN '' ELSE t.act END, t.in_scope,
			count(*), count(DISTINCT t.run_id), count(DISTINCT t.run_id) FILTER (WHERE t.live)
		FROM (
			SELECT CASE e.action WHEN 'egress.pending' THEN 'egress.hold' ELSE e.action END AS act, e.run_id,
				COALESCE(r.unit_id = ANY($3), false) AS in_scope,
				COALESCE(r.state = ANY($5), false) AS live
			FROM (
				SELECT action, run_id FROM audit_events
				WHERE action = ANY($1) AND recorded_at >= $2
				ORDER BY seq DESC LIMIT $4
			) e
			LEFT JOIN agent_runs r ON r.id = e.run_id
		) t
		GROUP BY GROUPING SETS ((t.act, t.in_scope), (t.in_scope))`
	args := []any{auditWallsActions(actions), since, units, limit, nonTerminalStateList()}
	return collect(ctx, s.Pool, "query", auditWallsCountNoun, sql, args, func(row pgx.Row) (WallsCount, error) {
		var c WallsCount
		err := row.Scan(&c.Action, &c.InScope, &c.Rows, &c.Runs, &c.Live)
		return c, err
	})
}

// AuditWallsFirstAllows — see WallsStartReader. Each run is probed on the (run_id, seq) index from its own
// since, so the probe reads only the partitions from that run's start.
func (s PG) AuditWallsFirstAllows(ctx context.Context, runIDs []uuid.UUID, since []time.Time, perRun int) ([]DecisionRow, error) {
	switch {
	case perRun < 1 || perRun > auditWallsMaxPerRun:
		return nil, fmt.Errorf("store: walls first allow rows per run %d is outside 1..%d", perRun, auditWallsMaxPerRun)
	case len(runIDs) > auditWallsMaxStartRuns:
		return nil, fmt.Errorf("store: walls first allow rows asked for %d runs, more than %d", len(runIDs), auditWallsMaxStartRuns)
	case len(runIDs) != len(since):
		return nil, errors.New("store: walls first allow rows need one since per run")
	case len(runIDs) == 0:
		return []DecisionRow{}, nil
	}
	sql := `SELECT ` + auditCols + `, recorded_at, seq
		FROM unnest($1::uuid[], $2::timestamptz[]) WITH ORDINALITY AS p(rid, since, ord)
		CROSS JOIN LATERAL (
			SELECT ` + auditCols + `, recorded_at, seq FROM audit_events
			WHERE run_id = p.rid AND action = $3 AND recorded_at >= p.since
			ORDER BY seq LIMIT $4
		) a
		ORDER BY p.ord, seq`
	args := []any{runIDs, since, auditWallsStartAction, perRun}
	scanned, err := collect(ctx, s.Pool, "query", auditWallsFirstNoun, sql, args, auditWallsScan)
	if err != nil {
		return nil, err
	}
	return auditWallsDecode(ctx, scanned), nil
}
