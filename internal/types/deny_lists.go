// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"fmt"

	"github.com/google/uuid"
)

// MaxDenyListEntries bounds each unit deny list (GovernanceLimits.DenyComponents, DenyModelProviders).
const MaxDenyListEntries = 200

// ValidDenyIDs checks a unit deny list: every entry is a canonical lower-case id (a stored
// component's id, or a model provider's uid), listed once, and the list is no longer than
// MaxDenyListEntries. A name or a mistyped id would deny nothing and read as a wall, so it is
// refused at write. The message names no field; the caller adds it. An empty list is valid.
func ValidDenyIDs(list []string) error {
	if len(list) > MaxDenyListEntries {
		return fmt.Errorf("at most %d entries", MaxDenyListEntries)
	}
	seen := make(map[string]bool, len(list))
	for i, id := range list {
		switch u, err := uuid.Parse(id); {
		case err != nil || u.String() != id:
			return fmt.Errorf("entry %d (%q) is not an id — use the lower-case id from the component or provider", i+1, id)
		case seen[id]:
			return fmt.Errorf("%q is listed twice", id)
		}
		seen[id] = true
	}
	return nil
}
