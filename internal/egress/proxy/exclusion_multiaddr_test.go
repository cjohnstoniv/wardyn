// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"net"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestControlPlaneExclusionCoversEveryResolvedAddress pins the other half of
// F002: resolveControlPlaneIP kept ips[0] ONLY, so a wardynd behind more than
// one A record had exactly one of its addresses excluded — while
// THREAT-MODEL.md states the exclusion covers "its resolved control-plane
// host". vetTrustedHost already checks every answer; this mirrors it.
func TestControlPlaneExclusionCoversEveryResolvedAddress(t *testing.T) {
	p := newProxy(Options{
		RunID:           uuid.New(),
		Policy:          CompilePolicy(types.RunPolicySpec{}),
		ControlPlaneIPs: ips("10.40.0.9", "10.40.0.10"),
	})
	for _, addr := range []string{"10.40.0.9", "10.40.0.10"} {
		if !p.onOwnSubnetOrControlPlane(net.ParseIP(addr)) {
			t.Fatalf("control-plane address %s is not excluded: EVERY answer for the control-plane host "+
				"must be, not just the first", addr)
		}
	}
	if p.onOwnSubnetOrControlPlane(net.ParseIP("10.40.0.11")) {
		t.Fatal("an unrelated address must not be excluded")
	}
}
