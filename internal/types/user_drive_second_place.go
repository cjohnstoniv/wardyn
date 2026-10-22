// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

// ErrDriveSecondPlace refuses a second place that is not a share on one runner,
// or a second place on a drive that is not itself a share. The caller words it.
var ErrDriveSecondPlace = errors.New("the second place must also be a share, and somewhere the first is not")

// PlacementEligible reports whether something outside Wardyn binds this backend's
// bytes (#113): a share, which can be reached from more than one place. A
// managed volume lives where Wardyn made it.
func PlacementEligible(b DriveBackend) bool {
	return EnforcementFor(b) == StorageEnforcementExternal
}

// SecondPlace is the drive as its second place mounts it: the same row, its
// backend and root the second place's. Only meaningful when AltRunnerID is set.
func (d UserDrive) SecondPlace() UserDrive {
	out := d
	out.Backend, out.HostRoot, out.StorageClass = d.AltBackend, d.AltHostRoot, ""
	return out
}

// ValidateDriveSecondPlace normalizes d's second place in place and checks it:
// nil when there is none, or when the first place is a share, the second a
// host_path on one runner at an absolute, cleaned path whose directories the
// drive's home template can name. ErrDriveSecondPlace for the wrong shape.
func ValidateDriveSecondPlace(d *UserDrive) error {
	d.AltHostRoot = strings.TrimSpace(d.AltHostRoot)
	if d.AltBackend == "" && d.AltHostRoot == "" && d.AltRunnerID == nil {
		return nil
	}
	if !PlacementEligible(d.Backend) || d.AltBackend != DriveBackendHostPath || d.AltRunnerID == nil ||
		*d.AltRunnerID == uuid.Nil || d.OwnerSubject != "" {
		return ErrDriveSecondPlace
	}
	root := d.AltHostRoot
	if root == "" || len(root) > maxUserDriveFieldLen || !driveTextIsClean(root) || !filepath.IsAbs(root) ||
		filepath.Clean(root) != root || root == "/" {
		return fmt.Errorf("alt_host_root: must be an absolute, cleaned path below /")
	}
	if ShareBackendRejectsTemplate(DriveBackendHostPath, d.HomeTemplate) {
		return fmt.Errorf("home_template %q cannot name a directory on the second place's share; pick %s or %s",
			d.HomeTemplate, HomeTemplateSub, HomeTemplateEmailLocal)
	}
	return nil
}
