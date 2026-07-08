// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/mount"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// helper: run CreateSandbox with the given mounts and return the agent's applied
// bind mounts (excluding the recording mount, which is off in these tests) plus
// any error.
func createWithMounts(t *testing.T, mounts []runner.Mount) ([]mount.Mount, error) {
	t.Helper()
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	d := newTestDriver(f)

	spec := testSpec()
	spec.Mounts = mounts
	sb, err := d.CreateSandbox(context.Background(), spec)
	if err != nil {
		return nil, err
	}
	agent := f.containers[agentContainerName(spec.RunID)]
	if agent == nil {
		t.Fatalf("agent container not created (ref=%s)", sb.Ref)
	}
	return agent.host.Mounts, nil
}

// TestCreateSandbox_AllowedMountApplied: an allowed (absolute, non-dangerous
// source; allowed target prefix) mount is applied as a bind mount with the
// requested read-only flag.
func TestCreateSandbox_AllowedMountApplied(t *testing.T) {
	got, err := createWithMounts(t, []runner.Mount{
		{Source: "/home/maintainer/repo", Target: "/home/agent/work", ReadOnly: false},
	})
	if err != nil {
		t.Fatalf("CreateSandbox with allowed mount failed: %v", err)
	}
	var found *mount.Mount
	for i := range got {
		if got[i].Target == "/home/agent/work" {
			found = &got[i]
		}
	}
	if found == nil {
		t.Fatalf("allowed mount not applied; agent mounts = %+v", got)
	}
	if found.Type != mount.TypeBind {
		t.Errorf("mount Type = %q, want bind", found.Type)
	}
	if found.Source != "/home/maintainer/repo" {
		t.Errorf("mount Source = %q", found.Source)
	}
	if found.ReadOnly {
		t.Errorf("mount ReadOnly = true, want false (policy opted into RW)")
	}
}

// TestCreateSandbox_MountDefaultReadOnly: when the spec's mount ReadOnly is true
// (the resolved default for an omitted policy field), the bind is read-only.
func TestCreateSandbox_MountDefaultReadOnly(t *testing.T) {
	got, err := createWithMounts(t, []runner.Mount{
		{Source: "/srv/data", Target: "/work/data", ReadOnly: true},
	})
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	for _, m := range got {
		if m.Target == "/work/data" {
			if !m.ReadOnly {
				t.Errorf("default mount should be ReadOnly=true, got false")
			}
			return
		}
	}
	t.Fatalf("mount /work/data not applied; got %+v", got)
}

// ITEM 34(a): the recording-mount TARGET must never be chmod 0777'd for a
// host-bind RecordingMount (that would make the operator's HOST directory
// world-writable). CastDir is always prepared; a named-volume target still is.
func TestRecordingChmodDirs_NeverLoosensHostBindRoot(t *testing.T) {
	const cast = "/var/log/wardyn"

	hostBind := recordingChmodDirs(Config{CastDir: cast, RecordingMount: "/host/recordings"})
	if containsStr(hostBind, RecordingMountTarget) {
		t.Errorf("host-bind RecordingMount: target %q must NOT be chmod 0777'd (host world-writable); got %v", RecordingMountTarget, hostBind)
	}
	if !containsStr(hostBind, cast) {
		t.Errorf("CastDir must always be prepared; got %v", hostBind)
	}

	vol := recordingChmodDirs(Config{CastDir: cast, RecordingMount: "wardyn-rec-vol"})
	if !containsStr(vol, RecordingMountTarget) {
		t.Errorf("named-volume RecordingMount: target must be prepared (Docker-managed); got %v", vol)
	}

	none := recordingChmodDirs(Config{CastDir: cast})
	if len(none) != 1 || none[0] != cast {
		t.Errorf("no RecordingMount => only CastDir; got %v", none)
	}
}

// ITEM 34(b): a host-path RecordingMount source is run through the same deny-list
// as workspace binds and FAILS CLOSED; a named volume is not a host path and is
// not deny-listed; a valid host path is applied as a bind.
func TestCreateSandbox_RecordingMountHostBindValidated(t *testing.T) {
	for _, src := range []string{"/", "/etc", "/proc", "/var/run/docker.sock", "/var/lib/docker"} {
		f := newFakeDocker()
		f.images["busybox:latest"] = true
		d := newWithClient(f, Config{ProxyImage: "wardyn-proxy:dev", RecordingMount: src})
		if _, err := d.CreateSandbox(context.Background(), testSpec()); err == nil {
			t.Errorf("denied RecordingMount source %q must FAIL CLOSED, got nil", src)
		} else if !strings.Contains(err.Error(), "denied recording mount") {
			t.Errorf("RecordingMount %q: error should identify the denied recording mount, got %v", src, err)
		}
	}

	// Valid host-path RecordingMount: applied as a bind at RecordingMountTarget.
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	d := newWithClient(f, Config{ProxyImage: "wardyn-proxy:dev", RecordingMount: "/srv/wardyn/recordings"})
	if _, err := d.CreateSandbox(context.Background(), testSpec()); err != nil {
		t.Fatalf("valid host-bind RecordingMount must succeed, got %v", err)
	}
	agent := f.containers[agentContainerName(testSpec().RunID)]
	var found bool
	for _, m := range agent.host.Mounts {
		if m.Target == RecordingMountTarget && m.Type == mount.TypeBind && m.Source == "/srv/wardyn/recordings" {
			found = true
		}
	}
	if !found {
		t.Errorf("valid host-bind RecordingMount not applied as bind; mounts=%+v", agent.host.Mounts)
	}

	// Named-volume RecordingMount: not a host path -> not deny-listed, succeeds.
	f2 := newFakeDocker()
	f2.images["busybox:latest"] = true
	d2 := newWithClient(f2, Config{ProxyImage: "wardyn-proxy:dev", RecordingMount: "wardyn-rec-vol"})
	if _, err := d2.CreateSandbox(context.Background(), testSpec()); err != nil {
		t.Fatalf("named-volume RecordingMount must not be deny-listed, got %v", err)
	}
}

// TestCreateSandbox_DeniedMountsRejected runs the full deny-list matrix: each
// dangerous Source (and a bad Target) must FAIL the CreateSandbox closed (the
// driver defense-in-depth re-check), so a bad mount can never reach Docker even
// if it somehow got past policy validation.
func TestCreateSandbox_DeniedMountsRejected(t *testing.T) {
	cases := []struct {
		name string
		m    runner.Mount
	}{
		{"root", runner.Mount{Source: "/", Target: "/home/agent/x"}},
		{"proc", runner.Mount{Source: "/proc", Target: "/home/agent/x"}},
		{"proc-sub", runner.Mount{Source: "/proc/sys", Target: "/home/agent/x"}},
		{"sys", runner.Mount{Source: "/sys", Target: "/home/agent/x"}},
		{"sys-cgroup", runner.Mount{Source: "/sys/fs/cgroup", Target: "/work/x"}},
		{"dev", runner.Mount{Source: "/dev", Target: "/home/agent/x"}},
		{"dev-mem", runner.Mount{Source: "/dev/mem", Target: "/work/x"}},
		{"run", runner.Mount{Source: "/run", Target: "/home/agent/x"}},
		{"var-run", runner.Mount{Source: "/var/run", Target: "/home/agent/x"}},
		{"var-lib-docker", runner.Mount{Source: "/var/lib/docker", Target: "/work/x"}},
		{"docker-sock-varrun", runner.Mount{Source: "/var/run/docker.sock", Target: "/work/x"}},
		{"docker-sock-anywhere", runner.Mount{Source: "/home/u/docker.sock", Target: "/work/x"}},
		{"etc", runner.Mount{Source: "/etc", Target: "/home/agent/x"}},
		{"etc-sub", runner.Mount{Source: "/etc/shadow", Target: "/work/x"}},
		{"boot", runner.Mount{Source: "/boot", Target: "/home/agent/x"}},
		{"root-home", runner.Mount{Source: "/root", Target: "/home/agent/x"}},
		{"root-home-sub", runner.Mount{Source: "/root/.ssh", Target: "/work/x"}},
		{"relative-source", runner.Mount{Source: "relative/path", Target: "/work/x"}},
		{"traversal-source", runner.Mount{Source: "/home/u/../../etc", Target: "/work/x"}},
		{"uncleaned-trailing-slash", runner.Mount{Source: "/home/u/repo/", Target: "/work/x"}},
		{"empty-source", runner.Mount{Source: "", Target: "/work/x"}},
		{"bad-target-prefix", runner.Mount{Source: "/home/u/repo", Target: "/etc"}},
		{"bad-target-usr", runner.Mount{Source: "/home/u/repo", Target: "/usr/local"}},
		{"relative-target", runner.Mount{Source: "/home/u/repo", Target: "work"}},
		{"empty-target", runner.Mount{Source: "/home/u/repo", Target: ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := createWithMounts(t, []runner.Mount{tc.m})
			if err == nil {
				t.Fatalf("CreateSandbox with denied mount %+v should FAIL CLOSED, got nil error", tc.m)
			}
			if !strings.Contains(err.Error(), "denied workspace mount") {
				t.Errorf("error should identify the denied mount, got: %v", err)
			}
		})
	}
}
