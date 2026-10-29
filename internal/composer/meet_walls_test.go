// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer_test

import (
	"math/rand"
	"reflect"
	"slices"
	"testing"
	"testing/quick"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// genWallsAllowed is what a person may write as an allowed site: a DNS name or a "*." wildcard, never an
// IP literal or a port (the write door refuses both).
var genWallsAllowed = []string{
	"*.corp.example", "corp.example", "git.corp.example", "api.vendor.example", "pypi.org", "*.pypi.org",
	"*.example", "x.y.corp.example", "*.y.corp.example",
}

type wallsCase struct {
	spec  types.RunPolicySpec
	walls types.PersonalWallsSpec
}

func (wallsCase) Generate(r *rand.Rand, _ int) reflect.Value {
	w := types.PersonalWallsSpec{}
	if r.Intn(2) == 0 {
		w.AllowedDomains = new(subset(r, genWallsAllowed))
	}
	if r.Intn(2) == 0 {
		w.DeniedDomains = new(subset(r, genDomains[:8]))
	}
	if r.Intn(4) == 0 {
		w.AllowAllEgress = new(false)
	}
	w.FirstUseApproval = some(r, pick(r, []types.FirstUseMode{types.FirstUseAlwaysDeny, types.FirstUseDenyWithReview, types.FirstUseWaitForReview}))
	w.MinConfinementClass = some(r, pick(r, []types.ConfinementClass{types.CC1, types.CC2, types.CC3}))
	if r.Intn(2) == 0 {
		w.Desktop = &types.DesktopControls{}
		for _, d := range []*types.TransferRule{&w.Desktop.PasteIn, &w.Desktop.CopyOut, &w.Desktop.Upload, &w.Desktop.Download} {
			if r.Intn(2) == 0 {
				*d = types.TransferBlocked
			}
		}
	}
	return reflect.ValueOf(wallsCase{spec: genAuthority(r).Ceiling, walls: w})
}

// Walls only narrow, judged by enforcement: every host the met policy reaches the original reached, none
// the walls deny, and none outside the walls' allowlist when they name one (design T4 §2.3).
func TestMeetWallsOnlyNarrows(t *testing.T) {
	err := quick.Check(func(c wallsCase) bool {
		got, err := composer.MeetWalls(c.spec, c.walls.Overlay())
		if err != nil {
			t.Logf("unexpected refusal: %v", err)
			return false
		}
		if why := narrower(composer.Authority{Ceiling: c.spec}, composer.Authority{Ceiling: got}); why != "" {
			t.Logf("spec %s\nwalls %+v\ngot %s\nwhy: %s", canon(composer.Authority{Ceiling: c.spec}), c.walls, canon(composer.Authority{Ceiling: got}), why)
			return false
		}
		deniedOnly := reach(types.RunPolicySpec{AllowAllEgress: true, DeniedDomains: derefList(c.walls.DeniedDomains)})
		listedOnly := reach(types.RunPolicySpec{AllowedDomains: derefList(c.walls.AllowedDomains)})
		for k := range reach(got) {
			if len(k) > 7 && k[:7] == "literal" {
				continue // an exact entry the spec already held; judged by narrower
			}
			if !deniedOnly[k] {
				t.Logf("spec %s\nwalls %+v\nreached a denied site: %s", canon(composer.Authority{Ceiling: c.spec}), c.walls, k)
				return false
			}
			if c.walls.AllowedDomains != nil && !listedOnly[k] {
				t.Logf("spec %s\nwalls %+v\nreached outside the allowlist: %s", canon(composer.Authority{Ceiling: c.spec}), c.walls, k)
				return false
			}
		}
		if c.walls.AllowedDomains != nil && got.AllowAllEgress {
			t.Logf("walls named an allowlist and the run is still open: %+v", c.walls)
			return false
		}
		if c.walls.MinConfinementClass != nil && got.MinConfinementClass.Rank() < c.walls.MinConfinementClass.Rank() {
			return false
		}
		if c.walls.Desktop != nil && !blocksWhatWallsBlock(*c.walls.Desktop, got.Desktop.Effective()) {
			t.Logf("desktop: spec %+v walls %+v got %+v", c.spec.Desktop, *c.walls.Desktop, got.Desktop.Effective())
			return false
		}
		return true
	}, quickCfg)
	if err != nil {
		t.Fatal(err)
	}
}

// blocksWhatWallsBlock: every direction the walls block is blocked in the result.
func blocksWhatWallsBlock(walls, got types.DesktopControls) bool {
	for _, d := range []struct{ w, g types.TransferRule }{
		{walls.PasteIn, got.PasteIn}, {walls.CopyOut, got.CopyOut}, {walls.Upload, got.Upload}, {walls.Download, got.Download},
	} {
		if d.w == types.TransferBlocked && d.g != types.TransferBlocked {
			return false
		}
	}
	return true
}

func derefList(p *[]string) []string {
	if p == nil {
		return nil
	}
	return *p
}

// A walls allowlist over an open policy leaves exactly the listed sites, where a plain intersection with the
// open policy's empty list would leave nothing.
func TestMeetWallsOpenPolicyMeetsTheAllowlistAsTheAllowlist(t *testing.T) {
	spec := types.RunPolicySpec{AllowAllEgress: true}
	w := types.PersonalWallsSpec{AllowedDomains: &[]string{"pypi.org", "*.corp.example"}}
	got, err := composer.MeetWalls(spec, w.Overlay())
	if err != nil {
		t.Fatal(err)
	}
	if got.AllowAllEgress || !slices.Equal(got.AllowedDomains, []string{"*.corp.example", "pypi.org"}) {
		t.Fatalf("got open=%v allowed=%v, want closed with the walls' list", got.AllowAllEgress, got.AllowedDomains)
	}
}

// An empty allowlist is default-deny beyond what the run's setup adds later, not "no list".
func TestMeetWallsEmptyAllowlistClosesAnOpenPolicy(t *testing.T) {
	got, err := composer.MeetWalls(types.RunPolicySpec{AllowAllEgress: true}, types.PersonalWallsSpec{AllowedDomains: &[]string{}}.Overlay())
	if err != nil {
		t.Fatal(err)
	}
	if got.AllowAllEgress || len(got.AllowedDomains) != 0 {
		t.Fatalf("got open=%v allowed=%v, want closed and empty", got.AllowAllEgress, got.AllowedDomains)
	}
}

// Walls that name nothing change nothing.
func TestMeetWallsEmptyIsTheIdentity(t *testing.T) {
	err := quick.Check(func(c wallsCase) bool {
		got, err := composer.MeetWalls(c.spec, types.PersonalWallsSpec{}.Overlay())
		return err == nil && narrower(composer.Authority{Ceiling: got}, composer.Authority{Ceiling: c.spec}) == "" &&
			narrower(composer.Authority{Ceiling: c.spec}, composer.Authority{Ceiling: got}) == ""
	}, quickCfg)
	if err != nil {
		t.Fatal(err)
	}
}

// A closed policy is never opened by walls, and Leq proves the met policy within it.
func TestMeetWallsIsWithinAClosedSpec(t *testing.T) {
	err := quick.Check(func(c wallsCase) bool {
		c.spec.AllowAllEgress = false
		got, err := composer.MeetWalls(c.spec, c.walls.Overlay())
		return err == nil && !got.AllowAllEgress && composer.Leq(got, c.spec, types.GovernanceLimits{}, types.GovernanceLimits{})
	}, quickCfg)
	if err != nil {
		t.Fatal(err)
	}
}
