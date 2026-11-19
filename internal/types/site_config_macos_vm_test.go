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

// macos_vm.allow_attended_window: absent reads off, only an explicit true reads on, and an explicit false
// survives the round trip.
func TestSiteMacOSVMAttendedWindowAllowed(t *testing.T) {
	var absent *SiteMacOSVM
	if absent.AttendedWindowAllowed() || (&SiteMacOSVM{MaxBaseAgeDays: 7}).AttendedWindowAllowed() {
		t.Fatal("an absent block or field must read off")
	}
	var sc SiteConfig
	if err := json.Unmarshal([]byte(`{"macos_vm":{"allow_attended_window":false}}`), &sc); err != nil || sc.MacOSVM.AttendedWindowAllowed() {
		t.Fatalf("decoded %+v, %v: false must read off", sc.MacOSVM, err)
	}
	raw, err := json.Marshal(sc)
	if err != nil || string(raw) != `{"macos_vm":{"allow_attended_window":false}}` {
		t.Fatalf("encoded %s, %v: false must be kept", raw, err)
	}
	if err := json.Unmarshal([]byte(`{"macos_vm":{"allow_attended_window":true}}`), &sc); err != nil || !sc.MacOSVM.AttendedWindowAllowed() {
		t.Fatalf("decoded %+v, %v: true must read on", sc.MacOSVM, err)
	}
	on := true
	if !(&SiteMacOSVM{AllowAttendedWindow: &on, AllowBackgroundWatch: new(bool)}).AttendedWindowAllowed() {
		t.Fatal("the window switch is independent of the watch's")
	}
}

// macos_vm.allow_kept_machines: absent reads off, only an explicit true reads on, and an explicit false survives
// the round trip.
func TestSiteMacOSVMKeptMachinesAllowed(t *testing.T) {
	var absent *SiteMacOSVM
	if absent.KeptMachinesAllowed() || (&SiteMacOSVM{MaxBaseAgeDays: 7}).KeptMachinesAllowed() {
		t.Fatal("an absent block or field must read off")
	}
	var sc SiteConfig
	if err := json.Unmarshal([]byte(`{"macos_vm":{"allow_kept_machines":false}}`), &sc); err != nil || sc.MacOSVM.KeptMachinesAllowed() {
		t.Fatalf("decoded %+v, %v: false must read off", sc.MacOSVM, err)
	}
	raw, err := json.Marshal(sc)
	if err != nil || string(raw) != `{"macos_vm":{"allow_kept_machines":false}}` {
		t.Fatalf("encoded %s, %v: false must be kept", raw, err)
	}
	if err := json.Unmarshal([]byte(`{"macos_vm":{"allow_kept_machines":true}}`), &sc); err != nil || !sc.MacOSVM.KeptMachinesAllowed() {
		t.Fatalf("decoded %+v, %v: true must read on", sc.MacOSVM, err)
	}
	on := true
	if (&SiteMacOSVM{AllowAttendedWindow: &on, AllowBackgroundWatch: &on}).KeptMachinesAllowed() {
		t.Fatal("the keep switch is independent of the window's and the watch's")
	}
}

// macos_vm.kept_machine_idle_days and macos_vm.max_kept_machines_per_person: a nil block or a 0 reads the
// default (30 days, 1 machine), and a stored value reads as stored.
func TestSiteMacOSVMKeptMachineLimits(t *testing.T) {
	var absent *SiteMacOSVM
	if got := absent.KeptMachineIdleDaysOrDefault(); got != 30 {
		t.Errorf("idle days on a nil block = %d, want 30", got)
	}
	if got := absent.MaxKeptMachinesPerPersonOrDefault(); got != 1 {
		t.Errorf("per-person cap on a nil block = %d, want 1", got)
	}
	empty := &SiteMacOSVM{MaxBaseAgeDays: 7}
	if empty.KeptMachineIdleDaysOrDefault() != 30 || empty.MaxKeptMachinesPerPersonOrDefault() != 1 {
		t.Errorf("an absent field must read the default: %d, %d", empty.KeptMachineIdleDaysOrDefault(), empty.MaxKeptMachinesPerPersonOrDefault())
	}
	var sc SiteConfig
	if err := json.Unmarshal([]byte(`{"macos_vm":{"kept_machine_idle_days":90,"max_kept_machines_per_person":4}}`), &sc); err != nil {
		t.Fatal(err)
	}
	if sc.MacOSVM.KeptMachineIdleDaysOrDefault() != 90 || sc.MacOSVM.MaxKeptMachinesPerPersonOrDefault() != 4 {
		t.Errorf("decoded %+v: stored limits must read as stored", sc.MacOSVM)
	}
	raw, err := json.Marshal(sc)
	if err != nil || string(raw) != `{"macos_vm":{"kept_machine_idle_days":90,"max_kept_machines_per_person":4}}` {
		t.Fatalf("encoded %s, %v", raw, err)
	}
	if KeptMachineIdleDaysMax != 90 || MaxKeptMachinesPerPersonMax != 4 {
		t.Errorf("bounds = %d, %d, want 90 and 4", KeptMachineIdleDaysMax, MaxKeptMachinesPerPersonMax)
	}
}
