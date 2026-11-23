// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

const (
	reasonAgentRulesInvalid         = "agent_rules_invalid"          // agent rules that name a widening value, a knob Wardyn does not enforce for them yet, or nothing, or that no longer pass the walls rules
	reasonAgentRulesConflict        = "agent_rules_conflict"         // the run cannot meet an agent's rules for the harness's own walls
	reasonAgentRulesHarnessUnknown  = "agent_rules_harness_unknown"  // agent rules for a harness the catalogue does not have
	reasonAgentRulesVersionConflict = "agent_rules_version_conflict" // a write or delete named a version that is not the stored one
	reasonAgentRulesUnavailable     = "agent_rules_unavailable"      // this server's store keeps no agent rules
)
