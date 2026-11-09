// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"fmt"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// MeetWalls is spec narrowed by a person's walls (types.PersonalWallsSpec.Overlay): ApplyOverlay with
// the walls as the overlay, so a knob the walls do not name is inherited and one they name can only
// narrow. A policy that allows all egress meets a walls allowlist as that allowlist, not as nothing,
// because an open run reaches every name the list allows on 80 or 443: those are added to the policy's
// own before the meet (openSeed, as ApplyRestrictionOverlay seeds), so none is dropped as uncovered. An IP
// literal or another port is not added, so it is kept only where the policy's own list names it. Withheld
// grants are not an overlay knob; the caller removes them. A widening the overlay somehow names is
// dropped, never applied.
func MeetWalls(spec types.RunPolicySpec, ov types.CeilingOverlay) (types.RunPolicySpec, error) {
	base := Authority{Ceiling: spec.Clone()}
	if base.Ceiling.AllowAllEgress && ov.AllowedDomains != nil {
		base.Ceiling.AllowedDomains = openSeed(base.Ceiling.AllowedDomains, *ov.AllowedDomains)
	}
	out, _, err := ApplyOverlay(base, Overlay{Ceiling: ov})
	if err != nil {
		return types.RunPolicySpec{}, fmt.Errorf("composer: meet the person's walls: %w", err)
	}
	return out.Ceiling, nil
}
