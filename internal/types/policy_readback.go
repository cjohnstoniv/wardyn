// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import "time"

// ProxyStartState is where one proxy start's policy readback stands.
type ProxyStartState string

const (
	// ProxyStartPending is a start whose driver hash or proxy report is not in yet.
	ProxyStartPending ProxyStartState = "pending"
	// ProxyStartEnforced is a start whose proxy reported the hash of the bytes its driver handed it.
	ProxyStartEnforced ProxyStartState = "enforced"
	// ProxyStartUnconfirmed is a legacy start: an older runner or proxy image, which cannot report.
	ProxyStartUnconfirmed ProxyStartState = "unconfirmed"
	// ProxyStartFailed is a start that did not confirm: its proxy is stopped.
	ProxyStartFailed ProxyStartState = "failed"
	// ProxyStartAbandoned is a revive's start whose new proxy never replaced the old one.
	ProxyStartAbandoned ProxyStartState = "abandoned"
	// ProxyStartSuperseded is an older start a newer proxy start replaced before it confirmed.
	ProxyStartSuperseded ProxyStartState = "superseded"
)

// RunPolicyReadback is what the run read shows of its current proxy start. It never carries a hash.
type RunPolicyReadback struct {
	State       ProxyStartState `json:"state"`
	Generation  int             `json:"generation"`
	ConfirmedAt *time.Time      `json:"confirmed_at,omitempty"`
}
