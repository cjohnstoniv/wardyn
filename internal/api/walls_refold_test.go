// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// refoldTestCase is one compose fixture: a chain, leaf first, built fresh on every call so no two resolves
// share a row, and the names of the steps composing it hands an observer, top down.
type refoldTestCase struct {
	name  string
	chain func() []types.GovernanceUnit
	steps []string
}

// refoldTestServer is a resolver whose deployment default is govDeployment with a size of its own.
func refoldTestServer() *Server {
	deployment := govDeployment()
	deployment.Resources = &types.ResourceLimits{CPUMillis: 1500, MemoryMiB: 3072}
	return &Server{cfg: Config{DefaultPolicy: deployment}}
}

// refoldTestCases are the governance_compose_test.go fixtures (govDeployment, govProfile, holdProfile,
// limitsProfile) as standalone rows, and chains over them: composed on a standalone root, with an own-walls
// leaf and with a pass-through leaf, composed on Default, a pass-through on Default, a composed row with no
// base, and Default alone.
func refoldTestCases() []refoldTestCase {
	at := func(sec int64) time.Time { return time.Unix(sec, 0).UTC() }
	plainID, holdsID, limitedID := uuid.New(), uuid.New(), uuid.New()
	rootID, midID, leafID, passID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	underID, passDefID, noBaseID, defID := uuid.New(), uuid.New(), uuid.New(), store.DefaultUnitID
	one := func(p *types.GovernanceUnit, id uuid.UUID) func() []types.GovernanceUnit {
		return func() []types.GovernanceUnit {
			row := *p
			row.ID, row.UpdatedAt = id, at(1)
			return []types.GovernanceUnit{row}
		}
	}
	root := func() types.GovernanceUnit {
		return types.GovernanceUnit{ID: rootID, Name: "refold-root", Ceiling: govDeployment(), UpdatedAt: at(1)}
	}
	mid := func() types.GovernanceUnit {
		return types.GovernanceUnit{ID: midID, Name: "refold-mid", ParentID: &rootID, UpdatedAt: at(2), Overlay: &types.CeilingOverlay{
			AllowedDomains: &[]string{"api.anthropic.com", "pypi.org"}, DeniedDomains: &[]string{"corp.internal"}}}
	}
	def := func() types.GovernanceUnit {
		return types.GovernanceUnit{ID: defID, Name: "Default", Kind: types.UnitKindDefault, UpdatedAt: at(3)}
	}
	return []refoldTestCase{
		{"standalone", one(govProfile("refold-plain"), plainID), nil},
		{"standalone with holds", one(holdProfile("refold-holds"), holdsID), nil},
		{"standalone with limits", one(limitsProfile("refold-limited", types.GovernanceLimits{DenyInteractive: true, MaxConcurrentRuns: 3}), limitedID), nil},
		{"composed on a standalone root", func() []types.GovernanceUnit { return []types.GovernanceUnit{mid(), root()} }, []string{"refold-mid"}},
		{"an own-walls leaf below it", func() []types.GovernanceUnit {
			leaf := types.GovernanceUnit{ID: leafID, Name: "refold-leaf", ParentID: &midID, UpdatedAt: at(4),
				Overlay: &types.CeilingOverlay{AllowedDomains: &[]string{"pypi.org"}}}
			return []types.GovernanceUnit{leaf, mid(), root()}
		}, []string{"refold-mid", "refold-leaf"}},
		{"a pass-through leaf below it", func() []types.GovernanceUnit {
			pass := types.GovernanceUnit{ID: passID, Name: "refold-pass", ParentID: &midID, UpdatedAt: at(5),
				Mode: types.UnitModePassThrough, Overlay: &types.CeilingOverlay{}}
			return []types.GovernanceUnit{pass, mid(), root()}
		}, []string{"refold-mid"}},
		{"composed on Default", func() []types.GovernanceUnit {
			under := types.GovernanceUnit{ID: underID, Name: "refold-under-default", ParentID: &defID, UpdatedAt: at(6),
				Overlay: &types.CeilingOverlay{AllowedDomains: &[]string{"api.anthropic.com"}}}
			return []types.GovernanceUnit{under, def()}
		}, []string{"deployment", "refold-under-default"}},
		{"a pass-through on Default", func() []types.GovernanceUnit {
			pass := types.GovernanceUnit{ID: passDefID, Name: "refold-pass-default", ParentID: &defID, UpdatedAt: at(7),
				Mode: types.UnitModePassThrough, Overlay: &types.CeilingOverlay{}}
			return []types.GovernanceUnit{pass, def()}
		}, []string{"deployment"}},
		{"composed with no base", func() []types.GovernanceUnit {
			return []types.GovernanceUnit{{ID: noBaseID, Name: "refold-no-base", UpdatedAt: at(8),
				Overlay: &types.CeilingOverlay{DeniedDomains: &[]string{"corp.internal"}}}}
		}, []string{"deployment", "refold-no-base"}},
		{"Default alone", func() []types.GovernanceUnit { return []types.GovernanceUnit{def()} }, []string{"deployment"}},
	}
}

// refoldTestChain is the named fixture's chain.
func refoldTestChain(t *testing.T, name string) []types.GovernanceUnit {
	t.Helper()
	for _, tc := range refoldTestCases() {
		if tc.name == name {
			return tc.chain()
		}
	}
	t.Fatalf("no compose fixture %q", name)
	return nil
}

// refoldRecorder is an observer that keeps every step it is handed.
func refoldRecorder(steps *[]wallsStep) wallsObserver {
	return func(st wallsStep) { *steps = append(*steps, st) }
}

// TestWallsObserverNilEqualsResolver: each resolver and its observed twin, with no observer and with one,
// return the same result, PathProvenance and Chain included, and the same error.
func TestWallsObserverNilEqualsResolver(t *testing.T) {
	srv := refoldTestServer()
	errCases := []refoldTestCase{
		{name: "an empty chain", chain: func() []types.GovernanceUnit { return nil }},
		{name: "not a clean line", chain: func() []types.GovernanceUnit {
			c := refoldTestChain(t, "composed on a standalone root")
			return []types.GovernanceUnit{c[0], refoldTestChain(t, "Default alone")[0]}
		}},
	}
	for _, tc := range append(refoldTestCases(), errCases...) {
		t.Run(tc.name, func(t *testing.T) {
			want, wantErr := srv.composeChain(tc.chain())
			bare, bareErr := srv.composeChainObserved(tc.chain(), nil)
			var steps []wallsStep
			seen, seenErr := srv.composeChainObserved(tc.chain(), refoldRecorder(&steps))
			if !reflect.DeepEqual(bare, want) || !reflect.DeepEqual(seen, want) {
				t.Errorf("observed twin differs from composeChain\n want %+v\n nil  %+v\n obs  %+v", want, bare, seen)
			}
			if fmt.Sprint(bareErr) != fmt.Sprint(wantErr) || fmt.Sprint(seenErr) != fmt.Sprint(wantErr) {
				t.Errorf("errors differ: want %v, nil observer %v, observer %v", wantErr, bareErr, seenErr)
			}
		})
	}

	ctx := context.Background()
	unit, err := srv.composeChain(refoldTestChain(t, "composed on a standalone root"))
	if err != nil {
		t.Fatal(err)
	}
	ceiling := func() governanceCeiling {
		return governanceCeiling{Spec: unit.Ceiling.Clone(), Limits: unit.Limits, Unit: unit}
	}
	at := time.Unix(9, 0).UTC()
	aID, bID := uuid.New(), uuid.New()
	list := func() []types.Restriction {
		return []types.Restriction{
			{ID: aID, Name: "refold-a", Spec: types.RestrictionSpec{DeniedDomains: &[]string{"deny.example"}}, UpdatedAt: at},
			{ID: bID, Name: "refold-b", Spec: types.RestrictionSpec{AllowedDomains: &[]string{"api.anthropic.com"}}, UpdatedAt: at},
		}
	}
	t.Run("restrictions", func(t *testing.T) {
		want, wantErr := srv.withRestrictions(ctx, ceiling(), list())
		bare, bareErr := srv.withRestrictionsObserved(ctx, ceiling(), list(), nil)
		var steps []wallsStep
		seen, seenErr := srv.withRestrictionsObserved(ctx, ceiling(), list(), refoldRecorder(&steps))
		if wantErr != nil || bareErr != nil || seenErr != nil {
			t.Fatalf("errors: %v %v %v", wantErr, bareErr, seenErr)
		}
		if !reflect.DeepEqual(bare, want) || !reflect.DeepEqual(seen, want) {
			t.Errorf("observed twin differs from withRestrictions\n want %+v\n nil  %+v\n obs  %+v", want, bare, seen)
		}
		if len(steps) != 2 || steps[0].ID != aID || steps[1].ID != bID || steps[1].Kind != wallsRefoldRestriction {
			t.Errorf("steps = %+v, want one restriction step per restriction, in list order", steps)
		}
	})
	stamp := time.Unix(10, 0).UTC()
	for _, b := range []types.Boundary{
		{Mode: types.BoundaryHardLimits, UpdatedAt: &stamp, Spec: types.BoundarySpec{
			AllowedDomains: &[]string{"api.anthropic.com"}, DeniedDomains: &[]string{"evil.example"}}},
		{Mode: types.BoundaryNoLimits, UpdatedAt: &stamp, Spec: types.BoundarySpec{AllowedDomains: &[]string{"api.anthropic.com"}}},
	} {
		t.Run("boundary "+string(b.Mode), func(t *testing.T) {
			want, wantErr := boundCeiling(b, ceiling())
			bare, bareErr := boundCeilingObserved(b, ceiling(), nil)
			var steps []wallsStep
			seen, seenErr := boundCeilingObserved(b, ceiling(), refoldRecorder(&steps))
			if wantErr != nil || bareErr != nil || seenErr != nil {
				t.Fatalf("errors: %v %v %v", wantErr, bareErr, seenErr)
			}
			if !reflect.DeepEqual(bare, want) || !reflect.DeepEqual(seen, want) {
				t.Errorf("observed twin differs from boundCeiling\n want %+v\n nil  %+v\n obs  %+v", want, bare, seen)
			}
			wantSteps := 0
			if b.HardLimits() {
				wantSteps = 1
			}
			if len(steps) != wantSteps {
				t.Errorf("%d boundary steps, want %d", len(steps), wantSteps)
			}
		})
	}
}

// refoldLegacyComposeChain is composeChain's body as it was before the observer, kept so the nil-observer
// path can be measured against it.
func (s *Server) refoldLegacyComposeChain(chain []types.GovernanceUnit) (*ResolvedUnit, error) {
	if len(chain) == 0 {
		return nil, errors.New("api: an empty governance profile chain")
	}
	if err := checkUnitChain(chain, store.MaxUnitDepthCap); err != nil {
		return nil, err
	}
	leaf := chain[0]
	out := &ResolvedUnit{ID: leaf.ID, Name: leaf.Name, Contact: leaf.Contact, Composed: leaf.Composed()}
	for _, r := range chain {
		out.Chain = append(out.Chain, unitLink{ID: r.ID, Name: r.Name, Locked: r.LockedKnobs, Contact: r.Contact})
	}
	root := len(chain) - 1
	var base composer.Authority
	next := root
	switch {
	case chain[root].Kind == types.UnitKindDefault:
		base = s.deploymentAuthority()
		next = root - 1
	case chain[root].Composed():
		base = s.deploymentAuthority()
	default:
		var warns []string
		base, warns = s.standaloneAuthority(chain[root])
		out.StandaloneRoot = true
		if r := chain[root].Ceiling.Resources; r != nil {
			out.AuthoredResources = *r
		}
		out.AdminWarnings = append(out.AdminWarnings, warns...)
		if root == 0 {
			out.Warnings = warns
		}
		next = root - 1
	}
	deployment := s.deploymentAuthority().Ceiling
	last := policyref.Provenance{Layer: policyref.LayerUnit, Name: chain[root].Name}
	if out.StandaloneRoot {
		out.PathProvenance = startProvenance(last)
		if out.AuthoredResources == (types.ResourceLimits{}) {
			out.PathProvenance["resources"] = deploymentProvenance
		}
	} else {
		out.PathProvenance = startProvenance(deploymentProvenance)
		out.PathProvenance.setBy(last, deployment, base.Ceiling)
	}
	for i := next; i >= 0; i-- {
		if chain[i].Mode == types.UnitModePassThrough {
			continue
		}
		before := base.Ceiling.Clone()
		var err error
		if base, err = s.applyStep(out, base, chain[i], i == 0); err != nil {
			return nil, err
		}
		last = policyref.Provenance{Layer: policyref.LayerUnit, Name: chain[i].Name}
		out.PathProvenance.setBy(last, before, base.Ceiling)
	}
	out.Ceiling, out.Limits = base.Ceiling, base.Limits
	if out.Composed {
		out.Ceiling.Resources = s.inheritDeploymentResources(out.Ceiling.Resources)
	}
	if !out.StandaloneRoot {
		out.PathProvenance.resetUnchanged(deploymentProvenance, deployment, out.Ceiling)
	}
	return out, nil
}

// TestWallsObserverIsNotCalledWhenNil: with no observer the resolver calls none (a call would panic) and builds
// no step, so it allocates no more than its body did before the observer: no Clone per step. With an
// observer, each step's copy is measurable, so the comparison would see one taken without it. A fixture that
// resolves with a warning is not measured: a warning is formatted through fmt's pool, which the race detector
// drops from at random, so its allocations are not a fixed count.
func TestWallsObserverIsNotCalledWhenNil(t *testing.T) {
	srv := refoldTestServer()
	measured := 0
	for _, tc := range refoldTestCases() {
		t.Run(tc.name, func(t *testing.T) {
			chain := tc.chain()
			legacy, legacyErr := srv.refoldLegacyComposeChain(chain)
			got, err := srv.composeChainObserved(chain, nil)
			if err != nil || legacyErr != nil || !reflect.DeepEqual(got, legacy) {
				t.Fatalf("the legacy body is not the resolver's: %v %v\n got    %+v\n legacy %+v", err, legacyErr, got, legacy)
			}
			if len(got.AdminWarnings) > 0 {
				t.Skipf("resolves with warnings %q", got.AdminWarnings)
			}
			if len(tc.steps) > 0 {
				measured++
			}
			before := testing.AllocsPerRun(50, func() { _, _ = srv.refoldLegacyComposeChain(chain) })
			unobserved := testing.AllocsPerRun(50, func() { _, _ = srv.composeChainObserved(chain, nil) })
			observed := testing.AllocsPerRun(50, func() { _, _ = srv.composeChainObserved(chain, func(wallsStep) {}) })
			if unobserved > before {
				t.Errorf("a nil observer allocates %.0f per resolve, the body before the observer %.0f: a step is built for no one", unobserved, before)
			}
			if n := float64(len(tc.steps)); observed < unobserved+n {
				t.Errorf("an observer adds %.0f allocations for %.0f steps: each step's copy should show", observed-unobserved, n)
			}
		})
	}
	if measured == 0 {
		t.Fatal("no fixture with a step was measured")
	}
}

// TestWallsObserverStepsMatchTheChain: one unit step per unit that is not a pass-through, in chain order from
// the root, after a deployment step for a Default root and for a composed root with no base; a standalone root
// is no step. Each step starts where the one before it ended, and the last ends at the resolved allowlist.
func TestWallsObserverStepsMatchTheChain(t *testing.T) {
	srv := refoldTestServer()
	for _, tc := range refoldTestCases() {
		t.Run(tc.name, func(t *testing.T) {
			chain := tc.chain()
			var steps []wallsStep
			res, err := srv.composeChainObserved(chain, refoldRecorder(&steps))
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, st := range steps {
				names = append(names, st.Name)
			}
			if !slices.Equal(names, tc.steps) {
				t.Fatalf("steps %q, want %q", names, tc.steps)
			}
			for i, st := range steps {
				switch st.Kind {
				case wallsRefoldDeployment:
					if i != 0 || st.ID != uuid.Nil || st.UpdatedAt != nil || !reflect.DeepEqual(st.Before, types.RunPolicySpec{}) {
						t.Errorf("deployment step %d = %+v, want the first, with no id, no time and nothing before it", i, st)
					}
				case wallsRefoldUnit:
					at := slices.IndexFunc(chain, func(u types.GovernanceUnit) bool { return u.ID == st.ID })
					if at < 0 {
						t.Errorf("unit step %d = %+v names no row of the chain", i, st)
						continue
					}
					row := chain[at]
					if row.Name != st.Name || row.Mode == types.UnitModePassThrough || st.UpdatedAt == nil || !st.UpdatedAt.Equal(row.UpdatedAt) {
						t.Errorf("unit step %d = %+v, want its own row's name and time, never a pass-through's", i, st)
					}
				default:
					t.Errorf("step %d has kind %q", i, st.Kind)
				}
				if i > 0 && !reflect.DeepEqual(st.Before, steps[i-1].After) {
					t.Errorf("step %d does not start where step %d ended", i, i-1)
				}
			}
			if n := len(steps); n > 0 && !slices.Equal(steps[n-1].After.AllowedDomains, res.Ceiling.AllowedDomains) {
				t.Errorf("the last step ends at %v, the resolve at %v", steps[n-1].After.AllowedDomains, res.Ceiling.AllowedDomains)
			}
		})
	}
}

// TestWallsRefoldClampRule pins wallsRefoldRemover: exact membership after lower-casing and trimming, an
// allow-all list holds everything, no cover, and exactly one remover or none.
func TestWallsRefoldClampRule(t *testing.T) {
	allow := func(hosts ...string) types.RunPolicySpec { return types.RunPolicySpec{AllowedDomains: hosts} }
	all := types.RunPolicySpec{AllowAllEgress: true}
	deny := func(hosts ...string) types.RunPolicySpec { return types.RunPolicySpec{DeniedDomains: hosts} }
	step := func(name string, before, after types.RunPolicySpec) wallsStep {
		return wallsStep{Kind: wallsRefoldUnit, Name: name, Before: before, After: after}
	}
	allowed := func(entry string) wallsStrike {
		return wallsStrike{Field: fieldAllowed, Entry: entry, Cause: causeLimits}
	}
	denied := func(entry string) wallsStrike { return wallsStrike{Field: fieldDenied, Entry: entry, Cause: causeUnit} }
	for _, tc := range []struct {
		name   string
		steps  []wallsStep
		strike wallsStrike
		want   string // the one remover's name, "" for none
	}{
		{"an exact entry removed", []wallsStep{step("a", allow("x.example", "y.example"), allow("x.example"))}, allowed("y.example"), "a"},
		{"case and space folded on the list", []wallsStep{step("a", allow("  PyPI.ORG "), allow("x.example"))}, allowed("pypi.org"), "a"},
		{"case and space folded on the entry", []wallsStep{step("a", allow("pypi.org"), allow("x.example"))}, allowed(" PyPI.org"), "a"},
		{"a respelling is not a removal", []wallsStep{step("a", allow("PyPI.org"), allow(" pypi.org "))}, allowed("pypi.org"), ""},
		{"allow-all entering", []wallsStep{step("a", all, allow("x.example"))}, allowed("y.example"), "a"},
		{"allow-all leaving", []wallsStep{step("a", allow("y.example"), all)}, allowed("y.example"), ""},
		{"allow-all throughout", []wallsStep{step("a", all, all)}, allowed("y.example"), ""},
		{"not on any wall's list", []wallsStep{step("a", allow("x.example"), allow("x.example")), step("b", allow("x.example"), allow())}, allowed("y.example"), ""},
		{"a wildcard is not cover", []wallsStep{step("a", allow("*.example.com"), allow())}, allowed("api.example.com"), ""},
		{"two steps both removing", []wallsStep{step("a", allow("y.example"), allow()), step("b", all, allow("x.example"))}, allowed("y.example"), ""},
		{"the one step that removed it", []wallsStep{step("a", allow("x.example", "y.example"), allow("x.example", "y.example")), step("b", allow("x.example", "y.example"), allow("x.example"))}, allowed("y.example"), "b"},
		{"a deny starts covering", []wallsStep{step("a", deny(), deny("evil.example")), step("b", deny("evil.example"), deny("evil.example", "x.example"))}, denied("evil.example"), "a"},
		{"a wildcard deny covers", []wallsStep{step("a", deny(), deny("*.evil.example"))}, denied("api.evil.example"), "a"},
		{"a deny covered from the start", []wallsStep{step("a", deny("evil.example"), deny("evil.example"))}, denied("evil.example"), ""},
		{"a deny dropped and covered again", []wallsStep{step("a", deny(), deny("evil.example")), step("b", deny("evil.example"), deny()), step("c", deny(), deny("evil.example"))}, denied("evil.example"), ""},
		{"another field", []wallsStep{step("a", allow("y.example"), allow())}, wallsStrike{Field: "eligible_grants", Entry: "y.example"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i, ok := wallsRefoldRemover(tc.steps, tc.strike)
			got := ""
			if ok {
				got = tc.steps[i].Name
			}
			if got != tc.want {
				t.Errorf("remover = %q (ok %v), want %q", got, ok, tc.want)
			}
		})
	}
}
