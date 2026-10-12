// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The member-drive refusals: the server sentences of storage.user_drive.member_create.
const (
	providers400MemberMax    = "storage.user_drive.member_create.max_per_person: %d is out of range — name 1 to %d drives per person"
	providers400MemberSize   = "storage.user_drive.member_create.size_mib: %d is above storage.user_drive.max_size_mib (%d)"
	providers400MemberNoSize = "storage.user_drive.member_create.size_mib: name the size of each drive's claim in MiB"
	providers400MemberDocker = "storage.user_drive.member_create.size_mib: a Docker volume has no size limit, so a size here would be a promise nothing keeps"
	providers400MemberClass  = "storage.user_drive.member_create.storage_class: a Docker volume has no storage class"
)

// validateMemberDrivePolicy holds the target-independent member-drive rules: a
// switch that is on names a per-person count in range, and a claim size is
// non-negative and under the deployment's drive ceiling. The rules that need the
// runner (Docker takes no size, k8s needs one) are validateMemberDriveTarget's.
func validateMemberDrivePolicy(m *types.MemberDrivePolicy, ceilingMiB int) error {
	if m == nil {
		return nil
	}
	if m.Enabled && (m.MaxPerPerson < 1 || m.MaxPerPerson > types.MaxMemberDrivesPerPerson) {
		return fmt.Errorf(providers400MemberMax, m.MaxPerPerson, types.MaxMemberDrivesPerPerson)
	}
	if m.SizeMiB < 0 {
		return fmt.Errorf(providers400Negative, "storage.user_drive.member_create.size_mib", m.SizeMiB)
	}
	if ceilingMiB > 0 && m.SizeMiB > ceilingMiB {
		return fmt.Errorf(providers400MemberSize, m.SizeMiB, ceilingMiB)
	}
	return nil
}

// validateMemberDriveTarget is validateMemberDrivePolicy's runner-aware half, a
// Server method for the same reason as validateADOTokenModes: the pure validator
// cannot see which substrate this deployment runs. A runner that is neither
// Docker nor k8s (none, or several) is left to the create route's own refusal.
func (s *Server) validateMemberDriveTarget(p *types.WorkspaceProviders) error {
	if p == nil || p.Storage == nil || p.Storage.UserDrive == nil || p.Storage.UserDrive.MemberCreate == nil || s.cfg.Runner == nil {
		return nil
	}
	m := p.Storage.UserDrive.MemberCreate
	switch s.cfg.Runner.Name() {
	case "docker":
		if m.SizeMiB > 0 {
			return fmt.Errorf("%s", providers400MemberDocker)
		}
		if m.StorageClass != "" {
			return fmt.Errorf("%s", providers400MemberClass)
		}
	case "k8s":
		if m.Enabled && m.SizeMiB <= 0 {
			return fmt.Errorf("%s", providers400MemberNoSize)
		}
	}
	return nil
}
