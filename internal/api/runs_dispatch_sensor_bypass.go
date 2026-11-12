// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// GroundtruthSensorConfig is Config's view of the ground-truth sensor, embedded
// so its wiring lives beside the feature rather than in server.go.
type GroundtruthSensorConfig struct {
	// GroundtruthK8sSensor is set when wardynd delivers the sensor token to a
	// Kubernetes Secret (WARDYN_GROUNDTRUTH_TOKEN_SECRET), that is, when the
	// deployment runs the Kubernetes sensor DaemonSet. Default false: Compose
	// records the CC3 gap only, exactly as before, and keeps the sensor's own
	// WARDYN_GROUNDTRUTH_BLIND_RUNS list.
	GroundtruthK8sSensor bool
}

// sensorBypassReason is why the host sensor cannot see a run of this class, or
// "" when it can. Host eBPF sees the host kernel only: a Kata (CC3) guest has
// its own kernel, and a gVisor (CC2) guest's syscalls are served by the gVisor
// kernel, so the agent's processes never reach the host's process events.
// CC3 is recorded on every deployment (the long-standing behaviour); CC2 only
// where the Kubernetes sensor runs, which is where it replaces a static list.
func sensorBypassReason(class types.ConfinementClass, k8sSensor bool) string {
	switch {
	case class == types.CC3:
		return "cc3-kata-host-ebpf-blind"
	case class == types.CC2 && k8sSensor:
		return "cc2-gvisor-host-ebpf-blind"
	}
	return ""
}

// recordSensorBypass writes the run's one kernel.sensor.bypass row, as the
// daemon (actor_type system, actor wardynd, never the sensor's identity), so
// the gap is visible whether or not an operator listed the run anywhere. Same
// data shape as the sensor's own bypass event (reason, run_id), so downstream
// correlation reads both alike.
func (s *Server) recordSensorBypass(ctx context.Context, run types.AgentRun) {
	reason := sensorBypassReason(run.ConfinementClass, s.cfg.GroundtruthK8sSensor)
	if reason == "" {
		return
	}
	s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "kernel.sensor.bypass",
		run.ID.String(), "success", mustJSON(map[string]any{
			"reason": reason, "run_id": run.ID.String(),
		})))
}
