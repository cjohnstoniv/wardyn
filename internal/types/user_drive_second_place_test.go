// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"errors"
	"testing"

	"github.com/google/uuid"
)

// TestValidateDriveSecondPlace: a second place is a host_path on one runner,
// beside a share; anything else is refused, and a drive with none passes.
func TestValidateDriveSecondPlace(t *testing.T) {
	runner := uuid.New()
	share := UserDrive{Name: "team-share", Backend: DriveBackendHostPath, HostRoot: "/srv/shares/team", HomeTemplate: HomeTemplateEmailLocal,
		AltBackend: DriveBackendHostPath, AltHostRoot: " /Volumes/team-share ", AltRunnerID: &runner}
	if err := ValidateDriveSecondPlace(&share); err != nil || share.AltHostRoot != "/Volumes/team-share" {
		t.Fatalf("a share bound to a runner: %v (path %q)", err, share.AltHostRoot)
	}
	none := UserDrive{Name: "scratch", Backend: DriveBackendDockerVolume}
	if err := ValidateDriveSecondPlace(&none); err != nil {
		t.Fatalf("a drive with one place: %v", err)
	}
	static := share
	static.Backend, static.HostRoot, static.HomeTemplate = DriveBackendK8sPVCStatic, "", HomeTemplateSub
	if err := ValidateDriveSecondPlace(&static); err != nil {
		t.Fatalf("a platform share bound to a runner: %v", err)
	}
	for name, c := range map[string]struct {
		edit  func(*UserDrive)
		shape bool // refused as the wrong shape, else as a bad path or template
	}{
		"a managed first place":  {func(d *UserDrive) { d.Backend, d.HostRoot = DriveBackendDockerVolume, "" }, true},
		"a managed second place": {func(d *UserDrive) { d.AltBackend = DriveBackendDockerVolume }, true},
		"no runner":              {func(d *UserDrive) { d.AltRunnerID = nil }, true},
		"the nil runner":         {func(d *UserDrive) { id := uuid.Nil; d.AltRunnerID = &id }, true},
		"a member's own drive":   {func(d *UserDrive) { d.OwnerSubject = "carol" }, true},
		"a relative path":        {func(d *UserDrive) { d.AltHostRoot = "Volumes/team" }, false},
		"an uncleaned path":      {func(d *UserDrive) { d.AltHostRoot = "/Volumes/../etc" }, false},
		"the root":               {func(d *UserDrive) { d.AltHostRoot = "/" }, false},
		"no path":                {func(d *UserDrive) { d.AltHostRoot = "" }, false},
		"a hash home":            {func(d *UserDrive) { d.HomeTemplate = HomeTemplateHash }, false},
	} {
		d := share
		c.edit(&d)
		err := ValidateDriveSecondPlace(&d)
		if err == nil || errors.Is(err, ErrDriveSecondPlace) != c.shape {
			t.Errorf("%s: err = %v, want refused (shape %v)", name, err, c.shape)
		}
	}
}

// TestPlacementEligible: only a share can be reached from more than one place.
func TestPlacementEligible(t *testing.T) {
	for b, want := range map[DriveBackend]bool{DriveBackendHostPath: true, DriveBackendK8sPVCStatic: true, DriveBackendDockerVolume: false, DriveBackendK8sPVC: false} {
		if PlacementEligible(b) != want {
			t.Errorf("PlacementEligible(%s) = %v, want %v", b, !want, want)
		}
	}
}
