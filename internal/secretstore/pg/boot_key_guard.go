// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package pg

import (
	"fmt"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
)

// refuseMixedBootKeys refuses a rewrap that finds a boot key under any key but
// platformID while another is already under it. Every boot key a wardynd wrote
// or moved sits under the platform key once one does, so the other was written
// where it sits after the move, by whoever holds that key and can write the
// table. It changes nothing and names the rows.
func refuseMixedBootKeys(platformID string, all []envelope) error {
	return refuseMixed(platformID, all,
		func(id string) bool { return id == platformID },
		func(id string) bool { return id != platformID })
}

// refuseMixed is that check for a given split of the boot keys' kek_ids: it
// refuses when some boot key is under a key moved accepts and another is under
// a key planted accepts. A key service writing beside a platform file leaves
// boot keys under the service or the file key legitimately, and only a key the
// age key alone derives is what its holder can plant.
func refuseMixed(platformID string, all []envelope, moved, planted func(kekID string) bool) error {
	var under bool
	var other []string
	for _, e := range all {
		if secretstore.Kind(e.ownedBy, e.name) != "platform" {
			continue
		}
		switch {
		case moved(e.kekID):
			under = true
		case planted(e.kekID):
			other = append(other, fmt.Sprintf("%s under %q", rowRef(e.ownedBy, e.name), e.kekID))
		}
	}
	if !under || len(other) == 0 {
		return nil
	}
	return &refusal{ErrMixedBootKeys, fmt.Sprintf("pg secretstore: rewrap REFUSED (nothing changed): boot keys are already under the platform key %q, "+
		"yet %s sit under another key, a mixed state no run of wardynd leaves. These rows were not written by Wardyn: "+
		"find out who wrote them (updated_at, the audit log, database access logs) and restore the boot keys from a backup if they are forged",
		platformID, strings.Join(other, ", "))}
}
