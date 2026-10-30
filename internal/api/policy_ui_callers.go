// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// maxUIAppCallers bounds one inbound app's callers list. Callers are fixed at create and ride every
// run.policy.resolve envelope, so the bound keeps a policy from carrying a directory.
const maxUIAppCallers = 32

// validateUIAppCallers is validateUIApps' check of one app's inbound declaration. callers belong to an
// inbound app only; each entry is `user:<sub or email>`, `group:<name>` or `service:<name>`, already in the
// form the resolver matches (lower-cased, trimmed) and named once. `all` is not a caller: an inbound app
// is never open to every signed-in person. Names are shape-checked only, as capability grants are: a name
// nobody holds admits nobody. The ceiling clamp compares entries as strings, so a spelling that is not
// canonical is refused here rather than folded, which would let two spellings of one caller differ
// between a proposal and its ceiling.
func validateUIAppCallers(a types.UIApp) error {
	if len(a.Callers) == 0 {
		return nil
	}
	if !a.Inbound {
		return fmt.Errorf("callers is only meaningful with inbound: true")
	}
	if len(a.Callers) > maxUIAppCallers {
		return fmt.Errorf("callers: %d entries exceeds the maximum of %d", len(a.Callers), maxUIAppCallers)
	}
	seen := make(map[string]bool, len(a.Callers))
	for j, c := range a.Callers {
		if err := validateUIAppCaller(c); err != nil {
			return fmt.Errorf("callers[%d]: %w", j, err)
		}
		if seen[c] {
			return fmt.Errorf("callers[%d]: duplicate caller %q", j, c)
		}
		seen[c] = true
	}
	return nil
}

func validateUIAppCaller(c string) error {
	kind, name, _ := strings.Cut(c, ":")
	switch kind {
	case "user":
		// A user subject keeps non-ASCII (an IdP-issued sub), unfolded, as CanonicalUserSubject does.
		if name == "" || len(name) > maxCapabilityGrantFieldLen || !controlCharFree(name) || strings.ContainsRune(name, ' ') || types.CanonicalUserSubject(name) != name {
			return fmt.Errorf("invalid user caller %q (want user:<sub or email>, lower-case, no surrounding spaces)", c)
		}
	case "group":
		if canon, ok := types.CanonicalGroupSubject(name); !ok || canon != name || len(name) > maxCapabilityGrantFieldLen || !controlCharFree(name) {
			return fmt.Errorf("invalid group caller %q (want group:<name>, printable lower-case ASCII, no surrounding spaces)", c)
		}
	case "service":
		if !serviceNameRE.MatchString(name) {
			return fmt.Errorf("invalid service caller %q (want service:<name>, a service identity name: lower-case letters, digits and dashes, 3-63 chars, starting with a letter)", c)
		}
	default:
		return fmt.Errorf("invalid caller %q (want user:<sub or email>, group:<name> or service:<name>; \"all\" is not a caller)", c)
	}
	return nil
}
