// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestMachineKindDenial_DenyOnlyShape(t *testing.T) {
	typ := reflect.TypeFor[MachineKindDenial]()
	var got []string
	for i := range typ.NumField() {
		got = append(got, typ.Field(i).Name)
	}
	if want := "Trust MachineKind SiteID"; strings.Join(got, " ") != want {
		t.Fatalf("MachineKindDenial fields = %v, want %q: an entry names a class, a kind and where it holds, and nothing that could admit", got, want)
	}
}

func TestValidateMachineKindDenials(t *testing.T) {
	d := func(c RunnerTrustClass, k MachineKind) MachineKindDenial {
		return MachineKindDenial{Trust: c, MachineKind: k}
	}
	at := func(site string, c RunnerTrustClass, k MachineKind) MachineKindDenial {
		return MachineKindDenial{Trust: c, MachineKind: k, SiteID: site}
	}
	siteA, siteB := uuid.NewString(), uuid.NewString()
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
		{"same cell everywhere and at one site", []MachineKindDenial{d(RunnerTrustPersonal, MachineLinuxVM), at(siteA, RunnerTrustPersonal, MachineLinuxVM)}, ""},
		{"same cell at two sites", []MachineKindDenial{at(siteA, RunnerTrustPersonal, MachineLinuxVM), at(siteB, RunnerTrustPersonal, MachineLinuxVM)}, ""},
		{"same cell at one site twice", []MachineKindDenial{at(siteA, RunnerTrustPersonal, MachineLinuxVM), at(siteA, RunnerTrustPersonal, MachineLinuxVM)}, "twice"},
		{"site id not a uuid", []MachineKindDenial{at("office", RunnerTrustPersonal, MachineLinuxVM)}, "site_id"},
		{"site id upper case", []MachineKindDenial{at(strings.ToUpper(siteA), RunnerTrustPersonal, MachineLinuxVM)}, "site_id"},
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

// A denial with no site holds at every site and with no site; one keyed to a site holds only for a runner
// whose pool is at that site, never at another site and never where the run has no site.
func TestMachineKindDenialsAt(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	everywhere := MachineKindDenial{Trust: RunnerTrustPersonal, MachineKind: MachineLinuxVM}
	atA := MachineKindDenial{Trust: RunnerTrustOrgCluster, MachineKind: MachineContainer, SiteID: a.String()}
	ds := []MachineKindDenial{everywhere, atA}
	for _, tc := range []struct {
		name string
		site *uuid.UUID
		want []MachineKindDenial
	}{
		{"no site", nil, []MachineKindDenial{everywhere}},
		{"the keyed site", &a, []MachineKindDenial{everywhere, atA}},
		{"another site", &b, []MachineKindDenial{everywhere}},
	} {
		if got := MachineKindDenialsAt(ds, tc.site); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: MachineKindDenialsAt = %v, want %v", tc.name, got, tc.want)
		}
	}
	if got := MachineKindDenialsAt(nil, &a); len(got) != 0 {
		t.Errorf("an empty list held %v", got)
	}
}
