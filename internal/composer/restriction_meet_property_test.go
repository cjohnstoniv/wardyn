// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer_test

import (
	"math/rand"
	"reflect"
	"testing"
	"testing/quick"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

type restrictionListCase struct {
	spec types.RunPolicySpec
	list []string
}

func (restrictionListCase) Generate(r *rand.Rand, _ int) reflect.Value {
	spec := genAuthority(r).Ceiling
	if r.Intn(2) == 0 {
		spec.AllowAllEgress = true
	}
	return reflect.ValueOf(restrictionListCase{spec: spec, list: subset(r, genDomains)})
}

// A restriction's allowlist never leaves a run more than the list, judged by enforcement: met into a run
// open or closed, the result is closed, reaches no host the list alone does not reach, and reaches nothing
// the run did not.
func TestApplyRestrictionOverlayNeverMoreThanTheList(t *testing.T) {
	err := quick.Check(func(c restrictionListCase) bool {
		list := c.list
		got, _, err := composer.ApplyRestrictionOverlay(composer.Authority{Ceiling: c.spec}, composer.Overlay{Ceiling: types.RestrictionSpec{AllowedDomains: &list}.Overlay()})
		if err != nil {
			t.Logf("unexpected refusal: %v", err)
			return false
		}
		if got.Ceiling.AllowAllEgress {
			t.Logf("spec %s\nlist %v\nthe run is still open", canon(composer.Authority{Ceiling: c.spec}), list)
			return false
		}
		listed := reach(types.RunPolicySpec{AllowedDomains: list})
		for k := range reach(got.Ceiling) {
			if !listed[k] {
				t.Logf("spec %s\nlist %v\nreached outside the list: %s", canon(composer.Authority{Ceiling: c.spec}), list, k)
				return false
			}
		}
		if why := narrower(composer.Authority{Ceiling: c.spec}, got); why != "" {
			t.Logf("spec %s\nlist %v\ngot %s\nwhy: %s", canon(composer.Authority{Ceiling: c.spec}), list, canon(got), why)
			return false
		}
		return true
	}, quickCfg)
	if err != nil {
		t.Fatal(err)
	}
}
