// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package placement

import "net/http"

// Reason is a machine-readable refusal reason of local placement and the
// runner link (design §12.3). The set is closed.
type Reason string

const (
	ReasonPlacementRequired            Reason = "placement_required"
	ReasonPlacementUnavailable         Reason = "placement_unavailable"
	ReasonPlacementDenied              Reason = "placement_denied"
	ReasonPlacementCredential          Reason = "placement_credential"
	ReasonPlacementTrustedOutput       Reason = "placement_trusted_output"
	ReasonPlacementComponentSelfDefine Reason = "placement_component_self_defined"
	ReasonPlacementCapability          Reason = "placement_capability"
	ReasonPlacementLocalPath           Reason = "placement_local_path"
	ReasonRunnerOffline                Reason = "runner_offline"
	ReasonRunnerAmbiguous              Reason = "runner_ambiguous"
	ReasonRunnerNotFound               Reason = "runner_not_found"
	ReasonRunnerUnclaimed              Reason = "runner_unclaimed"
	ReasonRunnerRevoked                Reason = "runner_revoked"
	ReasonRunnerTokenInvalid           Reason = "runner_token_invalid"
	ReasonRunnerClaimMismatch          Reason = "runner_claim_mismatch"
	ReasonRunnerPostureUnmet           Reason = "runner_posture_unmet"
	ReasonRunnerKeystoreUnavailable    Reason = "runner_keystore_unavailable"
	ReasonRunnerActionPending          Reason = "runner_action_pending"
	ReasonRelayRunnerMismatch          Reason = "relay_runner_mismatch"
	ReasonRelayRequired                Reason = "relay_required"
	ReasonRelayRouteRefused            Reason = "relay_route_refused"
	ReasonDeliveryViaOrg               Reason = "delivery_via_org"
	ReasonDeliveryRunnerResident       Reason = "delivery_runner_resident"
	ReasonDeliveryNotViaOrg            Reason = "delivery_not_via_org"
	ReasonViaOrgDestinationRefused     Reason = "via_org_destination_refused"
	ReasonViaOrgCapExceeded            Reason = "via_org_cap_exceeded"
)

// reasonStatus is the HTTP status each reason answers with.
var reasonStatus = map[Reason]int{
	ReasonPlacementRequired:            http.StatusUnprocessableEntity,
	ReasonPlacementUnavailable:         http.StatusUnprocessableEntity,
	ReasonPlacementDenied:              http.StatusForbidden,
	ReasonPlacementCredential:          http.StatusUnprocessableEntity,
	ReasonPlacementTrustedOutput:       http.StatusUnprocessableEntity,
	ReasonPlacementComponentSelfDefine: http.StatusForbidden,
	ReasonPlacementCapability:          http.StatusUnprocessableEntity,
	ReasonPlacementLocalPath:           http.StatusUnprocessableEntity,
	ReasonRunnerOffline:                http.StatusUnprocessableEntity,
	ReasonRunnerAmbiguous:              http.StatusUnprocessableEntity,
	ReasonRunnerNotFound:               http.StatusNotFound,
	ReasonRunnerUnclaimed:              http.StatusConflict,
	ReasonRunnerRevoked:                http.StatusUnauthorized,
	ReasonRunnerTokenInvalid:           http.StatusUnauthorized,
	ReasonRunnerClaimMismatch:          http.StatusForbidden,
	ReasonRunnerPostureUnmet:           http.StatusUnprocessableEntity,
	ReasonRunnerKeystoreUnavailable:    http.StatusUnprocessableEntity,
	ReasonRunnerActionPending:          http.StatusConflict,
	ReasonRelayRunnerMismatch:          http.StatusForbidden,
	ReasonRelayRequired:                http.StatusForbidden,
	ReasonRelayRouteRefused:            http.StatusForbidden,
	ReasonDeliveryViaOrg:               http.StatusForbidden,
	ReasonDeliveryRunnerResident:       http.StatusForbidden,
	ReasonDeliveryNotViaOrg:            http.StatusForbidden,
	ReasonViaOrgDestinationRefused:     http.StatusForbidden,
	ReasonViaOrgCapExceeded:            http.StatusTooManyRequests,
}

// Status is the HTTP status the reason answers with; 0 for a reason outside the set.
func (r Reason) Status() int { return reasonStatus[r] }

// Valid reports whether r is in the closed set.
func (r Reason) Valid() bool { _, ok := reasonStatus[r]; return ok }

// Reasons lists the closed set, unordered.
func Reasons() []Reason {
	out := make([]Reason, 0, len(reasonStatus))
	for r := range reasonStatus {
		out = append(out, r)
	}
	return out
}
