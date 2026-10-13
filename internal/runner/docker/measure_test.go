// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"testing"

	"github.com/moby/moby/api/types/system"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// What a Docker engine has: its CPUs and memory always, and its data root's free disk only where the
// path is on this host (a local engine that is not Docker Desktop's VM); anything else is 0, unknown.
func TestMeasureInfo(t *testing.T) {
	info := system.Info{NCPU: 8, MemTotal: 16 << 30, DockerRootDir: "/var/lib/docker"}
	free := func(string) int64 { return 500 * 1024 }
	if got, want := MeasureInfo(info, true, free), (types.RunnerAmounts{CPUMillis: 8000, MemoryMiB: 16384, DiskMiB: 500 * 1024}); got != want {
		t.Fatalf("local engine = %+v, want %+v", got, want)
	}
	if got := MeasureInfo(info, false, free); got.DiskMiB != 0 || got.CPUMillis != 8000 {
		t.Fatalf("remote engine = %+v, want no disk", got)
	}
	info.OperatingSystem = "Docker Desktop"
	if got := MeasureInfo(info, true, free); got.DiskMiB != 0 {
		t.Fatalf("Docker Desktop = %+v, want no disk", got)
	}
	if !localSocket("") || !localSocket("unix:///var/run/docker.sock") || localSocket("tcp://10.0.0.1:2376") {
		t.Fatal("localSocket")
	}
}
