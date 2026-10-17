// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

// TerminalWaitingReasons are the substrate "still waiting" reasons that will
// never resolve on their own (waiting longer changes nothing; only a person
// fixing the image, credential, or entrypoint does).
//
// Lives here in the tagless runner package, not the k8s driver that reads it,
// because three places must agree on the same six strings and two can't see
// a `k8s`-tagged symbol: the driver's fast-fail poll, the control plane's
// read projection (api/runs_status_detail.go), and the console's mirror
// (ui/.../run-status-detail.ts's TERMINAL_STATUS_REASONS). Not exhaustive —
// every other reason is covered by the wait's own timeout.
var TerminalWaitingReasons = map[string]bool{
	"ImagePullBackOff":           true,
	"ErrImagePull":               true,
	"CreateContainerError":       true,
	"CreateContainerConfigError": true,
	"InvalidImageName":           true,
	"CrashLoopBackOff":           true,
}

// IsTerminalWaitingReason reports whether reason is one that waiting cannot fix.
// The empty reason is not: "we have not read one yet" is not a verdict.
func IsTerminalWaitingReason(reason string) bool { return TerminalWaitingReasons[reason] }

// CapacityBlockerReasons is the CLOSED set of substrate reasons that mean a starting sandbox is
// waiting for room rather than for work: the fleet capacity view lists a STARTING run carrying
// one as unschedulable, and the startup wait decides its capacity wait against this same set.
var CapacityBlockerReasons = map[string]bool{"Unschedulable": true}
