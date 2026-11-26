// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"encoding/json"
	"testing"
)

// TestAudit_SpooledRowStampedFromStoredRun: a row a runner spooled is stamped from the stored run in the
// ingest transaction, not from anything the runner sent. A person's run names its owner and workload and no
// sponsor; a service identity's run also names the identity's sponsor as it stands.
func TestAudit_SpooledRowStampedFromStoredRun(t *testing.T) {
	for name, tc := range map[string]struct {
		createdBy, sponsor string
	}{
		"person-owned":  {"", ""},
		"service-owned": {"service:ci-bot", "boss@example.com"},
	} {
		t.Run(name, func(t *testing.T) {
			pool, st, rn, run, row := spoolPGFixture(t)
			ctx := context.Background()
			owner := run.CreatedBy
			if tc.createdBy != "" {
				owner = tc.createdBy
				if _, err := pool.Exec(ctx, `INSERT INTO service_identities (name, sponsor, created_by) VALUES ('ci-bot', $1, 'admin')`, tc.sponsor); err != nil {
					t.Fatal(err)
				}
				if _, err := pool.Exec(ctx, `UPDATE agent_runs SET created_by = $2 WHERE id = $1`, run.ID, owner); err != nil {
					t.Fatal(err)
				}
			}
			const workload = "spiffe://wardyn.test/agent-run/stored"
			if _, err := pool.Exec(ctx, `UPDATE agent_runs SET spiffe_id = $2 WHERE id = $1`, run.ID, workload); err != nil {
				t.Fatal(err)
			}
			if ok, err := st.IngestRunnerDecision(ctx, rn.ID, row, prepareSpoolTest); err != nil || !ok {
				t.Fatalf("ingest: %v %v", ok, err)
			}
			var raw []byte
			if err := pool.QueryRow(ctx, `SELECT data FROM audit_events WHERE run_id = $1 AND data->>'spooled' = 'true' LIMIT 1`, run.ID).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var data map[string]any
			if err := json.Unmarshal(raw, &data); err != nil {
				t.Fatal(err)
			}
			if data["run_owner"] != owner || data["run_workload"] != workload {
				t.Errorf("run_owner %v run_workload %v, want %q and %q (data %s)", data["run_owner"], data["run_workload"], owner, workload, raw)
			}
			if got, _ := data["sponsor"].(string); got != tc.sponsor {
				t.Errorf("sponsor = %q, want %q (data %s)", got, tc.sponsor, raw)
			}
		})
	}
}
