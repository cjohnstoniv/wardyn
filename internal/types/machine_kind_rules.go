// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"fmt"
	"slices"
)

// MachineKindDenial refuses one machine kind on runners of one trust class (#2950): a new local run of
// that kind is not placed on such a runner. The list is deny-only, so it can only narrow where a kind
// runs, and no entry admits a kind a built-in rule refuses (a macOS VM needs a managed_device runner).
type MachineKindDenial struct {
	Trust       RunnerTrustClass `json:"trust_class"`
	MachineKind MachineKind      `json:"machine_kind"`
}

// ValidateMachineKindDenials refuses an entry outside the closed sets, an empty kind, and a repeat.
func ValidateMachineKindDenials(ds []MachineKindDenial) error {
	seen := map[MachineKindDenial]bool{}
	for i, d := range ds {
		if !d.Trust.Valid() {
			return fmt.Errorf("machine_kind_denials[%d]: trust_class %q is not %q, %q or %q", i, d.Trust,
				RunnerTrustPersonal, RunnerTrustManagedDevice, RunnerTrustOrgCluster)
		}
		if d.MachineKind == "" || d.MachineKind.Validate() != nil {
			return fmt.Errorf("machine_kind_denials[%d]: machine_kind %q is not %q, %q or %q", i, d.MachineKind,
				MachineContainer, MachineLinuxVM, MachineMacOSVM)
		}
		d.MachineKind = d.MachineKind.Normalize()
		if seen[d] {
			return fmt.Errorf("machine_kind_denials[%d]: %s on %s runners is listed twice", i, ds[i].MachineKind, d.Trust)
		}
		seen[d] = true
	}
	return nil
}

// DeniesMachineKind reports whether ds refuses kind on a runner of trust; "" and "container" are one kind.
func DeniesMachineKind(ds []MachineKindDenial, trust RunnerTrustClass, kind MachineKind) bool {
	return slices.ContainsFunc(ds, func(d MachineKindDenial) bool {
		return d.Trust == trust && d.MachineKind.Normalize() == kind.Normalize()
	})
}
