// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package main

import (
	"context"
	"errors"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner/docker"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

type fakeWinDriver struct {
	probeErr error
	probes   int
	enabled  *docker.WinVMConfig
}

func (f *fakeWinDriver) ProbeWindowsVM(context.Context) (docker.WinVMConfig, error) {
	f.probes++
	return docker.WinVMConfig{SWTPMVersion: "0.10.0"}, f.probeErr
}

func (f *fakeWinDriver) EnableWindowsVM(c *docker.WinVMConfig) error {
	f.enabled = c
	return nil
}

var testLinuxCaps = &runnerwire.VMCaps{MachineType: "pc-q35-10.0", CPUModel: "Haswell-v4", QEMUVersion: "10.0.13", Virtiofs: true, Desktop: true, SignIns: true, Drives: true}

// Windows VM sandboxes are off unless the operator turns them on, and need Linux VM sandboxes on: in
// either case no probe runs and the caps offer none.
func TestWindowsVMOffByDefault(t *testing.T) {
	for _, v := range []string{"", "0", "true", "yes"} {
		f := &fakeWinDriver{}
		env := func(k string) string {
			if k == "WARDYN_RUNNER_WINDOWS_VM" {
				return v
			}
			return ""
		}
		if caps := enableWindowsVM(context.Background(), f, testLinuxCaps, env); caps != nil || f.probes != 0 {
			t.Fatalf("WARDYN_RUNNER_WINDOWS_VM=%q offered %+v after %d probes", v, caps, f.probes)
		}
	}
	f := &fakeWinDriver{}
	on := func(k string) string { return map[string]string{"WARDYN_RUNNER_WINDOWS_VM": "1"}[k] }
	if caps := enableWindowsVM(context.Background(), f, nil, on); caps != nil || f.probes != 0 || f.enabled != nil {
		t.Fatalf("with Linux VMs off: caps %+v, %d probes, enabled %v", caps, f.probes, f.enabled)
	}
}

// A passing probe turns Windows VMs on and offers the desktop alone: no sign-ins, no folders, no
// drives. A failing probe offers nothing and turns nothing on.
func TestWindowsVMProbeDecidesTheCaps(t *testing.T) {
	on := func(k string) string { return map[string]string{"WARDYN_RUNNER_WINDOWS_VM": "1"}[k] }
	f := &fakeWinDriver{}
	caps := enableWindowsVM(context.Background(), f, testLinuxCaps, on)
	if caps == nil || f.enabled == nil || f.enabled.SWTPMVersion != "0.10.0" {
		t.Fatalf("a passing probe: caps %+v, enabled %+v", caps, f.enabled)
	}
	if *caps != (runnerwire.VMCaps{MachineType: "pc-q35-10.0", CPUModel: "Haswell-v4", QEMUVersion: "10.0.13", Desktop: true}) {
		t.Fatalf("Windows caps %+v: the Linux machine and the desktop, nothing else", caps)
	}
	f = &fakeWinDriver{probeErr: errors.New("no UEFI firmware")}
	if caps := enableWindowsVM(context.Background(), f, testLinuxCaps, on); caps != nil || f.enabled != nil {
		t.Fatalf("a failed probe: caps %+v, enabled %+v", caps, f.enabled)
	}
}
