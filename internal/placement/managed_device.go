// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package placement

import "github.com/cjohnstoniv/wardyn/internal/types"

// managedDeviceRefusal is runners.require_managed_device: with it on, a plan judged at a personal runner
// is refused. The gate is the security operator's trust mark (managed_device), which a person checks
// against the MDM inventory; it is not a certificate or hardware attestation. org_cluster runners and
// an unknown class are not touched here (an unknown class already refuses elsewhere). The default is off
// so that an existing install keeps placing on personal runners until an admin turns it on.
func managedDeviceRefusal(p LocalPlan) *Refusal {
	if !p.RequireManagedDevice || p.Trust != types.RunnerTrustPersonal {
		return nil
	}
	return refuse(ReasonRunnerTrustRequired, "trust_class",
		"this runner is not marked as company-managed; ask a security admin to mark it as a managed device")
}
