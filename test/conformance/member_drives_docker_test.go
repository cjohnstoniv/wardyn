// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package conformance_test

import (
	"context"
	"os"
	"testing"
	"time"

	dockerclient "github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/runner/docker"
	"github.com/cjohnstoniv/wardyn/internal/runner/orchestrator"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/test/conformance"
)

// TestMemberDriveDocker is the member-drive row on Docker: run A's file is
// there for run B, a kept ended run holds the volume against delete, and a
// delete with nothing holding it removes the volume.
func TestMemberDriveDocker(t *testing.T) {
	if os.Getenv("WARDYN_TEST_DOCKER") != "1" {
		t.Skip("WARDYN_TEST_DOCKER=1 not set; skipping docker conformance")
	}
	cli, err := dockerclient.New(dockerclient.FromEnv)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	const network = "wardyn-conformance-member-drive"
	if _, err := cli.NetworkCreate(context.Background(), network, dockerclient.NetworkCreateOptions{Driver: "bridge"}); err != nil {
		t.Fatalf("create network %s: %v", network, err)
	}
	t.Cleanup(func() { _, _ = cli.NetworkRemove(context.Background(), network, dockerclient.NetworkRemoveOptions{}) })

	sub, err := docker.New(docker.Config{ProxyImage: "busybox:latest", ProxyCmd: []string{"sleep", "infinity"}, InternalNetwork: network})
	if err != nil {
		t.Fatalf("docker.New: %v", err)
	}
	mount := conformance.MemberDriveMount(t, types.DriveBackendDockerVolume, "sub-conformance-member", 0)
	t.Cleanup(func() {
		_, _ = cli.VolumeRemove(context.Background(), mount.ObjectName, dockerclient.VolumeRemoveOptions{Force: true})
	})
	conformance.RunMemberDriveLifecycle(t, orchestrator.New(sub), conformance.Options{SandboxImage: "busybox:latest", Timeout: 3 * time.Minute},
		mount, func(t *testing.T) bool {
			_, err := cli.VolumeInspect(context.Background(), mount.ObjectName, dockerclient.VolumeInspectOptions{})
			return err == nil
		})
}
