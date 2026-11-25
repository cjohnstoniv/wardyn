// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import "time"

// AgentRules is an organisation's rules for one agent harness (#2949): one narrow-only walls document met
// into every run that includes the harness, wherever the run is placed, and re-asserted at the run's seal.
// Spec is the walls a person may set for themselves (PersonalWallsSpec), so it can only narrow. InnerWalls is
// a requirement on the harness's own walls (#2822); it never names off or none.
type AgentRules struct {
	Harness    string             `json:"harness"`
	Spec       PersonalWallsSpec  `json:"spec"`
	InnerWalls *HarnessInnerWalls `json:"inner_walls,omitempty"`
	Version    int                `json:"version"`
	UpdatedBy  string             `json:"updated_by,omitempty"`
	UpdatedAt  time.Time          `json:"updated_at"`
}

// AgentRulesWrite is the body of PUT /agent-rules/{harness}. Version is the version the write replaces: 0
// creates the harness's rules, and any other value must match the stored row.
type AgentRulesWrite struct {
	Spec       PersonalWallsSpec  `json:"spec"`
	InnerWalls *HarnessInnerWalls `json:"inner_walls,omitempty"`
	Version    int                `json:"version"`
}

// Empty reports whether the rules name nothing, so no run is narrowed by them.
func (r AgentRules) Empty() bool {
	return r.Spec.Empty() && (r.InnerWalls == nil || *r.InnerWalls == HarnessInnerWalls{})
}
