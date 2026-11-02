// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import "testing"

// The failure tokens are a closed set the control plane keeps on a FAILED run; the phases are not in it,
// and the disk refusal's figures are whole GB, never negative.
func TestMacOSStartTokens(t *testing.T) {
	for _, r := range []string{MacOSCloneFailed, MacOSStartFailed, MacOSConnectFailed, MacOSDiskShort} {
		if !IsMacOSStartFailure(r) {
			t.Errorf("%s is not a start failure", r)
		}
	}
	for _, r := range []string{MacOSCloning, MacOSStarting, MacOSConnecting, "", "ImagePullBackOff"} {
		if IsMacOSStartFailure(r) {
			t.Errorf("%q is a start failure", r)
		}
	}
	if got, want := MacOSDiskShortMessage(85<<30+5, 40<<30), "macos: MacOSDiskShort: 85 GB reserved, 40 GB free"; got != want {
		t.Errorf("disk short = %q, want %q", got, want)
	}
	if got, want := MacOSDiskShortMessage(85<<30, -1), "macos: MacOSDiskShort: 85 GB reserved, 0 GB free"; got != want {
		t.Errorf("disk short with nothing free = %q, want %q", got, want)
	}
}
