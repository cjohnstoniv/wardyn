// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"time"

	"github.com/cjohnstoniv/wardyn/internal/sweephealth"
)

// recordingSweepInterval is how often the recording retention sweep ticks.
const recordingSweepInterval = time.Hour

// sweepInstall is what decides which sweeps this install runs: the conditions
// startBackgroundWorkers starts them under, which registerSweepHealth mirrors.
type sweepInstall struct {
	runner                 bool          // a runner is wired
	autoStop               time.Duration // WARDYN_AUTOSTOP_INTERVAL; the idle reaper runs when above 0
	approvalExpiry         time.Duration // the approval expiry sweep runs when above 0
	recordingSweepable     bool          // the recording store can sweep
	recordingRetentionDays int           // WARDYN_RECORDING_RETENTION_DAYS; default 0 is off
	runOutputPersist       bool          // WARDYN_RUN_OUTPUT_PERSIST; the run output retention sweep runs when on
	api                    []sweephealth.Sweep
}

// registerSweepHealth registers, on every replica, every sweep whose start
// condition holds on this install. Registration does not depend on holding the
// sweeper lock: a follower registers the leader's sweeps and reads the leader's
// ticks from the shared record, which is what lets it notice a leader that
// stopped. A sweep whose condition does not hold is not registered, so it has no
// series and can never be stale.
func registerSweepHealth(t *sweephealth.Tracker, in sweepInstall) {
	sweeps := []sweephealth.Sweep{
		{Name: sweephealth.RunSecret, Interval: runSecretSweepInterval},
		{Name: sweephealth.CredentialExpiry, Interval: credentialSweepInterval},
	}
	if in.runner && in.autoStop > 0 {
		sweeps = append(sweeps, sweephealth.Sweep{Name: sweephealth.IdleReaper, Interval: in.autoStop})
	}
	if in.approvalExpiry > 0 {
		sweeps = append(sweeps, sweephealth.Sweep{Name: sweephealth.ApprovalExpiry, Interval: in.approvalExpiry})
	}
	if in.recordingSweepable && in.recordingRetentionDays > 0 {
		sweeps = append(sweeps, sweephealth.Sweep{Name: sweephealth.RecordingRetention, Interval: recordingSweepInterval})
	}
	if in.runner {
		sweeps = append(sweeps, sweephealth.Sweep{Name: sweephealth.TerminalSandbox, Interval: terminalSandboxSweepInterval})
	}
	if in.runOutputPersist {
		sweeps = append(sweeps, sweephealth.Sweep{Name: sweephealth.RunOutput, Interval: runOutputSweepInterval})
	}
	t.Register(append(sweeps, in.api...)...)
}
