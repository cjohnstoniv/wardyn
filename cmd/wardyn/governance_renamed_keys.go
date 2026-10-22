// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"strings"
)

// renamedKeys maps each key a 0.9 governance document spelled to the one 0.10.0 reads.
var renamedKeys = map[string]string{
	"profiles": "units", "assignments": "memberships", "profile_id": "unit_id", "base_profile_id": "parent_id", "profile": "unit",
}

// renamedKeyHint names the new key when the strict decode stopped on a renamed one.
func renamedKeyHint(err error) string {
	for old, now := range renamedKeys {
		if strings.Contains(err.Error(), `unknown field "`+old+`"`) {
			return fmt.Sprintf(" (%q was renamed in 0.10.0: write %q)", old, now)
		}
	}
	return ""
}
