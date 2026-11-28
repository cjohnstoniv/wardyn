// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"errors"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// holderDocker lists what it is given and fails the test on any call that would change the daemon.
type holderDocker struct {
	*fakeDocker
	t     *testing.T
	items []container.Summary
	err   error
	calls []client.ContainerListOptions
}

func (h *holderDocker) ContainerList(_ context.Context, o client.ContainerListOptions) (client.ContainerListResult, error) {
	h.calls = append(h.calls, o)
	return client.ContainerListResult{Items: h.items}, h.err
}
func (h *holderDocker) ContainerCreate(context.Context, client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
	h.t.Error("the holder read created a container")
	return client.ContainerCreateResult{}, nil
}
func (h *holderDocker) ContainerRemove(context.Context, string, client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	h.t.Error("the holder read removed a container")
	return client.ContainerRemoveResult{}, nil
}
func (h *holderDocker) VolumeCreate(context.Context, client.VolumeCreateOptions) (client.VolumeCreateResult, error) {
	h.t.Error("the holder read created a volume")
	return client.VolumeCreateResult{}, nil
}
func (h *holderDocker) VolumeRemove(context.Context, string, client.VolumeRemoveOptions) (client.VolumeRemoveResult, error) {
	h.t.Error("the holder read removed a volume")
	return client.VolumeRemoveResult{}, nil
}

func holderDrive() types.DriveMount {
	return types.DriveMount{Backend: types.DriveBackendDockerVolume, ObjectName: "wardyn-drive-notes-1a2b3c"}
}

// TestDriveHolder_Docker_ReadOnly: the read lists every container (All) that mounts the volume, names the first
// without its leading slash, answers "" when none does, and creates and removes nothing.
func TestDriveHolder_Docker_ReadOnly(t *testing.T) {
	h := &holderDocker{fakeDocker: newFakeDocker(), t: t, items: []container.Summary{{ID: "c1", Names: []string{"/run-sandbox-1"}}}}
	d := newWithClient(h, Config{ProxyImage: "wardyn-proxy:dev"})
	got, err := d.DriveHolder(context.Background(), holderDrive())
	if err != nil || got != "run-sandbox-1" {
		t.Fatalf("DriveHolder = %q, %v; want run-sandbox-1", got, err)
	}
	if len(h.calls) != 1 || !h.calls[0].All {
		t.Fatalf("list calls = %+v; want exactly one with All set", h.calls)
	}
	if vals := h.calls[0].Filters["volume"]; len(vals) != 1 || !vals["wardyn-drive-notes-1a2b3c"] {
		t.Errorf("volume filter = %v; want the drive's volume", h.calls[0].Filters)
	}
	h.items = nil
	if got, err := d.DriveHolder(context.Background(), holderDrive()); err != nil || got != "" {
		t.Fatalf("DriveHolder with no container = %q, %v; want empty", got, err)
	}
	for name, m := range map[string]types.DriveMount{
		"cluster claim": {Backend: types.DriveBackendK8sPVC, ObjectName: "c"},
		"no object":     {Backend: types.DriveBackendDockerVolume},
	} {
		if got, err := d.DriveHolder(context.Background(), m); got != "" || !errors.Is(err, runner.ErrDriveHolderUnknown) {
			t.Errorf("%s: DriveHolder = %q, %v; want ErrDriveHolderUnknown", name, got, err)
		}
	}
}

// TestDriveHolder_Docker_ListErrorIsError: a daemon that cannot list is an error, never "no holder".
func TestDriveHolder_Docker_ListErrorIsError(t *testing.T) {
	boom := errors.New("daemon down")
	d := newWithClient(&holderDocker{fakeDocker: newFakeDocker(), t: t, err: boom}, Config{ProxyImage: "wardyn-proxy:dev"})
	got, err := d.DriveHolder(context.Background(), holderDrive())
	if got != "" || !errors.Is(err, boom) {
		t.Fatalf("DriveHolder = %q, %v; want the list error", got, err)
	}
}
