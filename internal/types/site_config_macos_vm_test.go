// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"encoding/json"
	"testing"
)

// macos_vm.max_base_age_days round-trips, and a document without the block decodes to nil and encodes without
// it, so a stored config from before it is unchanged.
func TestSiteConfigMacOSVMMaxBaseAgeDays(t *testing.T) {
	var sc SiteConfig
	if err := json.Unmarshal([]byte(`{"macos_vm":{"max_base_age_days":45}}`), &sc); err != nil || sc.MacOSVM == nil || sc.MacOSVM.MaxBaseAgeDays != 45 {
		t.Fatalf("decoded %+v, %v", sc.MacOSVM, err)
	}
	raw, err := json.Marshal(SiteConfig{})
	if err != nil || string(raw) != "{}" {
		t.Fatalf("an empty config encodes %s, %v", raw, err)
	}
}

// macos_vm.allow_background_watch: absent reads allowed, and an explicit false survives the round trip.
func TestSiteMacOSVMBackgroundWatchAllowed(t *testing.T) {
	var absent *SiteMacOSVM
	if !absent.BackgroundWatchAllowed() || !(&SiteMacOSVM{MaxBaseAgeDays: 7}).BackgroundWatchAllowed() {
		t.Fatal("an absent block or field must read allowed")
	}
	var sc SiteConfig
	if err := json.Unmarshal([]byte(`{"macos_vm":{"allow_background_watch":false}}`), &sc); err != nil || sc.MacOSVM.BackgroundWatchAllowed() {
		t.Fatalf("decoded %+v, %v: false must read off", sc.MacOSVM, err)
	}
	raw, err := json.Marshal(sc)
	if err != nil || string(raw) != `{"macos_vm":{"allow_background_watch":false}}` {
		t.Fatalf("encoded %s, %v: false must be kept", raw, err)
	}
	on := true
	if !(&SiteMacOSVM{AllowBackgroundWatch: &on}).BackgroundWatchAllowed() {
		t.Fatal("true must read allowed")
	}
}

// macos_vm.allow_direct_drive_mount: absent or a nil block reads off, and an explicit false survives the round trip.
func TestSiteMacOSVMDirectDriveMountAllowed(t *testing.T) {
	var absent *SiteMacOSVM
	if absent.DirectDriveMountAllowed() || (&SiteMacOSVM{MaxBaseAgeDays: 7}).DirectDriveMountAllowed() {
		t.Fatal("no direct mount switch stored must read off")
	}
	var sc SiteConfig
	if err := json.Unmarshal([]byte(`{"macos_vm":{"allow_direct_drive_mount":true}}`), &sc); err != nil || !sc.MacOSVM.DirectDriveMountAllowed() {
		t.Fatalf("decoded %+v, %v, want allowed", sc.MacOSVM, err)
	}
	off := false
	raw, err := json.Marshal(SiteConfig{MacOSVM: &SiteMacOSVM{AllowDirectDriveMount: &off}})
	if err != nil || string(raw) != `{"macos_vm":{"allow_direct_drive_mount":false}}` {
		t.Fatalf("encoded %s, %v, want the explicit false kept", raw, err)
	}
}
