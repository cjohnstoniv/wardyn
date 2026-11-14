// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package k8s

import (
	"errors"
	"slices"

	"github.com/google/uuid"
	authzv1 "k8s.io/api/authorization/v1"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// keepAddressAccess is the verbs a kept run's proxy address needs in each runs namespace: the run's Service, and the
// EndpointSlice Wardyn writes for it.
var keepAddressAccess = []authzv1.ResourceAttributes{
	{Resource: "services", Verb: "create"},
	{Resource: "services", Verb: "get"},
	{Resource: "services", Verb: "delete"},
	{Group: "discovery.k8s.io", Resource: "endpointslices", Verb: "create"},
	{Group: "discovery.k8s.io", Resource: "endpointslices", Verb: "get"},
	{Group: "discovery.k8s.io", Resource: "endpointslices", Verb: "update"},
	{Group: "discovery.k8s.io", Resource: "endpointslices", Verb: "delete"},
	{Group: "discovery.k8s.io", Resource: "endpointslices", Verb: "deletecollection"},
}

// checkAgentSandboxKeep refuses the keep switch without the agent-sandbox backend.
func checkAgentSandboxKeep(cfg Config) error {
	if cfg.AgentSandboxKeep && !cfg.AgentSandbox {
		return errors.New("k8s: refusing to boot: WARDYN_K8S_AGENT_SANDBOX_KEEP needs WARDYN_K8S_AGENT_SANDBOX on")
	}
	return nil
}

func (d *Driver) keepOn() bool { return d.cfg.AgentSandbox && d.cfg.AgentSandboxKeep }

func keepServiceName(runID uuid.UUID) string { return "wardyn-proxy-svc-" + runID.String() }

// labelKeep marks a run kept at its end. It is set on the agent pod, and so on its Sandbox and the Sandbox's template.
const labelKeep = "wardyn.keep"

// keepRun is whether a run is kept at its end: keep on, interactive, and its work folder on a kept drive.
func (d *Driver) keepRun(spec runner.SandboxSpec) bool {
	return d.keepOn() && spec.Interactive &&
		slices.ContainsFunc(spec.Drives, func(m types.DriveMount) bool { return m.Target == scratchWorkPath })
}
