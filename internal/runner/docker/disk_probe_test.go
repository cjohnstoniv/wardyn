// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/system"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// diskProbeContainer returns the one disk-probe container the fake saw.
func diskProbeContainer(t *testing.T, f *fakeDocker) *createdContainer {
	t.Helper()
	var found *createdContainer
	for name, c := range f.containers {
		if strings.HasPrefix(name, diskProbeNamePrefix) {
			if found != nil {
				t.Fatal("more than one disk probe container")
			}
			found = c
		}
	}
	if found == nil {
		t.Fatal("no disk probe container was created")
	}
	return found
}

func TestProbeDiskFreeMiBRunsOneHardenedThrowawayContainer(t *testing.T) {
	f := newFakeDocker()
	f.diskProbeOut = "204800\n"
	f.images["wardyn-proxy:dev"] = true
	d := newWithClient(f, Config{ProxyImage: "wardyn-proxy:dev", RunnerID: "r-1"})
	got, err := d.ProbeDiskFreeMiB(context.Background())
	if err != nil || got != 204800 {
		t.Fatalf("ProbeDiskFreeMiB = %d, %v", got, err)
	}
	c := diskProbeContainer(t, f)
	if !slices.Equal(c.cfg.Entrypoint, []string{"/usr/local/bin/wardyn-proxy"}) || !slices.Equal(c.cfg.Cmd, []string{"-disk-free", "/"}) {
		t.Errorf("entrypoint %v cmd %v", c.cfg.Entrypoint, c.cfg.Cmd)
	}
	h := c.host
	if string(h.NetworkMode) != "none" || !slices.Equal(h.CapDrop, []string{"ALL"}) || !slices.Contains(h.SecurityOpt, "no-new-privileges") ||
		!h.ReadonlyRootfs || h.AutoRemove || h.Memory == 0 || h.PidsLimit == nil || len(h.Mounts)+len(h.Binds) != 0 {
		t.Errorf("hardening wrong: %+v", h)
	}
	if c.cfg.Labels[labelManaged] != "true" || c.cfg.Labels[labelComponent] != componentDiskProbe || c.cfg.Labels[runner.LabelRunnerID] != "r-1" {
		t.Errorf("labels %v", c.cfg.Labels)
	}
	if !c.removed || !c.forceRemoved {
		t.Errorf("probe container removed=%v force=%v", c.removed, c.forceRemoved)
	}
}

func TestProbeDiskFreeMiBFailures(t *testing.T) {
	for _, tc := range []struct {
		name    string
		code    int64
		out     string
		pulled  bool
		wantErr string
	}{
		{name: "image absent", pulled: false, wantErr: "is not present locally"},
		{name: "image without the flag", pulled: true, code: 2, wantErr: "did not report its free disk (exit 2)"},
		{name: "unreadable output", pulled: true, out: "lots\n", wantErr: "unreadable result"},
		{name: "zero is no answer", pulled: true, out: "0\n", wantErr: "unreadable result"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeDocker()
			f.diskProbeExit, f.diskProbeOut = tc.code, tc.out
			f.images["wardyn-proxy:dev"] = tc.pulled
			got, err := newTestDriver(f).ProbeDiskFreeMiB(context.Background())
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) || got != 0 {
				t.Fatalf("got %d, %v; want error containing %q", got, err, tc.wantErr)
			}
			if tc.pulled {
				if c := diskProbeContainer(t, f); !c.removed || !c.forceRemoved {
					t.Errorf("probe container removed=%v force=%v", c.removed, c.forceRemoved)
				}
			}
		})
	}
}

func TestProbeDiskFreeMiBBoundedByContext(t *testing.T) {
	f := newFakeDocker()
	f.waitHangs = true
	f.images["wardyn-proxy:dev"] = true
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := newTestDriver(f).ProbeDiskFreeMiB(ctx); err == nil {
		t.Fatal("a probe with no answer must fail")
	}
	if c := diskProbeContainer(t, f); !c.removed || !c.forceRemoved {
		t.Error("probe container not force-removed")
	}
}

// The engine is VM-backed when it says so, by what it calls itself or the socket it is reached by,
// or when it is not a unix socket at all.
func TestVMBacked(t *testing.T) {
	for _, tc := range []struct {
		name string
		info system.Info
		host string
		want bool
	}{
		{"plain linux", system.Info{Name: "buildbox", OperatingSystem: "Ubuntu 24.04"}, "", false},
		{"explicit unix socket", system.Info{Name: "buildbox"}, "unix:///var/run/docker.sock", false},
		{"Docker Desktop", system.Info{OperatingSystem: "Docker Desktop"}, "", true},
		{"Colima by name", system.Info{Name: "colima"}, "", true},
		{"Colima profile", system.Info{Name: "colima-work"}, "", true},
		{"Rancher Desktop by name", system.Info{Name: "lima-rancher-desktop", OperatingSystem: "Alpine Linux v3.19"}, "", true},
		{"Lima instance", system.Info{Name: "lima-default"}, "", true},
		{"Colima socket", system.Info{Name: "x"}, "unix:///home/u/.colima/default/docker.sock", true},
		{"Rancher socket", system.Info{Name: "x"}, "unix:///Users/u/.rd/docker.sock", true},
		{"Lima socket", system.Info{Name: "x"}, "unix:///Users/u/.lima/docker/sock/docker.sock", true},
		{"Docker Desktop socket", system.Info{Name: "x"}, "unix:///Users/u/.docker/run/docker.sock", true},
		{"tcp host", system.Info{Name: "buildbox"}, "tcp://10.0.0.1:2376", true},
		{"ssh host", system.Info{Name: "buildbox"}, "ssh://me@box", true},
	} {
		if got := VMBacked(tc.info, tc.host); got != tc.want {
			t.Errorf("%s: VMBacked = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A VM-backed Driver.Measure reads the disk from the probe; a probe that cannot run leaves it unknown.
func TestMeasureReadsTheVMDiskFromTheProbe(t *testing.T) {
	for _, tc := range []struct {
		name string
		exit int64
		out  string
		want int64
	}{
		{"probe answers", 0, "102400\n", 102400},
		{"probe fails", 2, "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeDocker()
			f.info.OperatingSystem, f.info.DockerRootDir, f.info.NCPU, f.info.MemTotal = "Docker Desktop", "/var/lib/docker", 4, 8<<30
			f.images["wardyn-proxy:dev"] = true
			f.diskProbeExit, f.diskProbeOut = tc.exit, tc.out
			got, err := newTestDriver(f).Measure(context.Background())
			if err != nil || got.DiskMiB != tc.want || got.CPUMillis != 4000 {
				t.Fatalf("Measure = %+v, %v; want disk %d", got, err, tc.want)
			}
		})
	}
}
