// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build darwin && arm64 && cgo

package vzengine

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
)

// A plan's shares become one virtio-fs device each, in order, and no plan without shares gets one.
func TestShareDevicesOnePerShare(t *testing.T) {
	devs, err := shareDevices([]macvm.Share{
		{Tag: macvm.ShareTag(0), HostPath: t.TempDir(), ReadOnly: true},
		{Tag: macvm.ShareTag(1), HostPath: t.TempDir()},
	})
	if err != nil || len(devs) != 2 {
		t.Fatalf("shareDevices = %d devices, %v; want 2, nil", len(devs), err)
	}
	if devs, err := shareDevices(nil); err != nil || len(devs) != 0 {
		t.Fatalf("shareDevices(nil) = %d devices, %v; want none", len(devs), err)
	}
}
