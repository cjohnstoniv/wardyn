// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerpool

import (
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// sitePools is a catalogue with a site pool, a person's default, the organisation's default and an
// unrelated pool, each active.
func sitePools() (site, personal, org, other types.RunnerPool, all []types.RunnerPool) {
	site = pool("Office", types.RunnerPoolOrganization, types.RunnerPoolActive)
	personal = pool("Mine", types.RunnerPoolOrganization, types.RunnerPoolActive)
	org = pool("Shared", types.RunnerPoolOrganization, types.RunnerPoolActive)
	other = pool("Other", types.RunnerPoolOrganization, types.RunnerPoolActive)
	return site, personal, org, other, []types.RunnerPool{site, personal, org, other}
}

// An explicit pool beats the site default, the site default beats the person's and the organisation's,
// and with no site the order is today's.
func TestResolveOrderExplicitSitePersonalOrg(t *testing.T) {
	site, personal, org, other, all := sitePools()
	pd, od := types.RunnerPoolDefaults{PoolID: idp(personal)}, types.RunnerPoolDefaults{PoolID: idp(org)}
	for _, tc := range []struct {
		name     string
		choice   Choice
		personal types.RunnerPoolDefaults
		want     types.RunnerPool
		sel      types.RunnerPoolSelection
		pref     SitePreference
	}{
		{"explicit beats the site", Choice{PoolID: idp(other), SiteDefault: idp(site)}, pd, other, types.RunnerPoolSelectedExplicit, ""},
		{"the site beats the person's default", Choice{SiteDefault: idp(site)}, pd, site, types.RunnerPoolSelectedSite, SiteMet},
		{"the site beats the organisation's default", Choice{SiteDefault: idp(site)}, types.RunnerPoolDefaults{}, site, types.RunnerPoolSelectedSite, SiteMet},
		{"no site: the person's default", Choice{}, pd, personal, types.RunnerPoolSelectedPersonal, ""},
		{"no site: the organisation's default", Choice{}, types.RunnerPoolDefaults{}, org, types.RunnerPoolSelectedOrg, ""},
	} {
		got, pref, ref := ResolveSite(all, tc.choice, tc.personal, od)
		if ref != nil || got.ID != tc.want.ID || got.Selection != tc.sel || pref != tc.pref {
			t.Errorf("%s: got %s via %s (%q) %+v, want %s via %s (%q)", tc.name, got.Name, got.Selection, pref, ref, tc.want.Name, tc.sel, tc.pref)
		}
		plain, plainRef := Resolve(all, tc.choice, tc.personal, od)
		if plain != got || (plainRef == nil) != (ref == nil) {
			t.Errorf("%s: Resolve = %+v %+v, ResolveSite = %+v %+v", tc.name, plain, plainRef, got, ref)
		}
	}
}

// A site default that is switched off or deleted is passed over to the next default, never refused.
func TestResolveSiteDefaultDisabledFallsThrough(t *testing.T) {
	_, personal, org, _, all := sitePools()
	for _, st := range []types.RunnerPoolState{types.RunnerPoolDisabled, types.RunnerPoolDeleted} {
		site := pool("Office", types.RunnerPoolOrganization, st)
		cat := append(slices.Clone(all), site)
		got, pref, ref := ResolveSite(cat, Choice{SiteDefault: idp(site)}, types.RunnerPoolDefaults{PoolID: idp(personal)}, types.RunnerPoolDefaults{PoolID: idp(org)})
		if ref != nil || got.ID != personal.ID || got.Selection != types.RunnerPoolSelectedPersonal || pref != SiteUnusable {
			t.Errorf("%s site default: got %s via %s (%q) %+v, want the person's default, unusable", st, got.Name, got.Selection, pref, ref)
		}
		got, pref, ref = ResolveSite(cat, Choice{SiteDefault: idp(site)}, types.RunnerPoolDefaults{}, types.RunnerPoolDefaults{PoolID: idp(org)})
		if ref != nil || got.ID != org.ID || got.Selection != types.RunnerPoolSelectedOrg || pref != SiteUnusable {
			t.Errorf("%s site default: got %s via %s (%q) %+v, want the organisation's default, unusable", st, got.Name, got.Selection, pref, ref)
		}
	}
}

// A site grants nothing: a site default outside the caller's catalogue is passed over as if it were not
// there. With no default behind it the answer is today's "choose a pool", never a refusal that names a
// default the caller cannot see.
func TestResolveSiteDefaultOutsideCatalogueFallsThrough(t *testing.T) {
	site, personal, org, _, all := sitePools()
	without := slices.DeleteFunc(slices.Clone(all), func(p types.RunnerPool) bool { return p.ID == site.ID })
	c := Choice{SiteDefault: idp(site)}

	got, pref, ref := ResolveSite(without, c, types.RunnerPoolDefaults{PoolID: idp(personal)}, types.RunnerPoolDefaults{PoolID: idp(org)})
	if ref != nil || got.ID != personal.ID || pref != SiteUnusable {
		t.Errorf("outside the catalogue: got %s (%q) %+v, want the person's default, unusable", got.Name, pref, ref)
	}
	_, pref, ref = ResolveSite(without, c, types.RunnerPoolDefaults{}, types.RunnerPoolDefaults{})
	if ref == nil || ref.Reason != ReasonRequired || pref != SiteUnusable {
		t.Fatalf("outside the catalogue, no default: %+v (%q), want %s", ref, pref, ReasonRequired)
	}
	if ref.Reason == ReasonDefaultUnavailable || ref.Message() != RequiredMsg() {
		t.Errorf("an unusable site default was answered as a default refusal: %q", ref.Message())
	}
	// A person's default that is gone is still refused as today, after the site was passed over.
	gone := pool("Gone", types.RunnerPoolOrganization, types.RunnerPoolDeleted)
	_, _, ref = ResolveSite(without, c, types.RunnerPoolDefaults{PoolID: idp(gone)}, types.RunnerPoolDefaults{PoolID: idp(org)})
	if ref == nil || ref.Reason != ReasonDefaultUnavailable || ref.Source != types.RunnerPoolSelectedPersonal {
		t.Errorf("a gone personal default after an unusable site: %+v, want default_unavailable (personal)", ref)
	}
}

// A run held to one runner skips the site step: the site default is not weighed, and the answer is the
// one with no site at all.
func TestResolveSitePinnedSkipsTheSite(t *testing.T) {
	site, personal, org, _, all := sitePools()
	pd, od := types.RunnerPoolDefaults{PoolID: idp(personal)}, types.RunnerPoolDefaults{PoolID: idp(org)}
	got, pref, ref := ResolveSite(all, Choice{SiteDefault: idp(site), Pinned: true}, pd, od)
	want, wantRef := Resolve(all, Choice{}, pd, od)
	if ref != nil || wantRef != nil || got != want || pref != SitePinned {
		t.Errorf("pinned: got %+v (%q) %+v, want %+v, pinned", got, pref, ref, want)
	}
	if _, pref, _ := ResolveSite(all, Choice{PoolID: idp(site), Pinned: true}, pd, od); pref != "" {
		t.Errorf("an explicit pool on a pinned run reports %q; the site step never ran", pref)
	}
}

// The same inputs give the same pool, whatever the catalogue's order and however often it is asked.
func TestResolveSiteDeterministic(t *testing.T) {
	site, personal, org, _, all := sitePools()
	pd, od := types.RunnerPoolDefaults{PoolID: idp(personal)}, types.RunnerPoolDefaults{PoolID: idp(org)}
	missing := uuid.New()
	for _, c := range []Choice{{SiteDefault: idp(site)}, {SiteDefault: &missing}, {SiteDefault: idp(site), Pinned: true}} {
		first, firstPref, firstRef := ResolveSite(all, c, pd, od)
		for range 50 {
			cat := slices.Clone(all)
			rand.Shuffle(len(cat), func(i, j int) { cat[i], cat[j] = cat[j], cat[i] })
			got, pref, ref := ResolveSite(cat, c, pd, od)
			if got != first || pref != firstPref || (ref == nil) != (firstRef == nil) {
				t.Fatalf("%+v: got %+v (%q), first %+v (%q)", c, got, pref, first, firstPref)
			}
		}
	}
}
