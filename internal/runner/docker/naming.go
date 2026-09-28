// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"fmt"
	"strings"

	"github.com/google/uuid"
)

const (
	driverName = "docker"

	// agentNamePrefix is the deterministic prefix of every agent container
	// name; teardown recovers a run id from it when the run-id label is
	// unreadable.
	agentNamePrefix = "wardyn-agent-"

	// demoAgentImagePrefix is the repository prefix of demo agent images
	// `make agent-images` builds; they live in no registry, so a failed pull
	// points at that make target.
	demoAgentImagePrefix = "wardyn/agent-"

	// labelRun tags every Wardyn-owned object with its run UUID for audit and
	// teardown lookups.
	labelRun       = "wardyn.run-id"
	labelComponent = "wardyn.component" // "agent" | "proxy"
	labelManaged   = "wardyn.managed"   // "true"

	componentAgent = "agent"
	componentProxy = "proxy"
)

// Object names are deterministic per run, so teardown is selector-free and
// idempotent — a crashed control plane can reconstruct every name from the
// run UUID alone.
func agentContainerName(runID uuid.UUID) string { return agentNamePrefix + runID.String() }

// runIDFromAgentName recovers the run UUID from a deterministic agent
// container name (the daemon may report it with a leading slash; both forms
// accepted) — teardown's fallback when the wardyn.run-id label is unreadable.
func runIDFromAgentName(name string) (uuid.UUID, error) {
	name = strings.TrimPrefix(name, "/")
	if !strings.HasPrefix(name, agentNamePrefix) {
		return uuid.Nil, fmt.Errorf("%q is not a wardyn agent container name", name)
	}
	return uuid.Parse(strings.TrimPrefix(name, agentNamePrefix))
}
func proxyContainerName(runID uuid.UUID) string { return "wardyn-proxy-" + runID.String() }

// internalNetName is the per-run internal network joined by agent and proxy;
// internal=true means no gateway, preserving L0.
func internalNetName(runID uuid.UUID) string { return "wardyn-int-" + runID.String() }

// wardynLabels stamps every Wardyn-owned object for audit/teardown lookup by
// run and component. extra applies first and the three reserved keys stamp
// last (matching the k8s driver), so a caller-supplied wardyn.run-id entry
// can't rename the run.
func wardynLabels(runID uuid.UUID, component string, extra map[string]string) map[string]string {
	l := make(map[string]string, len(extra)+3)
	for k, v := range extra {
		l[k] = v
	}
	l[labelManaged] = "true"
	l[labelRun] = runID.String()
	l[labelComponent] = component
	return l
}
