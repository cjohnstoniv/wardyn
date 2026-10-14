// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// RelayProbe asks whether a proxy container on Network reaches Target through the hosts
// entry Host ("name:address"), the way a run's proxy reaches the runner's relay.
type RelayProbe struct {
	Network string        // the bridge a run's proxy leaves through (Config.InternalNetwork when empty)
	Host    string        // ExtraHosts entry, "name:address" ("address" may be "host-gateway")
	Target  string        // "name:port" the canary dials
	Timeout time.Duration // whole probe, pull excluded; 20s when zero
}

// ErrRelayUnreachable means the canary ran and could not connect to the relay.
var ErrRelayUnreachable = errors.New("relay unreachable from a proxy container")

const (
	defaultRelayProbeTimeout = 20 * time.Second
	componentRelayProbe      = "relay-probe"
)

// ProbeRelay runs the proxy image's -egress-canary in a throwaway container with the given
// hosts entry. A nil return means the dial succeeded; ErrRelayUnreachable means it ran and
// failed; any other error means the probe itself could not run, which proves nothing about
// the relay. The pull, if the image is absent, is bounded by ctx, not by p.Timeout.
func (d *Driver) ProbeRelay(ctx context.Context, p RelayProbe) error {
	image := d.cfg.ProxyImage
	if err := d.ensureImage(ctx, image, nil); err != nil {
		return fmt.Errorf("docker: probe relay: %w", err)
	}
	// PullImage drops in-stream errors, so a "successful" pull can leave nothing behind.
	present, err := d.imagePresent(ctx, image)
	if err != nil {
		return fmt.Errorf("docker: probe relay: %w", err)
	}
	if !present {
		return fmt.Errorf("docker: probe relay: image %q is still absent after pull", image)
	}

	timeout := p.Timeout
	if timeout <= 0 {
		timeout = defaultRelayProbeTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	created, err := d.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: "wardyn-relay-probe-" + uuid.New().String(),
		Config: &container.Config{
			Image: image,
			Cmd:   []string{"-egress-canary", p.Target}, // the image ENTRYPOINT is wardyn-proxy
			Labels: map[string]string{
				labelManaged:   "true",
				labelComponent: componentRelayProbe,
			},
		},
		HostConfig: &container.HostConfig{
			NetworkMode:    container.NetworkMode(cmp.Or(p.Network, d.cfg.InternalNetwork)),
			ExtraHosts:     []string{p.Host},
			CapDrop:        []string{"ALL"},
			SecurityOpt:    []string{"no-new-privileges"},
			ReadonlyRootfs: true,
			AutoRemove:     false, // removed explicitly below
			Resources:      proxyResources(false),
		},
	})
	if err != nil {
		return fmt.Errorf("docker: probe relay: create: %w", err)
	}
	defer func() {
		// Background, not ctx: the probe's bound may be spent, and a leaked container is worse.
		_, _ = d.cli.ContainerRemove(context.Background(), created.ID, client.ContainerRemoveOptions{Force: true})
	}()

	if _, err := d.cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("docker: probe relay: start: %w", err)
	}

	wait := d.cli.ContainerWait(ctx, created.ID, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	select {
	case res := <-wait.Result:
		if res.Error != nil {
			return fmt.Errorf("docker: probe relay: wait: %s", res.Error.Message)
		}
		switch res.StatusCode {
		case 0:
			return nil
		case 1:
			return fmt.Errorf("%w: %s did not reach %s", ErrRelayUnreachable, p.Host, p.Target)
		default:
			return fmt.Errorf("docker: probe relay: canary exited %d, not a dial result", res.StatusCode)
		}
	case werr := <-wait.Error:
		return fmt.Errorf("docker: probe relay: wait: %w", werr)
	case <-ctx.Done():
		return fmt.Errorf("docker: probe relay: no result within %s: %w", timeout, ctx.Err())
	}
}
