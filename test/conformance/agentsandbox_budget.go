// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package conformance

import "time"

// The limits of the agent-sandbox backend's keep and warm pool cases. The cases (the k8s-tagged files of this
// package's external test package) use these constants and TestAgentSandboxCaseBudgetsFitTheJobTimeout adds them
// up against ci.yml, so a raised limit or a new case reds the guard instead of drifting from the job's timeout.
const (
	ASKillBound        = time.Minute // startRun's cleanup: KillSandbox of the run
	ASWarmCleanupBound = time.Minute // warmPoolRunner's cleanup of the placeholders and the deny policy

	ASKeepServiceLimit   = 6 * time.Minute  // TestAgentSandboxKeepProxyThroughService, the case's context
	ASKeepSliceLimit     = 8 * time.Minute  // TestAgentSandboxKeepEndpointSliceElsewhereIsDropped
	ASKeepSliceDecoyWait = 2 * time.Minute  // its keepWaitFor for the decoy pod running
	ASKeepSliceGoneWait  = 10 * time.Second // for the agent's connect failing once the endpoint is elsewhere
	ASKeepSliceBackWait  = 30 * time.Second // for it succeeding once the endpoint is restored
	ASKeepReplaceLimit   = 8 * time.Minute  // TestAgentSandboxKeepLiveReplaceProxy
	ASKeepReplaceWait    = 30 * time.Second
	ASKeepHopfakeStart   = 3 * time.Minute  // keepProxyConfig's startHopFakeK8s: the hopfake build and its own 2-minute bound
	ASKeepSequenceLimit  = 15 * time.Minute // TestAgentSandboxKeepAPISequence, both of its runs
	ASKeepSequenceSettle = 5 * time.Second  // its sleep inside the remake window
	ASKeepSequenceStop   = 2 * time.Minute  // its keepWaitFor for no pod of the run remaining
	ASKeepSequenceHeld   = time.Minute      // for the held pod appearing behind the gate
	ASKeepAdmissionLimit = 2 * time.Minute  // TestAgentSandboxKeepAdmissionBoundsServices

	ASWarmReadyWait  = 180 * time.Second // both warm pool cases wait this long for a Ready placeholder
	ASWarmRefillWait = 120 * time.Second // TestWarmPoolPlaceholderClaimedAndRefilled, for the refill

	// ASMeasuredSuite is the go test step of the seven cases that ran hosted before the keep and warm pool cases:
	// 715 s and 658 s on two runs, the slower kept.
	ASMeasuredSuite = 715 * time.Second
)
