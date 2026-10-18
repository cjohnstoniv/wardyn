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
	// Limits is the runner's limit set, which the server requires: wardyn-runnerd measures the host
	// and proposes it (ProposeRunnerLimits), and the person may change any of it.
	Limits *RunnerLimits `json:"limits"`
	// MachineName is the host's own name (os.Hostname), runner-reported; omitted by an older runner.
	MachineName string `json:"machine_name,omitempty"`
	// Arch is the Docker engine's CPU architecture ("amd64" or "arm64"), runner-reported; omitted by an
	// older runner or when the engine could not be asked.
	Arch string `json:"arch,omitempty"`
}

// RunnerRegistration is safe to return to the anonymous runner; it has no human identity.
type RunnerRegistration struct {
	RunnerID     uuid.UUID   `json:"runner_id"`
	Fingerprint  string      `json:"fingerprint"`
	State        RunnerState `json:"state"`
	OrgURLSHA256 string      `json:"org_url_sha256"`
	// Limits is the limit set the server stored; nil on a runner registered before 0174 until its owner sets one.
	Limits *RunnerLimits `json:"limits"`
	// ProxyImage is the egress-proxy image this runner should run, chosen for the architecture it
	// reported: the register answer only (the claim answer leaves it empty), and empty when the
	// organisation names none.
	ProxyImage string `json:"proxy_image,omitempty"`
}

// RunnerClaimRequest confirms the fingerprint observed on the owner's host.
type RunnerClaimRequest struct {
	Fingerprint string `json:"fingerprint"`
}

// RunnerOrgTokenRequest is POST /runners/org-tokens: a security operator mints a token for an
// organization's runner. Name labels the token in its audit row.
type RunnerOrgTokenRequest struct {
	Name string `json:"name"`
}

// RunnerTrustRequest is PUT /runners/{id}/trust: a security operator's change of a runner's trust class,
// with the reason the audit row keeps.
type RunnerTrustRequest struct {
	TrustClass RunnerTrustClass `json:"trust_class"`
	Reason     string           `json:"reason"`
}
