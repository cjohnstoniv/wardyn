// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"encoding/json"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Who refused an egress request, the values of EgressRefusal.By.
const (
	// EgressRefusedByWardyn: the run proxy denied the request (its policy or a guard).
	EgressRefusedByWardyn = egress.RefusedByWardyn
	// EgressRefusedByOutside: the proxy allowed the host and a peer outside Wardyn refused or reset it.
	EgressRefusedByOutside = egress.RefusedByOutside
	// EgressRefusedByUnknown: the proxy allowed the host and the connection failed with no cause Wardyn can read.
	EgressRefusedByUnknown = egress.RefusedByUnknown
)

// EgressRefusal is the "whose refusal" part of an `egress.deny` audit event's data.
// Reason is a closed token (docs/AUDIT-ACTIONS.md lists them); Layer and LayerName
// ride only By == EgressRefusedByWardyn. It never carries request content.
type EgressRefusal struct {
	By        string `json:"by"`
	Reason    string `json:"refusal_reason"`
	Layer     string `json:"layer,omitempty"`
	LayerName string `json:"layer_name,omitempty"`
}

// EgressRefusalOf reads the refusal off an egress audit event, or nil when the
// event is not an egress.deny or carries none: absent means "not classified",
// which is different from By == EgressRefusedByUnknown.
func EgressRefusalOf(ev types.AuditEvent) *EgressRefusal {
	if ev.Action != "egress.deny" {
		return nil
	}
	var r EgressRefusal
	if json.Unmarshal(ev.Data, &r) != nil || r.By == "" {
		return nil
	}
	return &r
}
