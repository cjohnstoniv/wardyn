// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package placement

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A person's disk drive goes to a local runner only when it is kept on that runner (the API verifies
// the drive's Mac per candidate), and is refused on any other.
func TestLocalEligibilityHoldsADiskDriveToItsMac(t *testing.T) {
	plan := LocalPlan{Spec: runner.SandboxSpec{Drives: []types.DriveMount{{Backend: types.DriveBackendMacDisk, ObjectName: "wardyn-drive-x"}}}}
	if _, ref := LocalEligibility(plan); ref == nil || ref.Field != "SandboxSpec.Drives[0]" || ref.Reason != ReasonPlacementLocalPath {
		t.Fatalf("a disk drive kept on another runner = %+v, want it refused", ref)
	}
	plan.VerifiedLocalPaths = map[string]bool{"SandboxSpec.Drives[0]": true}
	if _, ref := LocalEligibility(plan); ref != nil && ref.Field == "SandboxSpec.Drives[0]" {
		t.Fatalf("a disk drive kept on this runner was refused: %+v", ref)
	}
}
