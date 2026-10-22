// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"strings"
)

// refuseRenamedGovernanceEnv refuses the 0.9 name of WARDYN_GOVERNANCE_PROFILE_PUT_EXISTING_ONLY. Nothing
// reads the old name, so a leftover would silently let a PUT create a unit at an id it names again.
func refuseRenamedGovernanceEnv(environ []string) error {
	const old = "WARDYN_GOVERNANCE_PROFILE_PUT_EXISTING_ONLY"
	for _, kv := range environ {
		if name, value, _ := strings.Cut(kv, "="); name == old && value != "" {
			return fmt.Errorf("refusing to start: %s was renamed in 0.10.0 and the old name is no longer read, so the setting would be silently lost. "+
				"Set WARDYN_GOVERNANCE_UNIT_PUT_EXISTING_ONLY instead and unset %s", old, old)
		}
	}
	return nil
}
