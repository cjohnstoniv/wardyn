// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import "fmt"

// A macOS VM's start, as the status_detail its create reports: "macos: <Reason>[: <message>]". The
// three phases run in order while the run is STARTING; a failure names the phase it stopped in, and
// the control plane keeps it on the FAILED run (api/runs_status_detail.go). The console mirrors every
// token by name and value (ui/.../run-status-detail.ts's MACOS_START), pinned by a parity test there.
const (
	MacOSStartComponent = "macos"

	MacOSCloning    = "MacOSCloning"    // the helper clones the base image into the run's disk
	MacOSStarting   = "MacOSStarting"   // the VM is handed its network and boots
	MacOSConnecting = "MacOSConnecting" // the VM runs; its Wardyn agent is waited for and handed the sandbox

	MacOSCloneFailed   = "MacOSCloneFailed"
	MacOSStartFailed   = "MacOSStartFailed"
	MacOSConnectFailed = "MacOSConnectFailed"
	// MacOSDiskShort is the clone refused before it began: the Mac's free disk is under the run's
	// reservation. Its message is MacOSDiskShortMessage's.
	MacOSDiskShort = "MacOSDiskShort"
)

// macOSStartFailures is the closed set of the failure tokens above.
var macOSStartFailures = map[string]bool{MacOSCloneFailed: true, MacOSStartFailed: true, MacOSConnectFailed: true, MacOSDiskShort: true}

// IsMacOSStartFailure reports whether reason is the token of a macOS VM start that stopped.
func IsMacOSStartFailure(reason string) bool { return macOSStartFailures[reason] }

// MacOSStartDetail is the status_detail of a macOS VM start at reason.
func MacOSStartDetail(reason string) string { return MacOSStartComponent + ": " + reason }

// MacOSDiskShortMessage is MacOSDiskShort's detail, in whole GB (GiB, as every macOS disk figure the
// console shows): what the run reserves, the floor every admission keeps included, and what this Mac
// has free beyond the other VMs' reservations.
func MacOSDiskShortMessage(reservedBytes, freeBytes int64) string {
	return fmt.Sprintf("%s: %d GB reserved, %d GB free", MacOSStartDetail(MacOSDiskShort), reservedBytes>>30, max(freeBytes, 0)>>30)
}
