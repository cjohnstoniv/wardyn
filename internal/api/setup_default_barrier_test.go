// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The default_barrier_unset row shows only while no organisation default is set and CC2 or CC3 is offered,
// and names the strongest class offered.
func TestDefaultBarrierUnsetRow(t *testing.T) {
	for _, c := range []struct {
		name    string
		org     types.ConfinementClass
		offered []string
		want    string // the class the detail names; "" means no row
	}{
		{"shown", "", []string{"CC1", "CC3"}, "CC3"},
		{"hidden: organisation default set", types.CC2, []string{"CC1", "CC3"}, ""},
		{"hidden: only CC1 offered", "", []string{"CC1"}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := &upgradeFake{defaults: types.RunnerPoolDefaults{Barrier: c.org}}
			s := &Server{cfg: Config{Store: f}}
			chk, ok := s.defaultBarrierUnsetRow(context.Background(), SetupRunner{ConfinementClasses: c.offered})
			if ok != (c.want != "") {
				t.Fatalf("row shown = %v, want %v (%+v)", ok, c.want != "", chk)
			}
			if !ok {
				return
			}
			if chk.ID != "default_barrier_unset" || chk.Status != "info" || chk.Blocking ||
				!strings.Contains(chk.Detail, "is "+c.want+",") || !strings.Contains(chk.Fix, "Set an organisation default barrier") {
				t.Errorf("row = %+v, want an info row naming %s with the fix", chk, c.want)
			}
		})
	}
}
