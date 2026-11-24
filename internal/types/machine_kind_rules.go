// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"fmt"
	"slices"

	"github.com/google/uuid"
)

// MachineKindDenial refuses one machine kind on runners of one trust class (#2950): a new local run of
// that kind is not placed on such a runner. The list is deny-only, so it can only narrow where a kind
// runs, and no entry admits a kind a built-in rule refuses (a macOS VM needs a managed_device runner).
// SiteID, when set, narrows the entry to runners whose pool is at that organisation site (#2997): it
// holds nowhere else, so a site can only take targets away, never add one.
type MachineKindDenial struct {
	Trust       RunnerTrustClass `json:"trust_class"`
	MachineKind MachineKind      `json:"machine_kind"`
	SiteID      string           `json:"site_id,omitempty"`
}

// ValidateMachineKindDenials refuses an entry outside the closed sets, an empty kind, a site id that is not
// a lower-case UUID, and a repeat. Whether the site exists is the caller's check.
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
		if id, err := uuid.Parse(d.SiteID); d.SiteID != "" && (err != nil || id.String() != d.SiteID) {
			return fmt.Errorf("machine_kind_denials[%d]: site_id %q is not a site id", i, d.SiteID)
		}
		d.MachineKind = d.MachineKind.Normalize()
		if seen[d] {
			where := "at every site"
			if d.SiteID != "" {
				where = "at site " + d.SiteID
			}
			return fmt.Errorf("machine_kind_denials[%d]: %s on %s runners is listed twice %s", i, ds[i].MachineKind, d.Trust, where)
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

// MachineKindDenialsAt is the entries of ds that hold for a runner whose pool is at site: every entry with
// no site, and the entries keyed to site. With site nil (no site, or sites off) only the unkeyed ones hold.
func MachineKindDenialsAt(ds []MachineKindDenial, site *uuid.UUID) []MachineKindDenial {
	var out []MachineKindDenial
	for _, d := range ds {
		if d.SiteID == "" || (site != nil && d.SiteID == site.String()) {
			out = append(out, d)
		}
	}
	return out
}
