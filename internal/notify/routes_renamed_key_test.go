// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"testing"

	"github.com/google/uuid"
)

// A 0.9 route scoped by "profiles" is refused at parse, never read as an unscoped route (which would
// page for every unit): the strict decode stops on the unknown key, and "units" is the key read now.
func TestParse_RefusesTheOldProfilesRouteKey(t *testing.T) {
	id := uuid.New().String()
	route := func(key string) string {
		return `{` + routeChannels + `,"routes":[{"` + key + `":["` + id + `"],"tiers":[` + tierJSON("0s", `"a"`, "") + `]}]}`
	}
	if _, err := Parse(route("profiles")); err == nil {
		t.Error(`route key "profiles": parsed, want a refusal`)
	}
	if _, err := Parse(route("units")); err != nil {
		t.Errorf(`route key "units": refused (%v), want it to parse`, err)
	}
}
