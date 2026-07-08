// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package groundtruth maps kernel-level observations (from an eBPF sensor —
// specifically Tetragon) into Wardyn's append-only audit vocabulary
// (types.AuditEvent). It is the SECOND of Wardyn's three advertised audit
// streams: the tamper-proof "ground-truth" counterpart to the agent's own
// self-report (the Postgres event log) and the human-watchable PTY replay.
//
// DESIGN (mirrors the proxy -> /internal/decisions -> recordAudit pattern):
// a host-scoped sidecar (cmd/wardyn-tetragon-ingest) consumes Tetragon kernel
// events, correlates each to a Wardyn run via the container label
// `wardyn.run-id`, maps a bounded subset to types.AuditEvent here, and POSTs
// batches to a new internal endpoint that records them append-only — so they
// land in Postgres AND fan to every SIEM sink with ZERO new fanout code. Every
// mapped event is keyed on run_id and discriminated by a `kernel.*` action
// prefix plus data.stream="ebpf".
//
// DEPENDENCY CHOICE: this package consumes Tetragon's JSON EXPORT stream
// (line-delimited JSON written to a file/stdout via Tetragon's export feature),
// NOT the Tetragon gRPC client. Defining the minimal structs we need (below)
// keeps go.mod light — no github.com/cilium/tetragon dependency — and is honest
// about exactly which fields we read.
//
// HONESTY: this stream is DETECTION, not prevention. The ld-linux/mmap
// dynamic-linker bypass of execve hooks is real (the "Ona Veto" lesson) and is
// surfaced (not hidden) via data.loader=true rather than suppressed; we never
// claim exec-blocking. Host eBPF is also blind inside CC3/Kata microVM guests —
// callers must emit a one-time kernel.sensor.blind event for such runs.
//
// This package is target-agnostic: it has zero knowledge of Docker, HTTP, the
// store, or how runs are correlated beyond the small Correlator interface.
package groundtruth

import (
	"encoding/json"
	"strings"

	"github.com/google/uuid"
)

// Kernel-event action namespace. Every event this stream emits carries one of
// these as types.AuditEvent.Action. The `kernel.` prefix is the discriminator
// the control plane enforces on the ground-truth ingest endpoint (any action
// without it is rejected) and the prefix SIEM rules key on alongside
// data.stream="ebpf".
const (
	// ActionProcessExec is a process execve observed by the kernel sensor.
	ActionProcessExec = "kernel.process.exec"
	// ActionNetworkConnect is an outbound TCP connect observed by the sensor.
	ActionNetworkConnect = "kernel.network.connect"
	// ActionFileWrite is a write to a sensitive path observed by the sensor.
	ActionFileWrite = "kernel.file.write"

	// ActionSensorHeartbeat is the periodic liveness beat the ingest sidecar
	// emits (run_id NULL). /healthz keys ebpf_groundtruth state off the most
	// recent one within a TTL — so the stream is only "healthy" when events
	// are actually arriving (the overclaim is structurally impossible).
	ActionSensorHeartbeat = "kernel.sensor.heartbeat"
	// ActionSensorBlind is the one-time event emitted for a run the host eBPF
	// sensor cannot see into (CC3/Kata microVM guest). Blindness is made
	// VISIBLE rather than silent.
	ActionSensorBlind = "kernel.sensor.blind"
)

// KernelActionPrefix is the required prefix for every ground-truth action. The
// control-plane endpoint rejects any submitted action lacking it (fail closed).
const KernelActionPrefix = "kernel."

// Stream is the value of data.stream on every event from this stream. SIEM
// rules and the UI discriminate ground-truth events from agent self-report on
// (data.stream == StreamEBPF) without parsing the action.
const Stream = "ebpf"

// SensorActor is the fixed audit actor for sensor-originated events. The
// control plane FORCES actor=SensorActor + actor_type=system on ingest, so a
// compromised sensor cannot impersonate a human or an agent run.
const SensorActor = "wardyn-tetragon-ingest"

// Correlation records whether an event was bound to a known Wardyn run.
type Correlation string

const (
	// CorrelationMapped means the event's container/cgroup resolved to a run.
	CorrelationMapped Correlation = "mapped"
	// CorrelationUnmapped means it did not. Such events are NEVER silently
	// dropped — they are emitted with run_id NULL so the blindness is visible
	// in the audit stream (an unmapped kernel event near a run is a signal).
	CorrelationUnmapped Correlation = "unmapped"
)

// Subtype is the fine-grained kernel-event kind carried in data.subtype. It is
// stable, machine-readable, and one-to-one with the action where applicable.
type Subtype string

const (
	SubtypeProcessExec    Subtype = "process_exec"
	SubtypeNetworkConnect Subtype = "network_connect"
	SubtypeFileWrite      Subtype = "file_write"
)

// EventData is the JSON shape stored in audit_events.data for every event from
// this stream (audit_events.data is JSONB; this requires NO schema change). It
// is deliberately small and stable so SIEM rules can rely on it.
type EventData struct {
	// Stream is always Stream ("ebpf").
	Stream string `json:"stream"`
	// Subtype is the fine-grained kernel-event kind.
	Subtype Subtype `json:"subtype"`
	// CgroupID is the kernel cgroup id of the observed process (0 if unknown).
	CgroupID uint64 `json:"cgroup_id,omitempty"`
	// ContainerID is the (truncated) container id the event was attributed to.
	ContainerID string `json:"container_id,omitempty"`
	// Argv is the full process command line for exec events.
	Argv []string `json:"argv,omitempty"`
	// Dst is "ip:port" for network_connect events.
	Dst string `json:"dst,omitempty"`
	// Path is the written file path for file_write events.
	Path string `json:"path,omitempty"`
	// Loader is true when the exec'd binary is a dynamic linker (ld-linux /
	// ld-musl). This is the ld-linux/mmap bypass surfaced honestly: such an
	// exec can load+run an arbitrary ELF the execve hook never named, so the
	// argv of an ld-linux invocation is the real program. We FLAG it, we do
	// not claim to block it.
	Loader bool `json:"loader,omitempty"`
	// Correlation is "mapped" or "unmapped".
	Correlation Correlation `json:"correlation"`
	// Reason carries a free-form explanation for sensor.blind / failure cases.
	Reason string `json:"reason,omitempty"`
}

// MarshalData serialises d for an AuditEvent.Data field. The error is folded in
// because audit data marshalling of a fixed struct cannot realistically fail;
// callers that want strictness can re-marshal.
func (d EventData) marshal() json.RawMessage {
	b, err := json.Marshal(d)
	if err != nil {
		// A fixed-shape struct cannot fail to marshal; fall back to a minimal
		// valid object rather than emit invalid JSONB.
		return json.RawMessage(`{"stream":"ebpf","correlation":"unmapped"}`)
	}
	return b
}

// Correlator resolves a container id and/or kernel cgroup id to a Wardyn run.
// The ingest sidecar implements this by listing docker containers labelled
// wardyn.managed=true and indexing them by container id (and, where available,
// cgroup id). It is the only knowledge this package has about how correlation
// happens — keeping the mapper target-agnostic and unit-testable.
type Correlator interface {
	// RunForContainer returns the run id for a container id (any prefix length
	// Tetragon emits) and whether it is a Wardyn-managed agent container. ok
	// is false for unknown / non-Wardyn containers.
	RunForContainer(containerID string) (runID uuid.UUID, ok bool)
}

// loaderPrefixes are the dynamic-linker path shapes we flag with loader=true.
// Matched as a path-suffix-aware prefix check against the resolved binary path.
var loaderPrefixes = []string{
	"/lib/ld-linux",     // /lib/ld-linux.so.2
	"/lib64/ld-linux",   // /lib64/ld-linux-x86-64.so.2
	"/lib32/ld-linux",   // 32-bit on multilib
	"/usr/lib/ld-linux", // some distros
	"/lib/ld-musl",      // /lib/ld-musl-x86_64.so.1 (Alpine)
	"/usr/lib/ld-musl",
}

// IsDynamicLinker reports whether binary is a dynamic linker / loader. Exec of a
// loader is the ld-linux/mmap bypass surface: it can run an ELF the execve hook
// never named. We flag it (data.loader=true) so it is visible; we do NOT claim
// to prevent it. The check also matches the loader appearing as the FIRST argv
// token (the common `ld-linux.so ./payload` invocation form).
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
