// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/runnerspool"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPG_RunnerSpoolDNSQueryIsItsOwnRow: a spooled DNS query replays as one
// run.dns.query row with its name, type and outcome, and no egress row.
func TestPG_RunnerSpoolDNSQueryIsItsOwnRow(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	st := store.NewPG(pool)
	ctx := context.Background()
	rn, err := st.CreateRunner(ctx, newRunner("spool-dns"))
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
	if run, err = st.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	spool, err := runnerspool.Open(filepath.Join(t.TempDir(), "spool"), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Close()
	q := egress.DNSQuery{Name: "exfil.example.com", Type: "A", Outcome: "refused", Via: "proxy"}
	if err := spool.Append(ctx, egress.DecisionLog{Request: egress.Request{RunID: run.ID, Host: q.Name, Time: time.Now().UTC().Truncate(time.Microsecond)},
		Decision: egress.Deny, RuleSource: "dns:refused", DNS: &q}); err != nil {
		t.Fatal(err)
	}
	rows, err := spool.After(ctx, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := st.IngestRunnerDecision(ctx, rn.ID, rows[0], prepareSpoolTest); err != nil || !ok {
		t.Fatalf("ingest: %v %v", ok, err)
	}
	if _, events := spoolCounts(t, pool, run.ID); events != 1 {
		t.Fatalf("%d spooled rows, want the one DNS row", events)
	}
	var action, target, outcome, name, qtype, out string
	if err := pool.QueryRow(ctx, `SELECT action, target, outcome, data->>'name', data->>'type', data->>'outcome' FROM audit_events WHERE run_id=$1 AND data->>'spooled'='true'`, run.ID).
		Scan(&action, &target, &outcome, &name, &qtype, &out); err != nil {
		t.Fatal(err)
	}
	if action != "run.dns.query" || target != q.Name || outcome != "denied" || name != q.Name || qtype != "A" || out != "refused" {
		t.Fatalf("row %s %s %s %s %s %s", action, target, outcome, name, qtype, out)
	}
}
