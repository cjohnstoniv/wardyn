// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import "encoding/json"

// Audit rows are immutable, so rows a 0.9 daemon wrote keep the spellings the 0.10.0 rename retired:
// the unit's name under "profile" and the provenance layer "profile". The readers below accept
// them, so an old run's policy view still explains itself.

// legacyLayerProfile is the layer name old rows carry for what is now a unit.
const legacyLayerProfile = "profile"

// legacyUnitName is the unit name an old audit row carries under "profile", or "".
func legacyUnitName(data []byte) string {
	var row struct {
		Profile string `json:"profile"`
	}
	if json.Unmarshal(data, &row) != nil {
		return ""
	}
	return row.Profile
}
