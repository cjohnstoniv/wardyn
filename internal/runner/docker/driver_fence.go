// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"

	"github.com/containerd/errdefs"
	"github.com/google/uuid"
	"github.com/moby/moby/client"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// labelProxyGeneration is the policy generation a replacement proxy was started for (ReplaceProxyHashedAt).
// A proxy without it, or with one that is not a number, is generation 0: none.
const labelProxyGeneration = "wardyn.proxy.generation"

var (
	_ runner.ProxyFencer            = (*Driver)(nil)
	_ runner.ProxyGenerationStopper = (*Driver)(nil)
	_ runner.ProxyReviverAt         = (*Driver)(nil)
)

// FenceProxy cuts the agent ref off every network (runner.ProxyFencer): the contained run's agent can
// reach nothing, whether it runs or not. A container the engine does not hold is already cut. It answers nil
// only once a second inspect shows no endpoint left. The proxy is stopped after that, best effort: the
// agent no longer reaches it, so its stop is tidiness and never decides the answer.
//
// A stopped agent is cut too, and a later start of it brings no endpoint back: Docker does not reconnect
// a container it was disconnected from (TestFenceProxy_StartAfterFenceStaysCut).
func (d *Driver) FenceProxy(ctx context.Context, ref string) error {
	if v, err := d.vmLookup(ctx, ref); err != nil {
		return err
	} else if v != nil {
		return fmt.Errorf("%w: %w", runner.ErrEndUnsupported, errVMUnsupported)
	}
	nets, found, err := d.agentEndpoints(ctx, ref)
	if err != nil || !found {
		return err
	}
	var errs []error
	for _, name := range nets {
		_, err := d.cli.NetworkDisconnect(ctx, name, client.NetworkDisconnectOptions{Container: ref, Force: true})
		// A daemon that says the container is not on the network, or that the network or the container is
		// gone, has said the endpoint is not there.
		if err != nil && !isNotFound(err) && !isNotConnected(err) {
			errs = append(errs, fmt.Errorf("docker: fence: disconnect %s from %s: %w", ref, name, err))
		}
	}
	left, found, err := d.agentEndpoints(ctx, ref)
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	if found && len(left) > 0 {
		return errors.Join(append(errs, fmt.Errorf("docker: fence: agent %s is still on %s", ref, strings.Join(left, ", ")))...)
	}
	if err := d.stopProxy(ctx, ref); err != nil {
		slog.Warn("docker: fence: the proxy of a fenced agent did not stop", "ref", ref, "err", err)
	}
	return nil
}

// agentEndpoints lists the networks the container ref has an endpoint on, sorted. found is false for a
// container the engine does not hold. The "none" network is no endpoint: it carries no traffic.
func (d *Driver) agentEndpoints(ctx context.Context, ref string) (nets []string, found bool, err error) {
	res, err := d.cli.ContainerInspect(ctx, ref, client.ContainerInspectOptions{})
	if isNotFound(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("docker: fence: inspect %s: %w", ref, err)
	}
	if ns := res.Container.NetworkSettings; ns != nil {
		for name := range ns.Networks {
			if name != "none" {
				nets = append(nets, name)
			}
		}
	}
	slices.Sort(nets)
	return nets, true, nil
}

// isNotConnected detects the daemon's "container is not connected to the network" refusal of a disconnect.
func isNotConnected(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "is not connected")
}

// isNameConflict detects a create refused because its name is taken.
func isNameConflict(err error) bool {
	return err != nil && (errdefs.IsConflict(err) || strings.Contains(err.Error(), "is already in use"))
}

// proxyAt inspects the run's proxy by its name and answers the container id it found, the policy generation
// its label names (0 for none, or one that is not a non-negative number) and whether there is one. Every
// removal that follows names the id it answered, so a proxy started under the same name in between is
// never the one removed.
func (d *Driver) proxyAt(ctx context.Context, id uuid.UUID) (containerID string, gen int, found bool, err error) {
	res, err := d.cli.ContainerInspect(ctx, proxyContainerName(id), client.ContainerInspectOptions{})
	if isNotFound(err) {
		return "", 0, false, nil
	}
	if err != nil {
		return "", 0, false, fmt.Errorf("docker: inspect proxy: %w", err)
	}
	if res.Container.Config != nil {
		if n, perr := strconv.Atoi(res.Container.Config.Labels[labelProxyGeneration]); perr == nil && n > 0 {
			gen = n
		}
	}
	return res.Container.ID, gen, true, nil
}

// removeProxyAt is removeProxy by the container id proxyAt answered. cid "" is no proxy: nothing to remove,
// and the producer is settled as removeProxy does.
func (d *Driver) removeProxyAt(ctx context.Context, runID uuid.UUID, cid string) error {
	d.closeLocalModels(runID) // before the remove frees the proxy's addresses
	if cid != "" {
		if _, err := d.cli.ContainerRemove(ctx, cid, client.ContainerRemoveOptions{Force: true}); err != nil && !isNotFound(err) {
			return fmt.Errorf("docker: remove proxy: %w", err)
		}
	}
	return d.proxyGone(ctx, runID)
}

// stopProxyAt is stopProxy by the container id proxyAt answered: the graceful stop, the kill when that
// fails, then the removal, all of that container and no other.
func (d *Driver) stopProxyAt(ctx context.Context, runID uuid.UUID, cid string) error {
	if cid == "" {
		return d.removeProxyAt(ctx, runID, "")
	}
	d.drainSink(ctx, runID)
	timeout := int(stopTimeout.Seconds())
	_, err := d.cli.ContainerStop(ctx, cid, client.ContainerStopOptions{Timeout: &timeout})
	if err != nil && !isNotFound(err) {
		if _, kerr := d.cli.ContainerKill(ctx, cid, client.ContainerKillOptions{Signal: "KILL"}); kerr != nil && !isNotFound(kerr) && !isNotRunning(kerr) {
			return fmt.Errorf("docker: stop proxy: %w; kill proxy: %w", err, kerr)
		}
	}
	return d.removeProxyAt(ctx, runID, cid)
}

// checkStopGeneration resolves the run's proxy for a stop or an end that acts for gen. A proxy started for a
// newer generation refuses it with runner.ErrProxyGenerationNewer and nothing is touched; the same
// generation is the caller's own proxy and goes.
func (d *Driver) checkStopGeneration(ctx context.Context, ref string, gen int) (runID uuid.UUID, cid string, err error) {
	runID, err = d.proxyRunID(ctx, ref)
	if err != nil {
		return uuid.Nil, "", err
	}
	cid, have, _, err := d.proxyAt(ctx, runID)
	if err != nil {
		return uuid.Nil, "", err
	}
	if have > gen {
		return uuid.Nil, "", fmt.Errorf("%w: it is for generation %d, this call is for %d", runner.ErrProxyGenerationNewer, have, gen)
	}
	return runID, cid, nil
}

// StopProxyAt is StopProxy that acts for policy generation gen (runner.ProxyGenerationStopper): gen 0 is
// StopProxy. Otherwise a proxy started for a newer generation is left running, and the proxy it stops is
// the one it inspected.
func (d *Driver) StopProxyAt(ctx context.Context, ref string, gen int) error {
	if gen == 0 {
		return d.StopProxy(ctx, ref)
	}
	runID, cid, err := d.checkStopGeneration(ctx, ref, gen)
	if err != nil {
		return err
	}
	err = d.stopProxyAt(ctx, runID, cid)
	if err == nil {
		return nil
	}
	// Fail closed as StopProxy does: a proxy that would not go costs the agent its run.
	timeout := int(stopTimeout.Seconds())
	if _, aerr := d.cli.ContainerStop(ctx, ref, client.ContainerStopOptions{Timeout: &timeout}); aerr != nil && !isNotFound(aerr) {
		return errors.Join(err, fmt.Errorf("docker: stop agent: %w", aerr))
	}
	return err
}

// EndSandboxAt is EndSandbox that acts for policy generation gen (runner.ProxyGenerationStopper): gen 0 is
// EndSandbox. Otherwise a proxy started for a newer generation refuses the end and nothing is touched; the
// proxy it removes is the one it inspected.
func (d *Driver) EndSandboxAt(ctx context.Context, ref string, gen int) (endErr error) {
	if gen == 0 {
		return d.EndSandbox(ctx, ref)
	}
	runID, cid, err := d.checkStopGeneration(ctx, ref, gen)
	if err != nil {
		return err
	}
	wipeErr := d.wipeAWSCaches(ctx, ref)
	if v := d.vmFor(ctx, ref); v != nil {
		d.vmPowerdown(ctx, v, ref)
		defer func() { endErr = errors.Join(endErr, d.removeVMSidecars(ctx, v.runID)) }()
	}
	timeout := int(stopTimeout.Seconds())
	if _, err := d.cli.ContainerStop(ctx, ref, client.ContainerStopOptions{Timeout: &timeout}); err != nil && !isNotFound(err) {
		return fmt.Errorf("docker: stop agent: %w", err)
	}
	if err := d.stopProxyAt(ctx, runID, cid); err != nil {
		return err
	}
	return wipeErr
}

// removeProxyForReplace removes the run's proxy ahead of a replacement that acts for policy generation gen.
// With gen > 0, a proxy whose label is gen or newer refuses it with runner.ErrProxyGenerationNewer before
// anything is touched, the drain included. What it removes is the container it inspected: a proxy
// started under the same name since is not it. A failed remove is runner.ErrProxyReplaceFailed, as a
// failed removeProxy was; the inspect and the refusal are not, for nothing was removed.
func (d *Driver) removeProxyForReplace(ctx context.Context, id uuid.UUID, gen int) error {
	cid, have, _, err := d.proxyAt(ctx, id)
	if err != nil {
		return fmt.Errorf("docker: replace proxy: %w", err)
	}
	if gen > 0 && have >= gen {
		return fmt.Errorf("%w: it is for generation %d, this replace is for %d", runner.ErrProxyGenerationNewer, have, gen)
	}
	d.drainSink(ctx, id)
	// A failed remove may still have removed it.
	if err := d.removeProxyAt(ctx, id, cid); err != nil {
		return fmt.Errorf("%w: %w", runner.ErrProxyReplaceFailed, err)
	}
	return nil
}
