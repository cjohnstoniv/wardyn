// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func stubDisk(t *testing.T, host types.RunnerAmounts, vmMiB int64, vmOK bool) *string {
	t.Helper()
	oldHost, oldVM := measureHost, engineVMDisk
	t.Cleanup(func() { measureHost, engineVMDisk = oldHost, oldVM })
	measureHost = func(context.Context) (types.RunnerAmounts, error) { return host, nil }
	asked := new(string)
	engineVMDisk = func(_ context.Context, image string) (int64, bool) { *asked = image; return vmMiB, vmOK }
	return asked
}

func TestMeasureWithVMDisk(t *testing.T) {
	t.Run("a disk the host measured is kept and nothing is probed", func(t *testing.T) {
		asked := stubDisk(t, types.RunnerAmounts{CPUMillis: 4000, DiskMiB: 5000}, 1, true)
		got, err := measureWithVMDisk(context.Background(), "img")
		if err != nil || got.DiskMiB != 5000 || *asked != "" {
			t.Fatalf("got %+v, %v, probed %q", got, err, *asked)
		}
	})
	t.Run("an unmeasured disk takes the VM probe, with the image given", func(t *testing.T) {
		asked := stubDisk(t, types.RunnerAmounts{CPUMillis: 4000}, 204800, true)
		got, err := measureWithVMDisk(context.Background(), "img")
		if err != nil || got.DiskMiB != 204800 || got.CPUMillis != 4000 || *asked != "img" {
			t.Fatalf("got %+v, %v, probed %q", got, err, *asked)
		}
	})
	t.Run("a probe that fails leaves the disk unmeasured, so --total-disk is asked for", func(t *testing.T) {
		stubDisk(t, types.RunnerAmounts{CPUMillis: 4000}, 0, false)
		got, err := measureWithVMDisk(context.Background(), "img")
		if err != nil || got.DiskMiB != 0 {
			t.Fatalf("got %+v, %v", got, err)
		}
		var f registerLimits
		if _, err := f.limits(newRegisterCommand(), got); err == nil {
			t.Fatal("limits accepted an unmeasured disk without --total-disk")
		}
	})
	t.Run("no image given probes with the release image", func(t *testing.T) {
		old := buildVersion
		buildVersion = func() string { return "1.2.3" }
		t.Cleanup(func() { buildVersion = old })
		asked := stubDisk(t, types.RunnerAmounts{}, 1024, true)
		if _, err := measureWithVMDisk(context.Background(), ""); err != nil || *asked != "ghcr.io/cjohnstoniv/wardyn-proxy:1.2.3" {
			t.Fatalf("probed %q, %v", *asked, err)
		}
	})
}

func TestSayNoDiskCap(t *testing.T) {
	old := engineDiskCapped
	t.Cleanup(func() { engineDiskCapped = old })
	for _, tc := range []struct {
		name       string
		capped, ok bool
		want       string
	}{
		{"no cap prints the one line", false, true, noDiskCapLine + "\n"},
		{"a cap prints nothing", true, true, ""},
		{"an engine that does not answer prints nothing", false, false, ""},
	} {
		engineDiskCapped = func(context.Context) (bool, bool) { return tc.capped, tc.ok }
		var out bytes.Buffer
		sayNoDiskCap(context.Background(), &out)
		if out.String() != tc.want {
			t.Errorf("%s: printed %q, want %q", tc.name, out.String(), tc.want)
		}
	}
}
