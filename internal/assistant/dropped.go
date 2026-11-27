// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package assistant

// Why a proposed part was refused outright. Each code is one dropped entry.
const (
	// DropOutsideCeiling is a host the person's limits do not allow.
	DropOutsideCeiling = "outside_ceiling"
	// DropClamped is a field the person's limits cut back.
	DropClamped = "clamped"
	// DropInvalid is a field the save route's validator refuses.
	DropInvalid = "invalid"
	// DropUnknownSection is a part that names no field of the target's body.
	DropUnknownSection = "unknown_section"
	// DropNotGranted is a host or field the person's capability grants leave out, as the save route narrows it.
	DropNotGranted = "not_granted"
)

// Refusal is one save-route refusal of a proposed part: a host or a field, why, and the unit whose limits
// refused it. Held marks a widening the save would hold for approval rather than refuse.
type Refusal struct {
	Host  string
	Field string
	Code  string
	Unit  string
	Held  bool
}

// Dropped is one part a proposal leaves out, as the browser explains it. AdminChangeApprovals is how many
// approvals a change to that unit's limits needs.
type Dropped struct {
	Host                 string `json:"host,omitempty"`
	Field                string `json:"field,omitempty"`
	Code                 string `json:"code"`
	Unit                 string `json:"unit,omitempty"`
	AdminChangeApprovals int    `json:"admin_change_approvals"`
}

// DroppedOf maps refusals to dropped entries, in order: one entry for each outright refusal, none for a held
// widening, which stays in the proposal. approvals is asked once per unit named.
func DroppedOf(refusals []Refusal, approvals func(unit string) int) []Dropped {
	out := []Dropped{}
	asked := map[string]int{}
	for _, r := range refusals {
		if r.Held {
			continue
		}
		n, ok := asked[r.Unit]
		if !ok && approvals != nil {
			n = approvals(r.Unit)
			asked[r.Unit] = n
		}
		out = append(out, Dropped{Host: r.Host, Field: r.Field, Code: r.Code, Unit: r.Unit, AdminChangeApprovals: n})
	}
	return out
}
