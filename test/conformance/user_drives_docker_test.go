// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package conformance_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	dockerclient "github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/docker"
	"github.com/cjohnstoniv/wardyn/internal/runner/orchestrator"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/test/conformance"
)

// dockerDriveFixture lays out the UserDrives case's host tree under one temp
// directory and returns the WARDYN_USER_DRIVE_HOST_ROOTS value the driver
// under test must be built with:
//
//	share/                      the ONE configured root
//	share/<home>/               the allowed drive
//	share-outside/<home>/       outside the root, and otherwise a perfect drive
//	share/<link> -> share-outside/<link>   inside the root by name, outside it once resolved
//
// share-outside is a SIBLING whose name starts with the root's own, so a
// ceiling that compares bare string prefixes admits it; and it is refused by
// the ceiling ALONE — it is inside its own host_root, named after its home, and
// mounts cleanly on a driver whose roots include it (RefusedDrive.Allowing).
// Disabling or widening the ceiling therefore turns that sub-case red.
//
// The drive directories are 0777 so uid 1000 can write them whatever uid runs
// the test; the locked directory is made by a root container, and so is the
// cleanup, which the test's own uid could not do for a 0700 root-owned tree.
func dockerDriveFixture(t *testing.T) (roots []string, fx *conformance.UserDriveFixture) {
	t.Helper()
	cli, err := dockerclient.New(dockerclient.FromEnv)
	if err != nil {
		t.Fatalf("dockerDriveFixture: create client: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	base := t.TempDir()
	t.Cleanup(func() { runAsRootIn(t, cli, base, "rm -rf /d/share /d/share-outside") })
	share := filepath.Join(base, "share")
	outside := filepath.Join(base, "share-outside")
	const home, linkHome = "conf-alice", "conf-carol"
	for _, dir := range []string{filepath.Join(share, home), filepath.Join(outside, home), filepath.Join(outside, linkHome)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o777); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(outside, linkHome), filepath.Join(share, linkHome)); err != nil {
		t.Fatal(err)
	}

	drive := func(hostRoot, home string) types.DriveMount {
		return types.DriveMount{
			DriveID:    uuid.New(),
			Backend:    types.DriveBackendHostPath,
			ObjectName: filepath.Join(hostRoot, home),
			HostRoot:   hostRoot,
			DriveName:  "conformance-share",
			HomeName:   home,
			Target:     runner.DriveTarget,
		}
	}

	widened, err := docker.New(docker.Config{
		ProxyImage:         "busybox:latest",
		ProxyCmd:           []string{"sleep", "infinity"},
		UserDriveHostRoots: []string{share, outside},
	})
	if err != nil {
		t.Fatalf("docker.New (widened ceiling): %v", err)
	}

	return []string{share}, &conformance.UserDriveFixture{
		Mount: drive(share, home),
		Lock: func(t *testing.T) {
			runAsRootIn(t, cli, filepath.Join(share, home), "mkdir /d/"+conformance.UserDriveLockedDir+
				" && echo s > /d/"+conformance.UserDriveLockedDir+"/"+conformance.UserDriveLockedFile+
				" && chmod 0700 /d/"+conformance.UserDriveLockedDir)
		},
		ReadBack: func(t *testing.T, name string) string {
			b, err := os.ReadFile(filepath.Join(share, home, name))
			if err != nil {
				t.Fatalf("read back %s from the host: %v", name, err)
			}
			return string(b)
		},
		Refused: []conformance.RefusedDrive{
			{
				Name:     "SiblingOfTheRoot",
				Mount:    drive(outside, home),
				WantErr:  "denied user drive mount",
				Allowing: orchestrator.New(widened),
			},
			{
				Name:    "SymlinkOutOfTheRoot",
				Mount:   drive(share, linkHome),
				WantErr: "denied user drive mount",
			},
		},
	}
}

// runAsRootIn runs script in a one-shot ROOT busybox container with dir bound
// at /d, failing the test on a non-zero exit.
func runAsRootIn(t *testing.T, cli *dockerclient.Client, dir, script string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	created, err := cli.ContainerCreate(ctx, dockerclient.ContainerCreateOptions{
		Name:       "wardyn-conformance-drive-fixture-" + uuid.NewString()[:8],
		Config:     &container.Config{Image: "busybox:latest", User: "0", Cmd: []string{"sh", "-c", script}},
		HostConfig: &container.HostConfig{Mounts: []mount.Mount{{Type: mount.TypeBind, Source: dir, Target: "/d"}}},
	})
	if err != nil {
		t.Fatalf("drive fixture: create root helper: %v", err)
	}
	defer func() {
		_, _ = cli.ContainerRemove(context.Background(), created.ID, dockerclient.ContainerRemoveOptions{Force: true})
	}()
	if _, err := cli.ContainerStart(ctx, created.ID, dockerclient.ContainerStartOptions{}); err != nil {
		t.Fatalf("drive fixture: start root helper: %v", err)
	}
	wait := cli.ContainerWait(ctx, created.ID, dockerclient.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	select {
	case res := <-wait.Result:
		if res.StatusCode != 0 {
			t.Fatalf("drive fixture: root helper %q exited %d", script, res.StatusCode)
		}
	case werr := <-wait.Error:
		t.Fatalf("drive fixture: wait root helper: %v", werr)
	}
}
