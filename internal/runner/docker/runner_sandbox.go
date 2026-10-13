// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"fmt"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/moby/moby/client"
)

var _ runner.RunnerSandboxProver = (*Driver)(nil)

// ProveRunnerSandbox is runner.RunnerSandboxProver. ref must be a
// deterministic agent name, and each of the run's agent and proxy containers
// that exists must carry wardyn.managed, the same run id and runnerID. When
// neither exists, nothing on the engine answers to the run's names.
func (d *Driver) ProveRunnerSandbox(ctx context.Context, ref, runnerID string) error {
	id, err := runIDFromAgentName(ref)
	if err != nil || ref != agentContainerName(id) {
		return fmt.Errorf("docker: %q is not a Wardyn agent container name", ref)
	}
	for _, name := range []string{ref, proxyContainerName(id)} {
		res, err := d.cli.ContainerInspect(ctx, name, client.ContainerInspectOptions{})
		if isNotFound(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("docker: inspect %s: %w", name, err)
		}
		var labels map[string]string
		if res.Container.Config != nil {
			labels = res.Container.Config.Labels
		}
		if labels[labelManaged] != "true" || labels[labelRun] != id.String() || labels[runner.LabelRunnerID] != runnerID {
			return fmt.Errorf("docker: container %s was not created by this runner", name)
		}
	}
	return nil
}
