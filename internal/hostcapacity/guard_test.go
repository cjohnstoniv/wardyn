// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package hostcapacity

import (
	"errors"
	"runtime"
	"testing"
)

func TestHostCapacityAdmit(t *testing.T) {
	fixed := func(avail int, load1 float64, err error) func() (int, float64, error) {
		return func() (int, float64, error) { return avail, load1, err }
	}
	both := Limits{MinAvailableMiB: 8192, MaxLoad1: 100}
	for _, tc := range []struct {
		name   string
		limits Limits
		read   func() (int, float64, error)
		reason string // "" = admitted
	}{
		{"disabled reads nothing", Limits{}, func() (int, float64, error) { panic("read while disabled") }, ""},
		{"within limits", both, fixed(9000, 99.9, nil), ""},
		{"at the limits", both, fixed(8192, 100, nil), ""},
		{"under floor", both, fixed(4096, 1, nil), "mem_available_mib=4096 < 8192"},
		{"over ceiling", both, fixed(9000, 112.4, nil), "load1=112.40 > 100"},
		{"both", both, fixed(4096, 112.4, nil), "mem_available_mib=4096 < 8192, load1=112.40 > 100"},
		{"floor only ignores load", Limits{MinAvailableMiB: 8192}, fixed(9000, 500, nil), ""},
		{"ceiling only ignores memory", Limits{MaxLoad1: 100}, fixed(1, 50, nil), ""},
		{"read error admits", both, fixed(0, 0, errors.New("no /proc")), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := New(tc.limits, tc.read).Admit()
			if tc.reason == "" {
				if err != nil {
					t.Fatalf("Admit() = %v, want nil", err)
				}
				return
			}
			var refused ErrRefused
			if !errors.As(err, &refused) || refused.Reason != tc.reason {
				t.Fatalf("Admit() = %v, want ErrRefused{%q}", err, tc.reason)
			}
		})
	}
}

func TestHostCapacityNilGuardAdmits(t *testing.T) {
	var g *Guard
	if err := g.Admit(); err != nil {
		t.Fatalf("nil guard Admit() = %v, want nil", err)
	}
}

func TestHostCapacityReadProc(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("/proc is Linux-only")
	}
	avail, load1, err := ReadProc()
	if err != nil || avail <= 0 || load1 < 0 {
		t.Fatalf("ReadProc() = %d, %v, %v; want a positive MemAvailable and no error", avail, load1, err)
	}
}
