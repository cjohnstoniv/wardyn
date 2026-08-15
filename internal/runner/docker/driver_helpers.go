// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"fmt"
	"os"
	"strings"

	"github.com/google/uuid"
	"github.com/moby/moby/api/types/container"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// statusFromInspect maps Docker container state to a Wardyn RunState.
func statusFromInspect(insp container.InspectResponse) runner.Status {
	st := runner.Status{State: types.RunRunning}
	if insp.State == nil {
		st.State = types.RunStopped
		return st
	}
	s := insp.State
	switch {
	case s.Running:
		st.State = types.RunRunning
	case s.OOMKilled:
		st.State = types.RunFailed
		st.Message = "OOM killed"
	case s.Status == "created":
		st.State = types.RunStarting
	case s.Status == "exited", s.Status == "dead":
		ec := s.ExitCode
		st.ExitCode = &ec
		if ec == 0 {
			st.State = types.RunStopped
		} else {
			st.State = types.RunFailed
			st.Message = fmt.Sprintf("exit code %d", ec)
		}
	default:
		st.State = types.RunStopped
	}
	if s.Error != "" {
		st.Message = strings.TrimSpace(st.Message + " " + s.Error)
	}
	return st
}

// envSlice converts a non-secret env map to Docker's KEY=VALUE slice form.
// Secrets never pass here (invariant 1) — the spec contract forbids it.
func envSlice(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// proxyEnv builds the environment handed to the wardyn-proxy sidecar: the
// full proxy config (incl. the run's egress policy — a proxy without a policy
// fails closed and the sandbox has no egress) as one JSON env var, plus the
// individual values for operator inspection. The run token is verifiable but
// not a usable secret outside the platform; env visibility is part of the
// documented daemon-trust tradeoff. A thin wrapper over runner.BuildProxyConfig
// (the substrate-agnostic field-mapping + marshal core, hoisted so a k8s
// substrate builds byte-identical sidecar config): this function adds only the
// docker-Env-slice shape and the operator-knob forwarding below.
func proxyEnv(runID uuid.UUID, pc runner.ProxyConfig, port int) []string {
	cfgJSON, _ := runner.BuildProxyConfig(runID, pc, port)
	env := []string{
		"WARDYN_PROXY_CONFIG_JSON=" + string(cfgJSON),
		"WARDYN_RUN_ID=" + runID.String(),
		"WARDYN_CONTROL_PLANE_URL=" + pc.ControlPlaneURL,
	}
	// Operator knobs the sidecar reads from ITS environment: forward them from
	// wardynd's environment when set, else they are dead on the docker runner
	// (host-run and custom-image proxies read their own env directly).
	for _, k := range []string{"WARDYN_LLM_SCAN", "WARDYN_GIT_BROKER_ENFORCE_BRANCH_NS"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// parseRunID parses a run-id label back to a UUID.
func parseRunID(s string) (uuid.UUID, error) { return uuid.Parse(s) }

// isNotRunning detects the daemon's "container not running" error so Kill can
// proceed to force-remove an already-stopped container.
func isNotRunning(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "is not running")
}
