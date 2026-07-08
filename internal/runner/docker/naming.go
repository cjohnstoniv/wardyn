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

// runIDFromAgentName recovers the run UUID from a deterministic agent container
// name. The Docker daemon reports the name with a leading slash
// ("/wardyn-agent-<uuid>"); both forms are accepted. It is the teardown fallback
// when the wardyn.run-id label is unreadable, so the sibling proxy + per-run
// network can still be located by their deterministic names.
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
// selectors can find them by run and component.
func wardynLabels(runID uuid.UUID, component string, extra map[string]string) map[string]string {
	l := map[string]string{
		labelManaged:   "true",
		labelRun:       runID.String(),
		labelComponent: component,
	}
	for k, v := range extra {
		l[k] = v
	}
	return l
}
