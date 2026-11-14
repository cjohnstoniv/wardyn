// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Per-node ground-truth sensor rows: one row per node, the newest beat wins, and a read returns only
// the nodes heard from since a time.
//
// Guarded by WARDYN_TEST_PG; skipped cleanly without one.
package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

func TestPG_GroundtruthSensorsUpsertAndList(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)

	// Node names nobody else writes: the table is shared by every test in the database.
	a, b := "node-a-"+uuid.NewString(), "node-b-"+uuid.NewString()
	now := time.Now().UTC().Truncate(time.Microsecond)
	kinds := map[string]uint64{"kernel.process.exec": 5, "kernel.network.connect": 2}

	for _, g := range []store.GroundtruthSensor{
		{Node: a, LastBeat: now.Add(-time.Minute), ObservedTotal: 1},
		{Node: a, LastBeat: now, DroppedTotal: 3, ObservedTotal: 7, DroppedUnmapped: 2, ObservedByKind: kinds},
		// A beat older than the stored one (a replica that lost the race) does not move the row back.
		{Node: a, LastBeat: now.Add(-2 * time.Minute), ObservedTotal: 99},
		{Node: b, LastBeat: now.Add(-48 * time.Hour), ObservedTotal: 4},
	} {
		if err := pg.UpsertGroundtruthSensor(ctx, g); err != nil {
			t.Fatalf("upsert %s at %s: %v", g.Node, g.LastBeat, err)
		}
	}

	mine := func(since time.Time) map[string]store.GroundtruthSensor {
		t.Helper()
		rows, err := pg.ListGroundtruthSensors(ctx, since)
		if err != nil {
			t.Fatalf("list since %s: %v", since, err)
		}
		out := map[string]store.GroundtruthSensor{}
		for _, r := range rows {
			if r.Node == a || r.Node == b {
				out[r.Node] = r
			}
		}
		return out
	}

	got := mine(now.Add(-24 * time.Hour))
	if _, has := got[b]; has || len(got) != 1 {
		t.Fatalf("since 24h ago = %+v, want node a only (b last beat 48h ago)", got)
	}
	ra := got[a]
	if !ra.LastBeat.Equal(now) || ra.DroppedTotal != 3 || ra.ObservedTotal != 7 || ra.DroppedUnmapped != 2 ||
		ra.ObservedByKind["kernel.process.exec"] != 5 || ra.ObservedByKind["kernel.network.connect"] != 2 {
		t.Errorf("node a = %+v, want the newest beat (%s, 3/7/2, exec 5, connect 2)", ra, now)
	}

	got = mine(now.Add(-72 * time.Hour))
	if rb, has := got[b]; !has || rb.ObservedTotal != 4 || rb.ObservedByKind != nil {
		t.Errorf("since 72h ago, node b = %+v (present %v), want observed 4 and no breakdown", rb, has)
	}
}

// The Compose sensor carries no node; its row is the empty node.
func TestPG_GroundtruthSensorsEmptyNode(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := pg.UpsertGroundtruthSensor(ctx, store.GroundtruthSensor{Node: "", LastBeat: now, ObservedTotal: 1}); err != nil {
		t.Fatalf("upsert empty node: %v", err)
	}
	rows, err := pg.ListGroundtruthSensors(ctx, now.Add(-time.Minute))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 || rows[0].Node != "" || rows[0].ObservedTotal != 1 {
		t.Fatalf("rows = %+v, want the one empty-node row", rows)
	}
}

// The upsert keeps when a node's kernel event counts (observed_total plus dropped_unmapped) last rose,
// in the same statement as the beat. /healthz on every replica reads it from the row, so it must move
// only on a rise between two beats at most GroundtruthEvidenceWindow apart, and never on dropped_total,
// which a lost heartbeat batch raises with no kernel event behind it.
func TestPG_GroundtruthSensorsEvidenceAt(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()
	pg := store.NewPG(pool)
	node := "node-evidence-" + uuid.NewString()
	t0 := time.Now().UTC().Truncate(time.Microsecond)
	at := func(d time.Duration) *time.Time { v := t0.Add(d); return &v }

	for _, step := range []struct {
		name                        string
		beat                        time.Duration
		observed, dropped, unmapped uint64
		want                        *time.Time
	}{
		{"first insert", 0, 5, 0, 1, nil},
		{"equal counts", 30 * time.Second, 5, 0, 1, nil},
		{"a rise within the window", time.Minute, 6, 0, 1, at(time.Minute)},
		{"equal counts keep it", 90 * time.Second, 6, 0, 1, at(time.Minute)},
		{"lower counts (an ingest restart) keep it", 2 * time.Minute, 0, 0, 0, at(time.Minute)},
		{"only dropped_total rose: no move", 150 * time.Second, 0, 9, 0, at(time.Minute)},
		{"only dropped_unmapped rose: move", 3 * time.Minute, 0, 9, 3, at(3 * time.Minute)},
		{"a rise exactly the window apart", 3*time.Minute + store.GroundtruthEvidenceWindow, 1, 9, 3,
			at(3*time.Minute + store.GroundtruthEvidenceWindow)},
		{"a rise across a longer gap", 3*time.Minute + 2*store.GroundtruthEvidenceWindow + time.Second, 2, 9, 3, nil},
		{"the next rise sets it again", 3*time.Minute + 2*store.GroundtruthEvidenceWindow + 31*time.Second, 3, 9, 3,
			at(3*time.Minute + 2*store.GroundtruthEvidenceWindow + 31*time.Second)},
	} {
		if err := pg.UpsertGroundtruthSensor(ctx, store.GroundtruthSensor{Node: node, LastBeat: t0.Add(step.beat),
			ObservedTotal: step.observed, DroppedTotal: step.dropped, DroppedUnmapped: step.unmapped}); err != nil {
			t.Fatalf("%s: upsert: %v", step.name, err)
		}
		got := groundtruthSensorRow(t, pg, node, t0)
		if (got.EvidenceAt == nil) != (step.want == nil) || (got.EvidenceAt != nil && !got.EvidenceAt.Equal(*step.want)) {
			t.Fatalf("%s: evidence_at = %v, want %v", step.name, got.EvidenceAt, step.want)
		}
	}

	// A beat older than the stored one changes nothing, however much its counts rose.
	before := groundtruthSensorRow(t, pg, node, t0)
	if err := pg.UpsertGroundtruthSensor(ctx, store.GroundtruthSensor{Node: node, LastBeat: t0.Add(time.Minute), ObservedTotal: 500}); err != nil {
		t.Fatalf("older beat: upsert: %v", err)
	}
	if got := groundtruthSensorRow(t, pg, node, t0); !got.LastBeat.Equal(before.LastBeat) || got.ObservedTotal != before.ObservedTotal ||
		got.EvidenceAt == nil || !got.EvidenceAt.Equal(*before.EvidenceAt) {
		t.Fatalf("an older beat moved the row: %+v, want %+v", got, before)
	}
}

// groundtruthSensorRow is node's row, heard from after since, or fails.
func groundtruthSensorRow(t *testing.T, pg store.PG, node string, since time.Time) store.GroundtruthSensor {
	t.Helper()
	rows, err := pg.ListGroundtruthSensors(context.Background(), since.Add(-time.Hour))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, r := range rows {
		if r.Node == node {
			return r
		}
	}
	t.Fatalf("no row for %s", node)
	return store.GroundtruthSensor{}
}
