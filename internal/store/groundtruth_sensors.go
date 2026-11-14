// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"time"
)

// GroundtruthSensor is one ground-truth sensor's latest heartbeat (groundtruth_sensors): the node it
// runs on ("" for the Compose sensor), when the server received the beat, and the counts it carried.
type GroundtruthSensor struct {
	Node            string
	LastBeat        time.Time
	ObservedTotal   uint64
	DroppedTotal    uint64
	DroppedUnmapped uint64
	ObservedByKind  map[string]uint64 // nil when the sensor sends no breakdown
	// EvidenceAt is when a beat last carried kernel event counts that rose, nil until one has (see
	// UpsertGroundtruthSensor). It is written by the upsert, never by the caller.
	EvidenceAt *time.Time
}

// GroundtruthEvidenceWindow is how far apart two beats may be for a rise in their counts to set
// EvidenceAt, and how old EvidenceAt may be before /healthz reads the sensor as quiet.
const GroundtruthEvidenceWindow = 15 * time.Minute

// GroundtruthSensorStore keeps each node sensor's latest heartbeat, so the health read can judge
// every node rather than only the newest beat.
type GroundtruthSensorStore interface {
	// UpsertGroundtruthSensor writes g as its node's row. A beat older than the stored one leaves the
	// row as it is, so two replicas taking beats out of order never move a node back in time.
	// EvidenceAt moves in the same statement, from the stored row: to g's time when observed_total
	// plus dropped_unmapped rose and the stored beat is at most GroundtruthEvidenceWindow older; to
	// nil when they rose across a longer gap; unchanged when they did not rise. A first row has none.
	// dropped_total is left out: a lost heartbeat batch raises it with no kernel event behind it.
	UpsertGroundtruthSensor(ctx context.Context, g GroundtruthSensor) error
	// ListGroundtruthSensors returns the nodes heard from after since, newest beat first, at most
	// maxGroundtruthSensors of them.
	ListGroundtruthSensors(ctx context.Context, since time.Time) ([]GroundtruthSensor, error)
}

var _ GroundtruthSensorStore = PG{}

// maxGroundtruthSensors bounds the rows one health read returns: /healthz is anonymous and reads
// them on every probe, and node names come from the sensor.
const maxGroundtruthSensors = 1000

// UpsertGroundtruthSensor implements GroundtruthSensorStore.
func (s PG) UpsertGroundtruthSensor(ctx context.Context, g GroundtruthSensor) error {
	var kinds []byte
	if g.ObservedByKind != nil {
		b, err := json.Marshal(g.ObservedByKind)
		if err != nil {
			return fmt.Errorf("store: groundtruth sensor kinds: %w", err)
		}
		kinds = b
	}
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO groundtruth_sensors (node, last_beat, observed_total, dropped_total, dropped_unmapped, observed_by_kind)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (node) DO UPDATE SET
			last_beat = EXCLUDED.last_beat,
			observed_total = EXCLUDED.observed_total,
			dropped_total = EXCLUDED.dropped_total,
			dropped_unmapped = EXCLUDED.dropped_unmapped,
			observed_by_kind = EXCLUDED.observed_by_kind,
			evidence_at = CASE
				WHEN EXCLUDED.observed_total::numeric + EXCLUDED.dropped_unmapped
					<= groundtruth_sensors.observed_total::numeric + groundtruth_sensors.dropped_unmapped
					THEN groundtruth_sensors.evidence_at
				WHEN EXCLUDED.last_beat - groundtruth_sensors.last_beat <= $7::interval THEN EXCLUDED.last_beat
				ELSE NULL END
		WHERE groundtruth_sensors.last_beat <= EXCLUDED.last_beat`,
		g.Node, g.LastBeat, clampInt64(g.ObservedTotal), clampInt64(g.DroppedTotal), clampInt64(g.DroppedUnmapped), kinds,
		GroundtruthEvidenceWindow.String())
	if err != nil {
		return fmt.Errorf("store: upsert groundtruth sensor: %w", err)
	}
	return nil
}

// ListGroundtruthSensors implements GroundtruthSensorStore.
func (s PG) ListGroundtruthSensors(ctx context.Context, since time.Time) ([]GroundtruthSensor, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT node, last_beat, observed_total, dropped_total, dropped_unmapped, observed_by_kind, evidence_at
		FROM groundtruth_sensors WHERE last_beat > $1
		ORDER BY last_beat DESC, node LIMIT $2`, since, maxGroundtruthSensors)
	if err != nil {
		return nil, fmt.Errorf("store: list groundtruth sensors: %w", err)
	}
	defer rows.Close()
	var out []GroundtruthSensor
	for rows.Next() {
		var g GroundtruthSensor
		var observed, dropped, unmapped int64
		var kinds []byte
		if err := rows.Scan(&g.Node, &g.LastBeat, &observed, &dropped, &unmapped, &kinds, &g.EvidenceAt); err != nil {
			return nil, fmt.Errorf("store: scan groundtruth sensor: %w", err)
		}
		g.ObservedTotal, g.DroppedTotal, g.DroppedUnmapped = uint64(max(observed, 0)), uint64(max(dropped, 0)), uint64(max(unmapped, 0))
		if len(kinds) > 0 {
			if err := json.Unmarshal(kinds, &g.ObservedByKind); err != nil {
				return nil, fmt.Errorf("store: decode groundtruth sensor kinds: %w", err)
			}
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list groundtruth sensors: %w", err)
	}
	return out, nil
}

// clampInt64 stores a sensor's unsigned count in a BIGINT column.
func clampInt64(v uint64) int64 {
	if v > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(v)
}
