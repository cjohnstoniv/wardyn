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
}

// RunnerRegistration is safe to return to the anonymous runner; it has no human identity.
type RunnerRegistration struct {
	RunnerID     uuid.UUID   `json:"runner_id"`
	Fingerprint  string      `json:"fingerprint"`
	State        RunnerState `json:"state"`
	OrgURLSHA256 string      `json:"org_url_sha256"`
}

// RunnerClaimRequest confirms the fingerprint observed on the owner's host.
type RunnerClaimRequest struct {
	Fingerprint string `json:"fingerprint"`
}
