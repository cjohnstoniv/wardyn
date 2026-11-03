// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

// UngovernedNotice is POST /runs/policy-preview's `ungoverned` block. It is present
// only when the caller's own runs stand outside governance (the admin token, local
// mode, or an admin while the install leaves admin runs ungoverned), and says what
// a member of the caller's type would be held to instead.
type UngovernedNotice struct {
	// NoPerson says nobody is signed in (the admin token, local mode), so there is
	// no profile to resolve and Unit and Limits are empty.
	NoPerson bool `json:"no_person,omitempty"`
	// NoProfile says a person is signed in but their type resolves to no governance
	// unit (the install's default has none to name), so there is nothing to compare with.
	NoProfile bool `json:"no_unit,omitempty"`
	// Unavailable says the comparison could not be read; it is never an empty limits list.
	Unavailable bool `json:"unavailable,omitempty"`
	// Unit is the governance unit the caller's type resolves to.
	Unit string `json:"unit,omitempty"`
	// Limits are the unit's headline limits as short plain-English phrases, at most
	// three, in a fixed order.
	Limits []string `json:"limits,omitempty"`
	// More says the unit sets further limits than Limits lists.
	More bool `json:"more,omitempty"`
}
