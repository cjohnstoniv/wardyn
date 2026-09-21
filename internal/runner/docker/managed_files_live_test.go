// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

// Real-daemon checks of managed-file delivery. What they pin is what the
// daemon does to a filesystem, which no fake can show. Skipped unless
// WARDYN_TEST_DOCKER=1.

import (
	"context"
	"fmt"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// A delivery must never change a directory it did not create. A bind mount is
// the sharpest case: the daemon mounts binds for the copy, so a directory entry
// in the archive re-owned the HOST directory to root and its owner could no
// longer write to it. The bind sits on the managed-file directory itself — a
// target no mount rule allows — so this holds the delivery to that property
// whatever paths the contract accepts.
func TestManagedFiles_LeavesAMountedDirectoryAlone_RealDocker(t *testing.T) {
	if os.Getenv("WARDYN_TEST_DOCKER") != "1" {
		t.Skip("set WARDYN_TEST_DOCKER=1 to run the real-Docker managed-file tests")
	}
	d := newNetworkTestDriver(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := d.ensureImage(ctx, "busybox:latest", func() {}); err != nil {
		t.Fatalf("ensure busybox: %v", err)
	}

	hostDir := t.TempDir()
	before := hostDirState(t, hostDir)
	created, err := d.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name:       "wardyn-managed-bind-" + uuid.NewString()[:8],
		Config:     &container.Config{Image: "busybox:latest", Cmd: []string{"true"}},
		HostConfig: &container.HostConfig{Binds: []string{hostDir + ":/etc/claude-code"}},
	})
	if err != nil {
		t.Fatalf("create container: %v", err)
	}
	t.Cleanup(func() {
		_, _ = d.cli.ContainerRemove(context.Background(), created.ID, client.ContainerRemoveOptions{Force: true})
	})

	err = d.deliverManagedFiles(ctx, created.ID, []runner.ManagedFile{
		{Path: "/etc/claude-code/managed-settings.json", Content: []byte(`{"managed":true}`)},
	})
	if after := hostDirState(t, hostDir); after != before {
		t.Errorf("delivery changed the HOST directory bound at /etc/claude-code: %s -> %s", before, after)
	}
	if err == nil {
		t.Error("delivery into a directory that already existed (a bind mount) succeeded; it must be refused")
	} else {
		t.Logf("refused: %v", err)
	}
}

// hostDirState is dir's owner, mode and entries as one comparable string.
func hostDirState(t *testing.T, dir string) string {
	t.Helper()
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat %s: %v", dir, err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("stat %s: no owner information on this platform", dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return fmt.Sprintf("owner %d:%d mode %04o entries %v", st.Uid, st.Gid, fi.Mode().Perm(), names)
}
