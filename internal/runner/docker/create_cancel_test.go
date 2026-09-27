// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"errors"
	"testing"

	"github.com/moby/moby/client"
)

// cancelMidCreateDocker is fakeDocker with a real client's context honesty on
// the calls a rollback makes, plus a kill landing mid-create: the agent's
// ContainerCreate cancels the create's context and fails with it.
type cancelMidCreateDocker struct {
	*fakeDocker
	cancel context.CancelFunc
	agent  string
}

func (c *cancelMidCreateDocker) ContainerCreate(ctx context.Context, opts client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
	if opts.Name == c.agent {
		c.cancel()
		return client.ContainerCreateResult{}, ctx.Err()
	}
	return c.fakeDocker.ContainerCreate(ctx, opts)
}

func (c *cancelMidCreateDocker) ContainerRemove(ctx context.Context, id string, opts client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	if err := ctx.Err(); err != nil {
		return client.ContainerRemoveResult{}, err
	}
	return c.fakeDocker.ContainerRemove(ctx, id, opts)
}

func (c *cancelMidCreateDocker) NetworkRemove(ctx context.Context, id string, opts client.NetworkRemoveOptions) (client.NetworkRemoveResult, error) {
	if err := ctx.Err(); err != nil {
		return client.NetworkRemoveResult{}, err
	}
	return c.fakeDocker.NetworkRemove(ctx, id, opts)
}

// TestCreateSandbox_CancelledMidCreateRollsBack: a kill of a STARTING run
// cancels its CreateSandbox (#1182). The rollback must still remove the proxy
// sidecar and the per-run network the create had already made, which it can
// only do because every rollback step runs on its own context, not the
// cancelled one.
func TestCreateSandbox_CancelledMidCreateRollsBack(t *testing.T) {
	f := newFakeDocker()
	f.images["busybox:latest"] = true
	spec := testSpec()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := newWithClient(&cancelMidCreateDocker{fakeDocker: f, cancel: cancel, agent: agentContainerName(spec.RunID)},
		Config{ProxyImage: "wardyn-proxy:dev"})

	if _, err := d.CreateSandbox(ctx, spec); !errors.Is(err, context.Canceled) {
		t.Fatalf("CreateSandbox = %v, want context.Canceled", err)
	}
	if _, ok := f.networks[internalNetName(spec.RunID)]; ok {
		t.Error("the per-run network survived the cancelled create")
	}
	if c := f.containers[proxyContainerName(spec.RunID)]; c == nil || !c.removed {
		t.Errorf("the proxy sidecar survived the cancelled create (record: %+v)", c)
	}
}
