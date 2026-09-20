// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

// TerminalWaitingReasons are the substrate "still waiting" reasons that will
// never resolve on their own: waiting longer changes nothing, and only a person
// (a fixed image reference, a registry credential, a working entrypoint) does.
//
// It lives HERE, in the tagless runner package, rather than in the k8s driver
// that reads it, because three places have to agree on the same six strings and
// two of them cannot see a `k8s`-tagged symbol: the driver's own fast-fail poll
// (k8s/canary.go's terminalWaitingReasons, which is this map), the control
// plane's read projection (a terminal reason is the one startup detail that
// survives a FAILED run, api/runs_status_detail.go), and the console's mirror
// (ui/.../run-status-detail.ts's TERMINAL_STATUS_REASONS, which a vitest case
// pins against this list by hand). Not exhaustive — every other reason is
// covered by the wait's own timeout — this is the fast path for the cases that
// are decidable the moment the kubelet says them.
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
