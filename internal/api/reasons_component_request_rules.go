// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// Request rules on custom components: the same closed set and convention as reasons.go, in a file of its own beside
// components_request_rules.go. TestReasonDocsMatchReasonsGo reads this file too.
const (
	reasonComponentRequestRulesOff   = "component_request_rules_off"   // a save adds or changes request_rules, or a run attaches a component with them, while features.component_request_rules is off
	reasonComponentRequestRulesLimit = "component_request_rules_limit" // a run's host rule sets, one per component with request_rules beside its policy's, would pass types.MaxHostRuleSets
)
