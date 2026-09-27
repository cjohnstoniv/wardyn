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

	// agentNamePrefix is the deterministic prefix of every agent container name
	// (agentContainerName). teardown recovers a run id from the name when the
	// run-id label is unreadable.
	agentNamePrefix = "wardyn-agent-"

	// demoAgentImagePrefix is the repository prefix of the demo agent images
	// `make agent-images` builds. They live in no registry, so a failed pull
	// for one of these has a make target as its fix.
	demoAgentImagePrefix = "wardyn/agent-"

	// labelRun tags every Wardyn-owned object with its run UUID for audit and
	// teardown selectors.
	labelRun       = "wardyn.run-id"
	labelComponent = "wardyn.component" // "agent" | "proxy"
	labelManaged   = "wardyn.managed"   // "true"

	componentAgent = "agent"
	componentProxy = "proxy"
)

// Object names are deterministic per run so teardown is selector-free and
// idempotent: a crashed control plane can reconstruct every name from the run
// UUID alone.
func agentContainerName(runID uuid.UUID) string { return agentNamePrefix + runID.String() }

// runIDFromAgentName recovers the run UUID from a deterministic agent
// container name. The Docker daemon reports the name with a leading slash;
// both forms are accepted. Teardown fallback when the wardyn.run-id label is
// unreadable.
func runIDFromAgentName(name string) (uuid.UUID, error) {
	name = strings.TrimPrefix(name, "/")
	if !strings.HasPrefix(name, agentNamePrefix) {
		return uuid.Nil, fmt.Errorf("%q is not a wardyn agent container name", name)
	}
	return uuid.Parse(strings.TrimPrefix(name, agentNamePrefix))
}
func proxyContainerName(runID uuid.UUID) string { return "wardyn-proxy-" + runID.String() }

// internalNetName is the per-run user-defined *internal* network joined by
// both the agent and the proxy. Internal=true => no gateway => L0 preserved.
func internalNetName(runID uuid.UUID) string { return "wardyn-int-" + runID.String() }

// wardynLabels stamps every Wardyn-owned object so audit and teardown
// selectors can find them by run and component. extra is applied FIRST and
// the three reserved keys are stamped LAST (same rule as the k8s driver's
// naming.go): extra is caller-supplied, and applying it last stops an entry
// named wardyn.run-id from renaming the run on every object it stamps.
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
