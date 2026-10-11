// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"github.com/cjohnstoniv/wardyn/internal/types"
	"testing"
)

func TestLocalSelfDefinedComponentsDefaultDenyAndMonotone(t *testing.T) {
	denied := Authority{}
	allowed := Authority{Limits: types.GovernanceLimits{LocalSelfDefinedComponents: true}}
	if Leq(allowed.Ceiling, denied.Ceiling, allowed.Limits, denied.Limits) {
		t.Fatal("permission widened absent base")
	}
	if !Leq(denied.Ceiling, allowed.Ceiling, denied.Limits, allowed.Limits) {
		t.Fatal("permission could not narrow")
	}
	got := mustApply(t, allowed, limitsOverlay(types.LimitsOverlay{LocalSelfDefinedComponents: ptr(false)}))
	if got.Limits.LocalSelfDefinedComponents {
		t.Fatal("overlay did not remove permission")
	}
	got = mustApply(t, allowed, limitsOverlay(types.LimitsOverlay{}))
	if !got.Limits.LocalSelfDefinedComponents {
		t.Fatal("absent overlay lost permission")
	}
	wantOverlayErr(t, ValidateOverlay(denied, limitsOverlay(types.LimitsOverlay{LocalSelfDefinedComponents: ptr(true)})), ReasonOverlayInvalid, "local_self_defined_components")
}
