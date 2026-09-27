// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package groundtruth

import (
	"encoding/json"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// HeartbeatEventWithDropped builds the periodic sensor liveness beat. run_id
// is NULL (host-scoped); /healthz keys ebpf_groundtruth state off the most
// recent one within a TTL.
//
// droppedTotal merges two loss causes (full POST buffer; oversized/
// unterminated export line) since both answer the same operator question; a
// log line distinguishes them. observedTotal is the real-kernel-event count
// mapped — together they show "alive but idle" vs "events flowing".
// observedByKind splits it per event kind (nil on older builds), since the
// aggregate can't tell all kinds arriving from one masking others gone blind.
// droppedUnmapped counts events refused for correlating to no run, so "saw
// nothing" isn't conflated with "saw plenty, correlated none".
func HeartbeatEventWithDropped(droppedTotal, observedTotal, droppedUnmapped uint64, observedByKind map[string]uint64) types.AuditEvent {
	data := heartbeatData{
		EventData: EventData{
			Stream:      Stream,
			Subtype:     "heartbeat",
			Correlation: CorrelationUnmapped, // host-scoped, not bound to a run
		},
		DroppedTotal:    droppedTotal,
		ObservedTotal:   observedTotal,
		DroppedUnmapped: droppedUnmapped,
		ObservedByKind:  observedByKind,
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

// heartbeatData extends EventData with the sensor's cumulative drop and observed
// counts. It is JSONB on audit_events.data; /healthz reads dropped_total and
// observed_total off it.
type heartbeatData struct {
	EventData
	DroppedTotal uint64 `json:"dropped_total"`
	// ObservedTotal is the real-kernel-event count mapped (NOT heartbeats/blinds).
	ObservedTotal uint64 `json:"observed_total"`
	// DroppedUnmapped counts events dropped for correlating to no run (omitted by an older sensor build).
	DroppedUnmapped uint64 `json:"dropped_unmapped,omitempty"`
	// ObservedByKind splits ObservedTotal by event Action.
	ObservedByKind map[string]uint64 `json:"observed_by_kind,omitempty"`
}

// BlindEvent builds the one-time event for a run the host eBPF sensor can't
// see into (CC3/Kata), bound to the run so the gap is visible, not silent.
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
