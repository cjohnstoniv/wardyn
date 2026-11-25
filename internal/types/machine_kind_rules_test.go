// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"reflect"
	"strings"
	"testing"
)

func TestMachineKindDenial_DenyOnlyShape(t *testing.T) {
	typ := reflect.TypeFor[MachineKindDenial]()
	var got []string
	for i := range typ.NumField() {
		got = append(got, typ.Field(i).Name)
	}
	if want := "Trust MachineKind"; strings.Join(got, " ") != want {
		t.Fatalf("MachineKindDenial fields = %v, want %q: an entry names a class and a kind and nothing that could admit", got, want)
	}
}

func TestValidateMachineKindDenials(t *testing.T) {
	d := func(c RunnerTrustClass, k MachineKind) MachineKindDenial {
		return MachineKindDenial{Trust: c, MachineKind: k}
	}
	var full []MachineKindDenial
	for _, c := range []RunnerTrustClass{RunnerTrustPersonal, RunnerTrustManagedDevice, RunnerTrustOrgCluster} {
		for _, k := range []MachineKind{MachineContainer, MachineLinuxVM, MachineMacOSVM} {
			full = append(full, d(c, k))
		}
	}
	for _, tc := range []struct {
		name string
		in   []MachineKindDenial
		want string
	}{
		{"empty", nil, ""},
		{"all nine cells", full, ""},
		{"unknown class", []MachineKindDenial{d("laptop", MachineLinuxVM)}, "trust_class"},
		{"empty class", []MachineKindDenial{d("", MachineLinuxVM)}, "trust_class"},
		{"unknown kind", []MachineKindDenial{d(RunnerTrustPersonal, "windows_vm")}, "machine_kind"},
		{"empty kind", []MachineKindDenial{d(RunnerTrustPersonal, "")}, "machine_kind"},
		{"duplicate", []MachineKindDenial{d(RunnerTrustPersonal, MachineLinuxVM), d(RunnerTrustPersonal, MachineLinuxVM)}, "twice"},
		{"container twice", []MachineKindDenial{d(RunnerTrustPersonal, MachineContainer), d(RunnerTrustPersonal, MachineContainer)}, "twice"},
		{"same kind on two classes", []MachineKindDenial{d(RunnerTrustPersonal, MachineLinuxVM), d(RunnerTrustOrgCluster, MachineLinuxVM)}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateMachineKindDenials(tc.in)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one naming %q", err, tc.want)
			}
		})
	}
}

func TestDeniesMachineKind(t *testing.T) {
	ds := []MachineKindDenial{{Trust: RunnerTrustPersonal, MachineKind: MachineContainer}, {Trust: RunnerTrustOrgCluster, MachineKind: MachineLinuxVM}}
	for _, tc := range []struct {
		trust RunnerTrustClass
		kind  MachineKind
		want  bool
	}{
		{RunnerTrustPersonal, "", true},
		{RunnerTrustPersonal, MachineContainer, true},
		{RunnerTrustPersonal, MachineLinuxVM, false},
		{RunnerTrustManagedDevice, "", false},
		{RunnerTrustOrgCluster, MachineLinuxVM, true},
		{RunnerTrustPersonal, MachineMacOSVM, false},
		{"", MachineLinuxVM, false},
	} {
		if got := DeniesMachineKind(ds, tc.trust, tc.kind); got != tc.want {
			t.Errorf("DeniesMachineKind(%q, %q) = %v, want %v", tc.trust, tc.kind, got, tc.want)
		}
	}
	if DeniesMachineKind(nil, RunnerTrustPersonal, MachineLinuxVM) {
		t.Error("an empty list denied a kind")
	}
}
