// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/runnerspool"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func spoolPGFixture(t *testing.T) (*pgxpool.Pool, store.PG, types.Runner, types.AgentRun, runnerspool.Record) {
	t.Helper()
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	rn, err := st.CreateRunner(ctx, newRunner("spool-owner"))
	if err != nil {
		t.Fatal(err)
	}
	rn, err = st.ClaimRunner(ctx, rn.ID, rn.Owner, rn.KeyFingerprint, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	run := newRun(types.RunRunning)
	run.CreatedBy = rn.Owner
	run.Placement = types.PlacementLocal
	run.RunnerID = &rn.ID
	run.EvidenceSource = types.RunEvidenceRunnerAsserted
	run, err = st.CreateRun(ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	spool, err := runnerspool.Open(filepath.Join(t.TempDir(), "spool"), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Close()
	if err := spool.Append(ctx, egress.DecisionLog{Request: egress.Request{RunID: run.ID, Host: "allowed.test", Time: time.Now().Add(-100 * 365 * 24 * time.Hour).UTC().Truncate(time.Microsecond)}, Decision: egress.Allow, RuleSource: "policy:allowed"}); err != nil {
		t.Fatal(err)
	}
	rows, err := spool.After(ctx, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	return pool, st, rn, run, rows[0]
}
func prepareSpoolTest(_ context.Context, ev types.AuditEvent) (types.AuditEvent, error) {
	return ev, nil
}
func spoolCounts(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID) (int, int) {
	t.Helper()
	var receipts, events int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM runner_spool_cursors WHERE run_id=$1`, runID).Scan(&receipts); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM audit_events WHERE run_id=$1 AND data->>'spooled'='true'`, runID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	return receipts, events
}
func TestPG_RunnerSpoolAtomicRollbackAndConcurrentDedup(t *testing.T) {
	pool, st, rn, run, row := spoolPGFixture(t)
	ctx := context.Background()
	failure := errors.New("seal unavailable")
	if _, err := st.IngestRunnerDecision(ctx, rn.ID, row, func(context.Context, types.AuditEvent) (types.AuditEvent, error) { return types.AuditEvent{}, failure }); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if c, a := spoolCounts(t, pool, run.ID); c != 0 || a != 0 {
		t.Fatalf("failed transaction advanced %d cursor/%d audit", c, a)
	}
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := st.IngestRunnerDecision(ctx, rn.ID, row, prepareSpoolTest)
			if err != nil {
				t.Error(err)
			}
			if ok {
				accepted.Add(1)
			}
		}()
	}
	wg.Wait()
	if accepted.Load() != 1 {
		t.Fatalf("duplicate acceptance %d", accepted.Load())
	}
	if c, a := spoolCounts(t, pool, run.ID); c != 1 || a != 1 {
		t.Fatalf("unexpected %d cursor/%d audit", c, a)
	}
}
func TestPG_RunnerSpoolKilledHistoryHasNoLifecycleEffects(t *testing.T) {
	pool, st, rn, run, row := spoolPGFixture(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET state='KILLED',token_renewed_at=clock_timestamp()-interval '2 hours' WHERE id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	before, err := st.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := st.IngestRunnerDecision(ctx, rn.ID, row, prepareSpoolTest); err != nil || !ok {
		t.Fatalf("historical killed replay: %v %v", ok, err)
	}
	after, err := st.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(before)
	b, _ := json.Marshal(after)
	if string(a) != string(b) {
		t.Fatalf("audit changed killed run:\nbefore%s\nafter%s", a, b)
	}
	var at time.Time
	var data []byte
	if err := pool.QueryRow(ctx, `SELECT time,data FROM audit_events WHERE run_id=$1 AND data->>'spooled'='true'`, run.ID).Scan(&at, &data); err != nil {
		t.Fatal(err)
	}
	if at.Before(time.Now().Add(-time.Minute)) || at.After(time.Now().Add(time.Minute)) {
		t.Fatalf("runner backdated arrival clock: %v", at)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["observed_at"] != row.Decision.Request.Time.Format(time.RFC3339Nano) || fields["evidence_source"] != "runner_asserted" {
		t.Fatalf("provenance lost: %s", data)
	}
	if _, err := pool.Exec(ctx, `UPDATE runners SET state='revoked',revoked_at=clock_timestamp() WHERE id=$1`, rn.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.IngestRunnerDecision(ctx, rn.ID, row, prepareSpoolTest); !errors.Is(err, store.ErrRunnerSpoolRefused) {
		t.Fatalf("revoked duplicate acknowledged: %v", err)
	}
}
func TestPG_RunnerSpoolOwnershipIdentityAndErasure(t *testing.T) {
	pool, st, rn, run, row := spoolPGFixture(t)
	ctx := context.Background()
	other, err := st.CreateRunner(ctx, newRunner(rn.Owner))
	if err != nil {
		t.Fatal(err)
	}
	other, err = st.ClaimRunner(ctx, other.ID, other.Owner, other.KeyFingerprint, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.IngestRunnerDecision(ctx, other.ID, row, prepareSpoolTest); !errors.Is(err, store.ErrRunnerSpoolRefused) {
		t.Fatalf("foreign runner accepted: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE runners SET owner='other' WHERE id=$1`, rn.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.IngestRunnerDecision(ctx, rn.ID, row, prepareSpoolTest); !errors.Is(err, store.ErrRunnerSpoolRefused) {
		t.Fatalf("foreign owner accepted: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE runners SET owner=$2 WHERE id=$1`, rn.ID, run.CreatedBy); err != nil {
		t.Fatal(err)
	}
	if _, err := st.IngestRunnerDecision(ctx, rn.ID, row, prepareSpoolTest); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM runners WHERE id=$1`, rn.ID); err != nil {
		t.Fatal(err)
	}
	if c, a := spoolCounts(t, pool, run.ID); c != 0 || a != 1 {
		t.Fatalf("runner erasure removed history or kept cursor: %d %d", c, a)
	}
	got, err := st.GetRun(ctx, run.ID)
	if err != nil || got.RunnerID != nil {
		t.Fatalf("run should survive runner erasure: %+v %v", got, err)
	}
}
func TestPG_RunnerSpoolReceiptSurvivesMissingAuditBody(t *testing.T) {
	pool, st, rn, run, row := spoolPGFixture(t)
	ctx := context.Background()
	if _, err := st.IngestRunnerDecision(ctx, rn.ID, row, prepareSpoolTest); err != nil {
		t.Fatal(err)
	}
	// Isolated database only: remove the physical audit body to model retention
	// independently of the existing attested partition-drop tests. The receipt
	// must not rely on finding that body. Restore trigger mode before any replay.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `SET session_replication_role=replica`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `DELETE FROM audit_events WHERE run_id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `SET session_replication_role=origin`); err != nil {
		t.Fatal(err)
	}
	conn.Release()
	if ok, err := st.IngestRunnerDecision(ctx, rn.ID, row, prepareSpoolTest); err != nil || ok {
		t.Fatalf("retention replay duplicated audit: %v %v", ok, err)
	}
	if c, a := spoolCounts(t, pool, run.ID); c != 1 || a != 0 {
		t.Fatalf("receipt/body counts %d %d", c, a)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM agent_runs WHERE id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	if c, _ := spoolCounts(t, pool, run.ID); c != 0 {
		t.Fatal("run erasure retained cursor")
	}
}

func rehashSpoolRow(t *testing.T, row runnerspool.Record) runnerspool.Record {
	t.Helper()
	row.Hash = ""
	b, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(b)
	row.Hash = hex.EncodeToString(hash[:])
	return row
}
func TestPG_RunnerSpoolIdentityCursorAndOverflowCollisions(t *testing.T) {
	pool, st, rn, run, row := spoolPGFixture(t)
	ctx := context.Background()
	if _, err := st.IngestRunnerDecision(ctx, rn.ID, row, prepareSpoolTest); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*runnerspool.Record){
		func(r *runnerspool.Record) { r.Decision.Request.Host = "substituted.test" },
		func(r *runnerspool.Record) { r.SpoolID = uuid.New() },
		func(r *runnerspool.Record) { r.Seq = 3 },
	} {
		changed := row
		mutate(&changed)
		changed = rehashSpoolRow(t, changed)
		if _, err := st.IngestRunnerDecision(ctx, rn.ID, changed, prepareSpoolTest); !errors.Is(err, store.ErrRunnerSpoolRefused) {
			t.Fatalf("collision accepted: %+v %v", changed, err)
		}
	}
	overflow := row
	overflow.Seq = 0
	overflow.Count = 3
	overflow.DropEnd = 3
	overflow.Decision.Decision = egress.Deny
	overflow.Decision.RuleSource = "builtin:spool-full"
	overflow = rehashSpoolRow(t, overflow)
	for i := 0; i < 2; i++ {
		ok, err := st.IngestRunnerDecision(ctx, rn.ID, overflow, prepareSpoolTest)
		if err != nil || ok != (i == 0) {
			t.Fatalf("overflow dedup: %v %v", ok, err)
		}
	}
	changed := overflow
	changed.Count = 2
	changed = rehashSpoolRow(t, changed)
	if _, err := st.IngestRunnerDecision(ctx, rn.ID, changed, prepareSpoolTest); !errors.Is(err, store.ErrRunnerSpoolRefused) {
		t.Fatalf("overflow collision accepted: %v", err)
	}
	row.Seq = 2
	row = rehashSpoolRow(t, row)
	if ok, err := st.IngestRunnerDecision(ctx, rn.ID, row, prepareSpoolTest); err != nil || !ok {
		t.Fatalf("sequential record refused: %v %v", ok, err)
	}
	if c, a := spoolCounts(t, pool, run.ID); c != 1 || a != 3 {
		t.Fatalf("failed records changed receipt/history: %d %d", c, a)
	}
}
func TestPG_RunnerSpoolScanPairCommitsAtomically(t *testing.T) {
	pool, st, rn, run, row := spoolPGFixture(t)
	ctx := context.Background()
	row.Decision.Scan = &egress.ScanSummary{Action: "alert", Channel: "model", FindingsTotal: 101}
	row = rehashSpoolRow(t, row)
	calls := 0
	if _, err := st.IngestRunnerDecision(ctx, rn.ID, row, func(_ context.Context, ev types.AuditEvent) (types.AuditEvent, error) {
		calls++
		if calls == 2 {
			return ev, errors.New("second row sealing failed")
		}
		return ev, nil
	}); err == nil {
		t.Fatal("partial scan pair accepted")
	}
	if c, a := spoolCounts(t, pool, run.ID); c != 0 || a != 0 {
		t.Fatalf("partial scan committed: %d %d", c, a)
	}
	if _, err := st.IngestRunnerDecision(ctx, rn.ID, row, prepareSpoolTest); err != nil {
		t.Fatal(err)
	}
	if c, a := spoolCounts(t, pool, run.ID); c != 1 || a != 2 {
		t.Fatalf("scan pair missing: %d %d", c, a)
	}
	row.Seq = 2
	row.Decision.Scan.Action = "bypass"
	row = rehashSpoolRow(t, row)
	if _, err := st.IngestRunnerDecision(ctx, rn.ID, row, prepareSpoolTest); err != nil {
		t.Fatal(err)
	}
	var bypass, allows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FILTER(WHERE action='llm.scan.bypass'),count(*) FILTER(WHERE action='egress.allow') FROM audit_events WHERE run_id=$1`, run.ID).Scan(&bypass, &allows); err != nil {
		t.Fatal(err)
	}
	if bypass != 1 || allows != 1 {
		t.Fatalf("bypass fabricated egress: %d %d", bypass, allows)
	}
}

// A spooled deny carries its refusal onto the audit row exactly as the live door writes it.
func TestPG_RunnerSpoolDenyCarriesTheRefusal(t *testing.T) {
	pool, st, rn, run, row := spoolPGFixture(t)
	ctx := context.Background()
	row.Decision.Decision = egress.Deny
	row.Decision.RuleSource = "builtin:dial-failed"
	row.Decision.Refusal = &egress.Refusal{By: egress.RefusedByOutside, Reason: "connection_reset"}
	row = rehashSpoolRow(t, row)
	if _, err := st.IngestRunnerDecision(ctx, rn.ID, row, prepareSpoolTest); err != nil {
		t.Fatal(err)
	}
	var by, reason string
	if err := pool.QueryRow(ctx, `SELECT data->>'by', data->>'refusal_reason' FROM audit_events WHERE run_id=$1 AND action='egress.deny'`, run.ID).Scan(&by, &reason); err != nil {
		t.Fatal(err)
	}
	if by != "outside" || reason != "connection_reset" {
		t.Fatalf("spooled deny data by=%q refusal_reason=%q", by, reason)
	}
}
