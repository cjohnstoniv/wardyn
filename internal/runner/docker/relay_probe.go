// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// ErrRelayUnreachable means the canary ran and could not connect to the relay target.
var ErrRelayUnreachable = errors.New("relay unreachable from a proxy container")

const (
	// relayProbeHost is the name the canary dials; ExtraHosts maps it to the target, so the
	// lookup goes through /etc/hosts the way the real proxy's control-plane name does.
	relayProbeHost = "wardyn-relay-probe"
	// relayProbeTimeout covers create, start and wait. It stays well above the canary's own 5s
	// dial plus container start: a tighter bound would report "no result" for an unreachable relay.
	relayProbeTimeout = 20 * time.Second
	// relayProbeCleanup bounds the removals, which run on a fresh context so a spent probe
	// bound cannot leak a container.
	relayProbeCleanup    = 10 * time.Second
	componentRelayProbe  = "relay-probe"
	proxyEntrypointPath  = "/usr/local/bin/wardyn-proxy"
	relayProbeNamePrefix = "wardyn-relay-probe-"
)

// ProbeRelay asks whether a container on the internal network, the proxy's only route out,
// reaches target:port (target is "host-gateway" or an IP), running the proxy image's own
// -egress-canary. It never pulls: the caller makes the image present first (EnsureProxyImage).
//
// nil means the dial connected. ErrRelayUnreachable means the canary ran and could not
// connect. Any other error means the probe could not run, which proves nothing about the relay.
func (d *Driver) ProbeRelay(ctx context.Context, target string, port uint16) error {
	image := d.proxyImage()
	present, err := d.imagePresent(ctx, image)
	if err != nil {
		return fmt.Errorf("docker: probe relay: %w", err)
	}
	if !present {
		return fmt.Errorf("docker: probe relay: proxy image %q is not present locally; run `docker pull %s`", image, image)
	}
	if err := d.checkImageArch(ctx, image, ""); err != nil {
		return fmt.Errorf("docker: probe relay: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, relayProbeTimeout)
	defer cancel()

	name := relayProbeNamePrefix + uuid.New().String()
	labels := map[string]string{labelManaged: "true", labelComponent: componentRelayProbe}
	if d.cfg.RunnerID != "" {
		labels[runner.LabelRunnerID] = d.cfg.RunnerID // lets a sweeper claim a stray
	}
	remove := func(id string) {
		rctx, rcancel := context.WithTimeout(context.Background(), relayProbeCleanup)
		defer rcancel()
		_, _ = d.cli.ContainerRemove(rctx, id, client.ContainerRemoveOptions{Force: true})
	}
	created, err := d.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: name,
		Config: &container.Config{
			Image: image,
			// Explicit, so an image that is not wardyn-proxy fails at start rather than
			// running some other entrypoint with these arguments.
			Entrypoint: []string{proxyEntrypointPath},
			Cmd:        []string{"-egress-canary", relayProbeHost + ":" + strconv.Itoa(int(port))},
			Labels:     labels,
		},
		HostConfig: &container.HostConfig{
			NetworkMode:    container.NetworkMode(d.cfg.InternalNetwork),
			ExtraHosts:     []string{relayProbeHost + ":" + target},
			CapDrop:        []string{"ALL"},
			SecurityOpt:    []string{"no-new-privileges"},
			ReadonlyRootfs: true,
			AutoRemove:     false, // removed explicitly below
			Resources:      proxyResources(false),
		},
	})
	if err != nil {
		// A create that timed out client-side may still have made the container.
		remove(name)
		return fmt.Errorf("docker: probe relay: create: %w", err)
	}
	defer remove(created.ID)

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
			return fmt.Errorf("%w: a container on this engine did not connect to %s:%d", ErrRelayUnreachable, target, port)
		default:
			return fmt.Errorf("docker: probe relay: the proxy image %s did not run its reachability check (exit %d); it must be wardyn-proxy 0.9.0 or later", image, res.StatusCode)
		}
	case werr := <-wait.Error:
		return fmt.Errorf("docker: probe relay: wait: %w", werr)
	case <-ctx.Done():
		return fmt.Errorf("docker: probe relay: no result within %s: %w", relayProbeTimeout, ctx.Err())
	}
}
