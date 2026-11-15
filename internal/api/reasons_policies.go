// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// /api/v1/policies (policies.go).
const (
	reasonPolicyRequestInvalid    = "policy_request_invalid"     // the create/update body fails decodePolicyRequest
	reasonPolicySecretRefsInvalid = "policy_secret_refs_invalid" // a secret reference in the spec fails shape validation
	reasonPolicyNameConflict      = "policy_name_conflict"       // a policy by that name already exists
	reasonPolicyStale             = "policy_stale"               // the policy was saved after the updated_at the update sent
	reasonPolicyRevisionRequired  = "policy_revision_required"   // the update named no expected_updated_at
	// Shared by the policy CRUD door, POST /runs' inline_policy and a launch
	// preset's inline_policy: the spec's
	// azure_devops_capabilities names something the catalogue cannot grant.
	reasonADOCapabilityUnknown = "ado_capability_unknown"
	// Review's mirror of the dispatch refusal: a member's azure_devops_capabilities
	// that leaves nothing standing (POST /runs/preflight).
	reasonADOCapabilitiesNonePermitted = "ado_capabilities_none_permitted"
	// The bounded list reaches past the provider row's capability_ceiling.
	reasonADOCapabilitiesAboveCeiling = "ado_capabilities_above_ceiling"
)
