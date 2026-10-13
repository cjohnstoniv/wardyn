// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import "github.com/google/uuid"

// RunnerTokenRequest names the person an operator is registering a runner for.
type RunnerTokenRequest struct {
	Owner string `json:"owner"`
}

// RunnerRegisterRequest carries only the public half of the runner identity.
type RunnerRegisterRequest struct {
	Token     string `json:"token"`
	PublicKey []byte `json:"public_key"`
	Name      string `json:"name"`
	// MaxRuns is the runner's "Runs at once"; omitted (0), the server stores RunnerMaxRunsDefault.
	MaxRuns int `json:"max_runs,omitempty"`
	// MachineName is the host's own name (os.Hostname), runner-reported; omitted by an older runner.
	MachineName string `json:"machine_name,omitempty"`
}

// RunnerRegistration is safe to return to the anonymous runner; it has no human identity.
type RunnerRegistration struct {
	RunnerID     uuid.UUID   `json:"runner_id"`
	Fingerprint  string      `json:"fingerprint"`
	State        RunnerState `json:"state"`
	OrgURLSHA256 string      `json:"org_url_sha256"`
	MaxRuns      int         `json:"max_runs"`
}

// RunnerClaimRequest confirms the fingerprint observed on the owner's host.
type RunnerClaimRequest struct {
	Fingerprint string `json:"fingerprint"`
}
