// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"testing"

	"github.com/moby/moby/client"
)

// cancelMidStopDocker is a daemon that, like a real one, gives up on a call whose context is done.
// The first ContainerStop cancels the caller's call, as the organisation does when it resets a slow one.
type cancelMidStopDocker struct {
	*fakeDocker
	cancel context.CancelFunc
}

func (c *cancelMidStopDocker) ContainerStop(ctx context.Context, id string, o client.ContainerStopOptions) (client.ContainerStopResult, error) {
	c.cancel()
	if err := ctx.Err(); err != nil {
		return client.ContainerStopResult{}, err
	}
	return c.fakeDocker.ContainerStop(ctx, id, o)
}

func (c *cancelMidStopDocker) ContainerInspect(ctx context.Context, id string, o client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	if err := ctx.Err(); err != nil {
		return client.ContainerInspectResult{}, err
	}
	return c.fakeDocker.ContainerInspect(ctx, id, o)
}

func (c *cancelMidStopDocker) ContainerRemove(ctx context.Context, id string, o client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	if err := ctx.Err(); err != nil {
		return client.ContainerRemoveResult{}, err
	}
	return c.fakeDocker.ContainerRemove(ctx, id, o)
}

func (c *cancelMidStopDocker) NetworkRemove(ctx context.Context, id string, o client.NetworkRemoveOptions) (client.NetworkRemoveResult, error) {
	if err := ctx.Err(); err != nil {
		return client.NetworkRemoveResult{}, err
	}
	return c.fakeDocker.NetworkRemove(ctx, id, o)
}

// A caller that cancels its call mid-stop does not strand the run's proxy sidecar or network: the
// stop and the teardown finish on their own context.
func TestStopSandbox_FinishesWhenTheCallIsCancelled(t *testing.T) {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := newWithClient(&cancelMidStopDocker{fakeDocker: f, cancel: cancel}, Config{ProxyImage: "wardyn-proxy:dev"})

	sb, err := d.CreateSandbox(context.Background(), testSpec())
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	if err := d.StopSandbox(ctx, sb.Ref); err != nil {
		t.Fatalf("StopSandbox after the call was cancelled: %v", err)
	}
	runID := testSpec().RunID
	if c := f.containers[sb.Ref]; c != nil && !c.removed {
		t.Error("the agent container is still there after a cancelled StopSandbox")
	}
	if c := f.containers[proxyContainerName(runID)]; c != nil && !c.removed {
		t.Error("the proxy sidecar is still there after a cancelled StopSandbox")
	}
	if _, ok := f.networks[internalNetName(runID)]; ok {
		t.Error("the internal network is still there after a cancelled StopSandbox")
	}
}
