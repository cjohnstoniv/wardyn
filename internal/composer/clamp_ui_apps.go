// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"fmt"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// clampUIApps bounds a proposal's UI apps: the ceiling's list is an allowlist of (name, port) pairs
// when it sets one, no opinion otherwise. Unlike allowed_methods, an empty ceiling is never adopted —
// empty ui_apps means "no UI apps" (RunPolicySpec.UIApps' own doc), so adopting would grant relay
// access never asked for.
//
// Matched on name AND port, since name alone would let a proposal point the ceiling's app at any
// port in the sandbox; path is left as proposed. Inbound survives only where the ceiling's entry is
// inbound too, and callers are cut to the ceiling's (a caller the ceiling does not name is dropped
// with a warning). A ceiling that lists no apps has no opinion on either.
func clampUIApps(proposed, ceiling []types.UIApp, warns *[]string) []types.UIApp {
	if len(ceiling) == 0 || len(proposed) == 0 {
		return proposed
	}
	var kept []types.UIApp
	var dropped, uninbound, uncalled []string
	for _, a := range proposed {
		name := fmt.Sprintf("%s:%d", a.Name, a.Port)
		i := slices.IndexFunc(ceiling, func(c types.UIApp) bool { return c.Name == a.Name && c.Port == a.Port })
		if i < 0 {
			dropped = append(dropped, name)
			continue
		}
		switch c := ceiling[i]; {
		case a.Inbound && !c.Inbound:
			a.Inbound, a.Callers = false, nil
			uninbound = append(uninbound, name)
		case a.Inbound:
			if allowed := slices.DeleteFunc(slices.Clone(a.Callers), func(x string) bool { return !slices.Contains(c.Callers, x) }); len(allowed) != len(a.Callers) {
				a.Callers = allowed
				uncalled = append(uncalled, name)
			}
		}
		kept = append(kept, a)
	}
	if len(dropped) > 0 {
		*warns = append(*warns, fmt.Sprintf("dropped %d ui_app(s) not in operator allowlist: %s", len(dropped), strings.Join(dropped, ",")))
	}
	if len(uninbound) > 0 {
		*warns = append(*warns, fmt.Sprintf("dropped inbound from %d ui_app(s) the operator allowlist does not make inbound: %s", len(uninbound), strings.Join(uninbound, ",")))
	}
	if len(uncalled) > 0 {
		*warns = append(*warns, fmt.Sprintf("dropped callers not in operator allowlist from %d ui_app(s): %s", len(uncalled), strings.Join(uncalled, ",")))
	}
	return kept
}
