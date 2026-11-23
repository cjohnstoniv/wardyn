// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

// A Windows image is registered only by digest: a tag, a bare name, an upper-case or short digest and a second
// digest are each refused, and an image also needs a name and a pool.
func TestWindowsImageRefMustBeDigestPinned(t *testing.T) {
	digest := strings.Repeat("ab", 32)
	for ref, ok := range map[string]bool{
		"registry.example/win/server@sha256:" + digest:                  true,
		"win@sha256:" + digest:                                          true,
		"registry.example/win/server:2025":                              false,
		"registry.example/win/server":                                   false,
		"registry.example/win/server:2025@sha256:" + digest[:62]:        false,
		"registry.example/win/server@sha256:" + strings.ToUpper(digest): false,
		"registry.example/win/server@sha512:" + digest + digest:         false,
		"a@b@sha256:" + digest:                                          false,
		" win@sha256:" + digest:                                         false,
		"":                                                              false,
	} {
		if err := ValidateWindowsImageRef(ref); (err == nil) != ok {
			t.Errorf("ref %q: err %v, want accepted %v", ref, err, ok)
		}
	}
	img := WindowsImage{Name: "Windows Server 2025", Ref: "win@sha256:" + digest, LicencePoolID: uuid.New()}
	if err := img.Validate(); err != nil {
		t.Fatalf("a valid image: %v", err)
	}
	for name, mut := range map[string]func(*WindowsImage){
		"no name":   func(i *WindowsImage) { i.Name = "" },
		"tag ref":   func(i *WindowsImage) { i.Ref = "win:latest" },
		"no pool":   func(i *WindowsImage) { i.LicencePoolID = uuid.Nil },
		"long name": func(i *WindowsImage) { i.Name = strings.Repeat("w", WindowsLicenceNameMax+1) },
	} {
		bad := img
		mut(&bad)
		if bad.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// A pool declares 1 to 10000 seats and a name.
func TestWindowsLicencePoolValidate(t *testing.T) {
	for seats, ok := range map[int]bool{0: false, 1: true, WindowsLicenceSeatsMax: true, WindowsLicenceSeatsMax + 1: false, -1: false} {
		if err := (WindowsLicencePool{Name: "Engineering", Seats: seats}).Validate(); (err == nil) != ok {
			t.Errorf("seats %d: err %v, want accepted %v", seats, err, ok)
		}
	}
	if (WindowsLicencePool{Name: " padded", Seats: 1}).Validate() == nil {
		t.Error("a padded name was accepted")
	}
	if got := NormalizeWindowsLicenceSubject("  Ana@Example.COM "); got != "ana@example.com" {
		t.Errorf("normalized subject %q", got)
	}
}
