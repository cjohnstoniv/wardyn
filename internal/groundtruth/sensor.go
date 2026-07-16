// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package groundtruth

import (
	"encoding/json"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// HeartbeatEventWithDropped builds the periodic sensor liveness beat. run_id is
// NULL (the sensor is host-scoped, not per-run); actor_type=system,
// actor=sensor. The control plane records these append-only and /healthz keys
// the ebpf_groundtruth state off the most recent one within a TTL. droppedTotal
// is the sensor's cumulative backpressure-drop count and observedTotal the
// cumulative count of real kernel events mapped off the tail; both are surfaced
// on /healthz so it can tell "sensor alive but observing nothing" (idle) apart
// from "events flowing" (healthy) and show the drop-gap size. A live heartbeat
// ALONE never proves kernel ground truth is arriving.
func HeartbeatEventWithDropped(droppedTotal, observedTotal uint64) types.AuditEvent {
	data := heartbeatData{
		EventData: EventData{
			Stream:      Stream,
			Subtype:     "heartbeat",
			Correlation: CorrelationUnmapped, // host-scoped, not bound to a run
		},
		DroppedTotal:  droppedTotal,
		ObservedTotal: observedTotal,
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
	// ObservedTotal is the cumulative count of real kernel ground-truth events
	// the sensor has mapped off the tail (NOT heartbeats/blinds). /healthz reads
	// it to tell "sensor alive but observing nothing" (idle) apart from "events
	// flowing" (healthy) — a live heartbeat alone never proves ground truth.
	ObservedTotal uint64 `json:"observed_total"`
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
