// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package groundtruth maps kernel-level eBPF (Tetragon) observations into
// Wardyn's audit vocabulary (types.AuditEvent): the tamper-proof
// ground-truth counterpart to the agent's self-report and PTY replay. A
// host-scoped sidecar (cmd/wardyn-tetragon-ingest) correlates each kernel
// event to a run via the `wardyn.run-id` container label, maps it here, and
// POSTs append-only batches keyed on run_id with a `kernel.*` action prefix
// and data.stream="ebpf" (Tetragon's JSON export, not gRPC, to keep go.mod light).
//
// HONESTY: this stream is detection, not prevention — the ld-linux/mmap
// dynamic-linker bypass of execve hooks is real and surfaced via data.loader=true
// rather than hidden; host eBPF is also blind inside CC3/Kata microVM guests,
// so callers emit a one-time kernel.sensor.bypass event.
package groundtruth

import (
	"encoding/json"
	"strings"

	"github.com/google/uuid"
)

// Kernel-event action namespace: every event carries one of these as
// types.AuditEvent.Action. The `kernel.` prefix is enforced by the ingest
// endpoint (rejected if absent) and is what SIEM rules key on alongside data.stream="ebpf".
const (
	ActionProcessExec    = "kernel.process.exec"    // process execve observed by the kernel sensor
	ActionNetworkConnect = "kernel.network.connect" // outbound TCP connect observed by the sensor
	ActionFileWrite      = "kernel.file.write"      // write to a sensitive path observed by the sensor

	// ActionSensorHeartbeat is the periodic liveness beat the ingest sidecar
	// emits (run_id NULL); /healthz keys ebpf_groundtruth state off the most
	// recent one within a TTL, so "healthy" requires events actually arriving.
	ActionSensorHeartbeat = "kernel.sensor.ping"
	// ActionSensorBlind is the one-time event for a run the host eBPF sensor
	// can't see into (CC3/Kata microVM guest); blindness is made visible, not silent.
	ActionSensorBlind = "kernel.sensor.bypass"
)

// KernelActionPrefix is the required prefix for every ground-truth action; the control-plane endpoint fail-closed rejects any action lacking it.
const KernelActionPrefix = "kernel."

// Stream is data.stream on every event here; SIEM rules and the UI
// discriminate ground-truth from agent self-report on it, without parsing the action.
const Stream = "ebpf"

// SensorActor is the fixed audit actor for sensor-originated events. SECURITY:
// the control plane forces actor=SensorActor + actor_type=system on ingest, so a compromised sensor can't impersonate a human or an agent run.
const SensorActor = "wardyn-tetragon-ingest"

// Correlation records whether an event was bound to a known Wardyn run.
type Correlation string

const (
	CorrelationMapped Correlation = "mapped" // the event's container/cgroup resolved to a run
	// CorrelationUnmapped means it did not; never silently dropped, but
	// emitted with run_id NULL so an unmapped kernel event near a run is a visible signal.
	CorrelationUnmapped Correlation = "unmapped"
)

// Subtype is the fine-grained kernel-event kind carried in data.subtype: stable, machine-readable, one-to-one with the action where applicable.
type Subtype string

const (
	SubtypeProcessExec    Subtype = "process_exec"
	SubtypeNetworkConnect Subtype = "network_connect"
	SubtypeFileWrite      Subtype = "file_write"
)

// EventData is the JSON shape stored in audit_events.data for every event here
// (audit_events.data is JSONB, so this needs no schema change); deliberately
// small and stable so SIEM rules can rely on it.
type EventData struct {
	Stream      string   `json:"stream"`                 // always Stream ("ebpf")
	Subtype     Subtype  `json:"subtype"`                // fine-grained kernel-event kind
	CgroupID    uint64   `json:"cgroup_id,omitempty"`    // kernel cgroup id of the observed process (0 if unknown)
	ContainerID string   `json:"container_id,omitempty"` // (truncated) container id the event was attributed to
	Argv        []string `json:"argv,omitempty"`         // full process command line for exec events
	Dst         string   `json:"dst,omitempty"`          // "ip:port" for network_connect events
	Path        string   `json:"path,omitempty"`         // written file path for file_write events
	// Loader is true when the exec'd binary is a dynamic linker (ld-linux /
	// ld-musl): such an exec can load+run an arbitrary ELF the execve hook
	// never named. Flagged, not blocked.
	Loader      bool        `json:"loader,omitempty"`
	Correlation Correlation `json:"correlation"`      // "mapped" or "unmapped"
	Reason      string      `json:"reason,omitempty"` // free-form explanation for sensor.blind / failure cases
}

// MarshalData serialises d for an AuditEvent.Data field, folding in the error
// since marshalling a fixed struct can't realistically fail (re-marshal for strictness).
func (d EventData) marshal() json.RawMessage {
	b, err := json.Marshal(d)
	if err != nil {
		return json.RawMessage(`{"stream":"ebpf","correlation":"unmapped"}`)
	}
	return b
}

// Correlator resolves a container id to a Wardyn run. The ingest sidecar
// implements it by indexing docker containers labelled wardyn.managed=true
// (fed by `docker events`, reconciled by `docker ps -a`; no cgroup-id index,
// since this export shape carries none) — the only correlation knowledge
// this package has, keeping the mapper target-agnostic and unit-testable.
type Correlator interface {
	// RunForContainer returns the run id for a container id (any prefix
	// length Tetragon emits); ok is false for unknown / non-Wardyn containers.
	RunForContainer(containerID string) (runID uuid.UUID, ok bool)
}

// loaderPrefixes are dynamic-linker path shapes flagged with loader=true, matched as a prefix check against the resolved binary path.
var loaderPrefixes = []string{
	"/lib/ld-linux",     // /lib/ld-linux.so.2
	"/lib64/ld-linux",   // /lib64/ld-linux-x86-64.so.2
	"/lib32/ld-linux",   // 32-bit on multilib
	"/usr/lib/ld-linux", // some distros
	"/lib/ld-musl",      // /lib/ld-musl-x86_64.so.1 (Alpine)
	"/usr/lib/ld-musl",
}

// IsDynamicLinker reports whether binary is a dynamic linker / loader (the
// ld-linux/mmap bypass surface, flagged not prevented), including as the
// first argv token (`ld-linux.so ./payload`).
func IsDynamicLinker(binary string) bool {
	b := strings.TrimSpace(binary)
	if b == "" {
		return false
	}
	// Normalise: a bare basename (e.g. "ld-musl-x86_64.so.1") also counts.
	base := b
	if i := strings.LastIndex(b, "/"); i >= 0 {
		base = b[i+1:]
	}
	if strings.HasPrefix(base, "ld-linux") || strings.HasPrefix(base, "ld-musl") {
		return true
	}
	for _, p := range loaderPrefixes {
		if strings.HasPrefix(b, p) {
			return true
		}
	}
	return false
}
