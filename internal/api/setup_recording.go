// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import "fmt"

// recordingCheck is the recording row: whether session recording is on, which store is selected, and, when
// it is off, why and how to turn it on. selected is the components.recording entry /healthz reports;
// configured is the WARDYN_RECORDING_STORE value, which /healthz reduces to "none" when no store came up.
// ha drops the remedies a high-availability install refuses (the fs store and the chart's persistence).
// Never Blocking: runs work without a recording, and the operator reads this row in the place that fixes it.
//
// An off store is not told apart by who chose it: the chart renders WARDYN_RECORDING_STORE=off for an
// install without persistence, and an operator who sets off by hand writes the same value, so both warn.
func recordingCheck(selected ComponentInfo, configured string, ha bool) SetupCheck {
	chk := SetupCheck{ID: "recording", Label: "Session recording"}
	if selected.Selected != "none" || selected.Source != "disabled" {
		chk.Status = "info"
		chk.Detail = fmt.Sprintf("Session recording is on: the %s store is selected (%s).", selected.Selected, selected.Source)
		return chk
	}
	chk.Status = "warn"
	const pg = "set WARDYN_RECORDING_STORE=pg (Helm: env.WARDYN_RECORDING_STORE=pg), which keeps recordings in Postgres and needs no volume"
	switch configured {
	case "fs":
		chk.Cause = "no_dir"
		chk.Detail = "Session recording is off: WARDYN_RECORDING_STORE is fs and the recording directory (-recording-dir) is empty, so there is nowhere to write recordings."
		chk.Fix = "Point -recording-dir (WARDYN_RECORDING_DIR) at a writable path, or " + pg + "."
	default:
		chk.Cause = "store_off"
		chk.Detail = "Session recording is off: WARDYN_RECORDING_STORE is off (the Helm chart's default without persistence, or set by hand)."
		chk.Fix = "To record, " + pg
		if !ha {
			chk.Fix += ", or use the fs store on a volume (Helm: persistence.enabled=true)"
		}
		chk.Fix += "."
	}
	chk.Detail += " No session is recorded or replayable, and every run is marked not_recorded: org_off, self-hosted runners' runs included."
	return chk
}

// recordingRows is the recording row as a list, empty when the server was given no recording component.
func (s *Server) recordingRows() []SetupCheck {
	sel, ok := s.cfg.Components["recording"]
	if !ok {
		return nil
	}
	return []SetupCheck{recordingCheck(sel, s.cfg.RecordingSelection, s.cfg.HA)}
}
