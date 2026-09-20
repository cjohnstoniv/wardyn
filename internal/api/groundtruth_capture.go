// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// groundtruth_capture.go — what a REVIEWER is told about how much of a
// capture the kernel actually corroborated.
//
// Two different facts get separate caveats, not one shared sentence: the
// eBPF sensor is host-wide and its heartbeat is global, so folding it into a
// per-run caveat mislabels the run. A capture recorded while the sensor was
// down, reviewed after it recovered, would carry no caveat at all — and a
// well-corroborated capture reviewed during an idle patch would be told it
// had "no kernel-level corroboration". The host's state would stand in for
// the run's.
//
// They are separated here. The capture's own claim comes from the capture's
// own evidence — the exec/connect/file-write lines the sensor bound to THIS
// run — and the host's state is reported as what it is: review-time state.
package api

import (
	"context"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/recordmode"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// DRAFT (M2 canon pending)

// The kernel-ground-truth notes a reviewer reads on a capture
// (RecordTaskResult.Caveats) and on a synthesized profile
// (profileResponse.Warnings).
const (
	// The capture's OWN claim, from its own events.
	captureNoCorroborationCaveat = "kernel corroboration: none for this capture — no exec, connect or file-write was " +
		"bound to this run by the kernel sensor, so the proxy's own egress decisions are the whole of the evidence here"
	// The four host-sensor states, as REVIEW-TIME
	// state about the host rather than a claim about this capture.
	hostSensorUnavailableCaveat = "kernel ground truth (host sensor, as of this review): unavailable — no eBPF sensor " +
		"heartbeat has ever been observed on this host"
	hostSensorDegradedCaveat = "kernel ground truth (host sensor, as of this review): degraded — the eBPF sensor's " +
		"heartbeat is stale; whether it was healthy while this capture ran is a separate question, which this capture's own kernel evidence is what answers"
	hostSensorIdleCaveat = "kernel ground truth (host sensor, as of this review): idle — the eBPF sensor is alive but " +
		"has mapped zero kernel events; whether it was healthy while this capture ran is a separate question, which this capture's own kernel evidence is what answers"
	hostSensorPartialCaveatPrefix = "kernel ground truth (host sensor, as of this review): partial — the eBPF sensor " +
		"has never observed "
	hostSensorPartialCaveatSuffix = "; whether it was healthy while this capture ran is a separate question, which this capture's own kernel evidence is what answers"
)

// captureNoCorroborationFragment is the substring a test (or a UI filter) uses
// to recognise the per-capture note without pinning the whole sentence.
const captureNoCorroborationFragment = "kernel corroboration: none for this capture"

// groundtruthCaveats returns the kernel-ground-truth notes for ONE capture, in
// reading order: what this capture's own evidence shows, then what the host
// sensor is doing at review time.
//
// The first line is the one that answers the reviewer's actual question, and
// it needs no store round trip: ExecArgv0s, Connects and FileWrites are the
// kernel's own lines about THIS run, and their absence is the whole of the
// claim. The second is the host's state, which is worth reporting — a sensor
// that is down now is a real operational fact — but which is no longer allowed
// to speak for the capture.
func (s *Server) groundtruthCaveats(ctx context.Context, obs recordmode.Observations) []string {
	var out []string
	if len(obs.ExecArgv0s) == 0 && len(obs.Connects) == 0 && len(obs.FileWrites) == 0 {
		out = append(out, captureNoCorroborationCaveat)
	}
	if host := s.hostSensorCaveat(ctx); host != "" {
		out = append(out, host)
	}
	return out
}

// hostSensorCaveat is the review-time state of the host's eBPF sensor, read
// through the SAME ebpfGroundtruthStatus /healthz uses so the two surfaces can
// never disagree about what "healthy" means. "" when it is fully healthy —
// there is nothing to report.
func (s *Server) hostSensorCaveat(ctx context.Context) string {
	status := s.ebpfGroundtruthStatus(ctx)
	switch status["state"] {
	case "unavailable":
		return hostSensorUnavailableCaveat
	case "degraded":
		return hostSensorDegradedCaveat
	case "idle":
		return hostSensorIdleCaveat
	case "partial":
		missing, _ := status["missing_kinds"].([]string)
		return hostSensorPartialCaveatPrefix + strings.Join(missing, ", ") + hostSensorPartialCaveatSuffix
	default: // "healthy", or absent (tests with no Store)
		return ""
	}
}

// kernelWindow reports what the sensor said about itself WHILE run was being
// captured — the one input Capture cannot derive from the run's own events.
//
// The window is [run.CreatedAt, run.UpdatedAt] — the run's own row, which for
// the production caller (reconcileRecordRun, at the terminal transition) is the
// capture. `now` stands in for the end only when UpdatedAt is unusable: before
// CreatedAt, or in the future. A still-recording run reviewed mid-flight
// therefore gets the NARROWER window ending at its last row update, which fails
// silent rather than false.
//
// Only the LATEST heartbeat is consulted, because that is the only one the
// store can answer without a new query shape. So this is a conservative
// detector: it fires when the newest beat happens to fall inside the window,
// and stays silent otherwise — it never invents an anomaly out of a beat that
// says nothing about this run.
//
// Newest-beat-in-window, not a per-window counter delta. A delta needs a
// heartbeats-between-two-times query; add one if a real deployment shows this
// missing drops it should have caught.
func (s *Server) kernelWindow(ctx context.Context, run types.AgentRun) recordmode.KernelWindow {
	if s.cfg.Store == nil {
		return recordmode.KernelWindow{}
	}
	status := s.ebpfGroundtruthStatus(ctx)
	beat, ok := status["last_heartbeat"].(string)
	if !ok {
		return recordmode.KernelWindow{} // no sensor has ever beaten here
	}
	at, err := time.Parse(time.RFC3339, beat)
	if err != nil {
		return recordmode.KernelWindow{}
	}
	end := run.UpdatedAt
	if now := s.cfg.Now().UTC(); end.Before(run.CreatedAt) || end.After(now) {
		end = now
	}
	if at.Before(run.CreatedAt.Add(-time.Second)) || at.After(end.Add(time.Second)) {
		return recordmode.KernelWindow{}
	}
	// BOTH counters, because neither means anything alone: drops are
	// cumulative and count every host event outside a sandbox, so only "drops
	// with nothing correlated" is a fact about correlation rather than about
	// the host being busy. Capture applies that pairing.
	dropped, _ := status["dropped_unmapped"].(uint64)
	observed, _ := status["observed_total"].(uint64)
	return recordmode.KernelWindow{Beat: true, DroppedUnmapped: dropped, ObservedTotal: observed}
}
