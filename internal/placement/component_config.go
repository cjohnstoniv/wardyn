// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package placement

import (
	"maps"
	"slices"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// OrgComponentConfigKeys is the sorted set of environment variable names the
// config of org components (never a person's own) dispatches.
func OrgComponentConfigKeys(orgDefs []types.ComponentDefinition) []string {
	set := map[string]struct{}{}
	for _, d := range orgDefs {
		for k := range d.Config {
			set[k] = struct{}{}
		}
	}
	return slices.Sorted(maps.Keys(set))
}

// StripOrgComponentConfig returns env without the variables an org component's
// config set, and the names it left out, sorted. An org component's config is
// operator-authored and dispatched as plain Env, so nothing but its position
// says a value is not meant for a laptop; the laptop-bound copy of a spec loses
// it, as it loses every other operator-class value (design §6.3). env is not
// modified. Callers pass the keys from OrgComponentConfigKeys, and audit what was
// dropped as run.component_config.drop does.
func StripOrgComponentConfig(env map[string]string, orgKeys []string) (kept map[string]string, dropped []string) {
	kept = maps.Clone(env)
	for _, k := range orgKeys {
		if _, ok := kept[k]; ok {
			delete(kept, k)
			dropped = append(dropped, k)
		}
	}
	slices.Sort(dropped)
	return kept, dropped
}
