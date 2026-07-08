// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package groundtruth

import (
	"encoding/json"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// HeartbeatEvent builds the periodic sensor liveness beat with no drop count.
// Equivalent to HeartbeatEventWithDropped(0).
func HeartbeatEvent() types.AuditEvent {
	return HeartbeatEventWithDropped(0)
}

// HeartbeatEventWithDropped builds the periodic sensor liveness beat. run_id is
// NULL (the sensor is host-scoped, not per-run); actor_type=system,
// actor=sensor. The control plane records these append-only and /healthz keys
// the ebpf_groundtruth state off the most recent one within a TTL — so the
// stream reports "healthy" only while beats are actually arriving. droppedTotal
// is the sensor's cumulative backpressure-drop count, surfaced on /healthz so
// operators see the gap size.
func HeartbeatEventWithDropped(droppedTotal uint64) types.AuditEvent {
	data := heartbeatData{
		EventData: EventData{
			Stream:      Stream,
			Subtype:     "heartbeat",
			Correlation: CorrelationUnmapped, // host-scoped, not bound to a run
		},
		DroppedTotal: droppedTotal,
	}
	raw, err := json.Marshal(data)
	if err != nil {
		raw = data.EventData.marshal()
	}
	return types.AuditEvent{
		RunID:     nil,
		ActorType: types.ActorSystem,
		Actor:     SensorActor,
		Action:    ActionSensorHeartbeat,
		Outcome:   "success",
		Data:      raw,
	}
}

// heartbeatData extends EventData with the sensor's cumulative drop count. It
// is JSONB on audit_events.data; /healthz reads dropped_total off it.
type heartbeatData struct {
	EventData
	DroppedTotal uint64 `json:"dropped_total"`
}

// BlindEvent builds the one-time event emitted for a run the host eBPF sensor
// cannot see into (CC3/Kata microVM guest). It is bound to the run (so the gap
// is attributable) with a fixed reason. This makes the published host-eBPF-vs-
// Kata blindness gap VISIBLE in the audit stream rather than a silent absence.
func BlindEvent(runID uuid.UUID, reason string) types.AuditEvent {
	if reason == "" {
		reason = "cc3-kata-host-ebpf-blind"
	}
	rid := runID
	data := EventData{
		Stream:      Stream,
		Subtype:     "blind",
		Correlation: CorrelationMapped, // we know which run we are blind to
		Reason:      reason,
	}
	return types.AuditEvent{
		RunID:     &rid,
		ActorType: types.ActorSystem,
		Actor:     SensorActor,
		Action:    ActionSensorBlind,
		Outcome:   "failure", // a coverage gap is an unexpected/degraded state
		Data:      data.marshal(),
	}
}
