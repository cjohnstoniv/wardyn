// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// ingestTunnelRow replays the fixture's spooled allow with the given mark and scan summary, and returns the
// run's opaque-tunnel count. Each case runs on a fresh fixture, so a count is one row's doing.
func ingestTunnelRow(t *testing.T, marked bool, scan *egress.ScanSummary) (int64, string) {
	t.Helper()
	pool, st, rn, run, row := spoolPGFixture(t)
	ctx := context.Background()
	row.Decision.ModelTunnel = marked
	row.Decision.Scan = scan
	row = rehashSpoolRow(t, row)
	if _, err := st.IngestRunnerDecision(ctx, rn.ID, row, prepareSpoolTest); err != nil {
		t.Fatal(err)
	}
	n, err := st.RunOpaqueTunnels(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var mark string
	if err := pool.QueryRow(ctx, `SELECT coalesce(data->>'model_tunnel','') FROM audit_events WHERE run_id=$1 AND action='egress.allow'`, run.ID).Scan(&mark); err != nil {
		// A replayed bypass-only row writes no egress.allow.
		mark = "-"
	}
	return n, mark
}

// TestRunOpaqueTunnels_CountsMarkedRowsAndOldBypassRows: a marked egress.allow counts as one tunnel with the
// scanner off, an old runner's bypass row still counts, and the two rows one tunnel writes when the scanner is
// on are one tunnel and not two. An unmarked allow is not a tunnel.
func TestRunOpaqueTunnels_CountsMarkedRowsAndOldBypassRows(t *testing.T) {
	bypass := &egress.ScanSummary{Action: "bypass", Coverage: "tunneled-opaque"}
	for _, tc := range []struct {
		name   string
		marked bool
		scan   *egress.ScanSummary
		want   int64
	}{
		{"marked allow, scanner off", true, nil, 1},
		{"old runner bypass row", false, bypass, 1},
		{"marked allow and its bypass row", true, bypass, 1},
		{"unmarked allow", false, nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, _ := ingestTunnelRow(t, tc.marked, tc.scan); got != tc.want {
				t.Errorf("RunOpaqueTunnels = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestRunnerDecisionEvents_CarriesModelTunnel: a runner's replayed allow keeps the mark in the row's data, and
// an unmarked one has none.
func TestRunnerDecisionEvents_CarriesModelTunnel(t *testing.T) {
	if _, mark := ingestTunnelRow(t, true, nil); mark != "true" {
		t.Errorf("marked replay data.model_tunnel = %q, want \"true\"", mark)
	}
	if _, mark := ingestTunnelRow(t, false, nil); mark != "" {
		t.Errorf("unmarked replay data.model_tunnel = %q, want absent", mark)
	}
}
