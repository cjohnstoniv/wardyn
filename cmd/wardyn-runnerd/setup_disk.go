// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"cmp"
	"context"
	"fmt"
	"io"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// engineVMDisk is the free disk of a VM-backed engine as a container on it sees it (ok false when the
// engine is not VM-backed or the probe failed), and engineDiskCapped whether the engine can cap a run's
// disk. Both are swapped in tests.
var (
	engineVMDisk     = vmEngineDiskMiB
	engineDiskCapped = diskCapEnforced
)

// noDiskCapLine is what setup says, once, when runs on this engine have no disk cap.
const noDiskCapLine = "runs on this engine have no disk cap (overlayfs); see docs/operations/hybrid-laptops.md"

// measureWithVMDisk is measureHost plus, on a VM-backed engine whose data root is not a path here, the
// VM's free disk read by the proxy image (image, else the release image). A probe that cannot run leaves
// the disk unmeasured, so --total-disk is asked for as before.
func measureWithVMDisk(ctx context.Context, image string) (types.RunnerAmounts, error) {
	m, err := measureHost(ctx)
	if err != nil || m.DiskMiB > 0 {
		return m, err
	}
	if mib, ok := engineVMDisk(ctx, cmp.Or(image, releaseProxyImage())); ok {
		m.DiskMiB = mib
	}
	return m, nil
}

// sayNoDiskCap prints noDiskCapLine when the engine cannot cap a run's disk. An engine that does not
// answer prints nothing: setup's own engine check has already spoken.
func sayNoDiskCap(ctx context.Context, out io.Writer) {
	if capped, ok := engineDiskCapped(ctx); ok && !capped {
		fmt.Fprintln(out, noDiskCapLine)
	}
}
