// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// macWithBases is readyMac holding the other bases bs.
func macWithBases(bs ...runnerwire.MacVMDerivedBase) *runnerwire.MacVMCaps {
	mc := readyMac()
	mc.Bases = bs
	return mc
}

// A run that picks another base is judged on that base: its harnesses and its age; one the Mac has no
// ready copy of is refused with MAC-B08, for one Mac and for a pool alike.
func TestMacOSVMOffer_PickedBase(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	rn := managedMac("mac-studio-07")
	xcode := runnerwire.MacVMDerivedBase{Name: "xcode-16", State: runnerwire.MacVMDerivedReady, Items: 1, GuestOS: "macOS 27.0.1 (26A434)",
		Harnesses: []string{"none"}, CreatedAt: now.AddDate(0, 0, -41).Format(time.RFC3339)}
	caps := runnerwire.Caps{MacOSVM: macWithBases(xcode, runnerwire.MacVMDerivedBase{Name: "sec", State: runnerwire.MacVMDerivedRebuilding, Progress: 30})}

	for _, base := range []string{"", types.MacOSStandardBase} {
		if o := macOSVMOffer(rn, caps, true, 0, false, macOSBaseLimit{Base: base}); !o.Offered || !slices.Equal(o.Harnesses, []string{"none", "claude-code"}) {
			t.Errorf("Standard (%q): %+v", base, o)
		}
	}
	if o := macOSVMOffer(rn, caps, true, 0, false, macOSBaseLimit{Base: "xcode-16"}); !o.Offered || !slices.Equal(o.Harnesses, []string{"none"}) {
		t.Errorf("a ready other base: %+v", o)
	}
	o := macOSVMOffer(rn, caps, true, 0, false, macOSBaseLimit{Base: "xcode-16", MaxDays: 30, Now: now})
	if o.Offered || o.Why != client.MacOSVMWhyBaseTooOld || o.Have != 41 {
		t.Errorf("the picked base's age: %+v", o)
	}
	const b08 = "No Mac in this pool has the macOS base %s. Choose another base or another pool."
	for _, base := range []string{"sec", "absent"} {
		o := macOSVMOffer(rn, caps, true, 0, false, macOSBaseLimit{Base: base})
		if o.Offered || o.Why != client.MacOSVMWhyNoBase || o.Pool != "" || o.Refusal.Field != "macos_base" ||
			o.Refusal.Detail != "No Mac in this pool has the macOS base "+base+". Choose another base or another pool." {
			t.Errorf("%s: %+v, want %q", base, o, b08)
		}
	}

	// macOSVMFit reads the picked base's harnesses.
	fit := *caps.MacOSVM
	fit.Harnesses = macOSVMOffer(rn, caps, true, 0, false, macOSBaseLimit{Base: "xcode-16"}).Harnesses
	if ref := macOSVMFit(&fit, rn, macOSVMAsk{Harnesses: []string{"claude-code"}}, false); ref == nil || ref.Field != "harness" {
		t.Errorf("a harness the picked base cannot serve: %+v", ref)
	}
}

// A pool's other bases: ready only where every Mac holds it ready; rebuilding at the least progress; missing
// where one Mac lacks it or failed, which outranks rebuilding. Ready, the facts every copy shares.
func TestPoolMacOSBases(t *testing.T) {
	ready := func(name, guest, created string, harnesses ...string) runnerwire.MacVMDerivedBase {
		return runnerwire.MacVMDerivedBase{Name: name, State: runnerwire.MacVMDerivedReady, GuestOS: guest, CreatedAt: created, Harnesses: harnesses}
	}
	rebuilding := func(name string, p int) runnerwire.MacVMDerivedBase {
		return runnerwire.MacVMDerivedBase{Name: name, State: runnerwire.MacVMDerivedRebuilding, Progress: p}
	}
	a := macWithBases(ready("xcode", "g1", "2026-10-20T00:00:00Z", "none", "claude-code"), rebuilding("sec", 60), ready("only-a", "g1", ""),
		rebuilding("mixed", 10))
	b := macWithBases(ready("xcode", "g2", "2026-10-10T00:00:00Z", "none"), rebuilding("sec", 20),
		runnerwire.MacVMDerivedBase{Name: "mixed", State: runnerwire.MacVMDerivedFailed})
	got := poolMacOSBases([]*runnerwire.MacVMCaps{a, b})
	want := []client.PoolMacOSBase{
		{Name: "mixed", State: client.MacOSBaseMissing},
		{Name: "only-a", State: client.MacOSBaseMissing},
		{Name: "sec", State: client.MacOSBaseRebuilding, Progress: 20},
		{Name: "xcode", State: client.MacOSBaseReady, Harnesses: []string{"none"}, CreatedAt: "2026-10-10T00:00:00Z"},
	}
	if !slices.EqualFunc(got, want, func(x, y client.PoolMacOSBase) bool {
		return x.Name == y.Name && x.State == y.State && x.Progress == y.Progress && x.GuestOS == y.GuestOS &&
			slices.Equal(x.Harnesses, y.Harnesses) && x.CreatedAt == y.CreatedAt
	}) {
		t.Fatalf("bases %+v\nwant  %+v", got, want)
	}
	if got := poolMacOSBases([]*runnerwire.MacVMCaps{readyMac()}); got != nil {
		t.Errorf("no other bases: %+v", got)
	}
	if got := poolMacOSVM([]macOSCandidate{macCandidate(managedMac("studio"), a, 0)}, false); !got.Offered || len(got.Bases) != 4 {
		t.Errorf("the pool list: %+v", got)
	}
}

// macos_base rides only a macOS VM run and names a base; it is kept on the run's vm_machine from create
// for dispatch, and a run that picks none keeps none.
func TestMacOSBaseRequest(t *testing.T) {
	mac := string(types.MachineMacOSVM)
	for _, tc := range []struct {
		req  createRunRequest
		want bool
	}{
		{createRunRequest{}, true},
		{createRunRequest{MachineKind: mac}, true},
		{createRunRequest{MachineKind: mac, MacOSBase: "Standard"}, true},
		{createRunRequest{MachineKind: mac, MacOSBase: "xcode-16"}, true},
		{createRunRequest{MachineKind: mac, MacOSBase: "../bases/0"}, false},
		{createRunRequest{MachineKind: mac, MacOSBase: "Xcode"}, false},
		{createRunRequest{MacOSBase: "xcode-16"}, false},
		{createRunRequest{MachineKind: string(types.MachineLinuxVM), MacOSBase: "xcode-16"}, false},
	} {
		if ok := macOSBaseRequestRefusal(tc.req) == ""; ok != tc.want {
			t.Errorf("%+v: accepted %v, want %v", tc.req, ok, tc.want)
		}
	}
	if macOSBaseMachine(createRunRequest{MachineKind: mac}) != nil || macOSBaseMachine(createRunRequest{MacOSBase: "x"}) != nil {
		t.Error("a run that picks no base, or is no macOS VM, was given a machine")
	}
	run := types.AgentRun{MachineKind: types.MachineMacOSVM, VMMachine: macOSBaseMachine(createRunRequest{MachineKind: mac, MacOSBase: "xcode-16"})}
	if got := macOSBaseOf(run); got != "xcode-16" {
		t.Errorf("macOSBaseOf = %q", got)
	}
	if got := macOSBaseOf(types.AgentRun{MachineKind: types.MachineMacOSVM}); got != "" {
		t.Errorf("no machine: %q", got)
	}
	if msg := governanceLimitsRefusal(types.GovernanceLimits{AllowedMacOSBases: []string{"Standard", "xcode-16"}}); msg != "" {
		t.Errorf("a profile's valid bases: %s", msg)
	}
	if msg := governanceLimitsRefusal(types.GovernanceLimits{AllowedMacOSBases: []string{"../x"}}); msg == "" {
		t.Error("a profile's base that is no name was accepted")
	}
}

// allowed_macos_bases refuses a governed member a base it does not list with MAC-B09, naming the profile; an
// empty list, a listed base and an ungoverned caller pass.
func TestMacOSBaseDeniedRefusal(t *testing.T) {
	s := New(Config{Audit: &recRecorder{}})
	member := govMemberCtx([]string{"eng"}, false)
	ceiling := governanceCeiling{Unit: &ResolvedUnit{ID: uuid.New(), Name: "Interns"},
		Limits: types.GovernanceLimits{AllowedMacOSBases: []string{types.MacOSStandardBase}}}
	f := s.macOSBaseDeniedRefusal(member, "xcode-16", ceiling)
	if f == nil || f.status != http.StatusForbidden || f.body.Reason != "placement_denied" || f.body.Field != "macos_base" ||
		f.body.Error != "Interns doesn't allow the macOS base xcode-16. Choose another base." {
		t.Fatalf("refusal %+v", f)
	}
	for _, base := range []string{"", types.MacOSStandardBase} {
		if f := s.macOSBaseDeniedRefusal(member, base, ceiling); f != nil {
			t.Errorf("a listed base (%q) was refused: %+v", base, f)
		}
	}
	ceiling.Limits.AllowedMacOSBases = nil
	if f := s.macOSBaseDeniedRefusal(member, "xcode-16", ceiling); f != nil {
		t.Errorf("an empty list refused: %+v", f)
	}
	if f := s.macOSBaseDeniedRefusal(member, "xcode-16", governanceCeiling{}); f != nil {
		t.Errorf("no profile refused: %+v", f)
	}
}

// allowed_macos_bases binds a macOS VM run already placed, as deny_placements does: a revive of a run on a
// base some list no longer allows is refused placement_denied with MAC-B09, whether the list is the
// profile's or a restriction's (each non-empty list must allow the base). Other runs and listed bases pass.
func TestRevivePlacementDenied_MacOSBase(t *testing.T) {
	rec := &recRecorder{}
	s := New(Config{Audit: rec})
	run := newFinalizeRun()
	run.MachineKind = types.MachineMacOSVM
	run.VMMachine = &types.VMMachine{MacOSBase: "xcode-16"}
	list := func(bases ...string) *[]string { return &bases }
	restriction := func(l *[]string) types.Restriction {
		return types.Restriction{ID: uuid.New(), Limits: types.RestrictionLimits{AllowedMacOSBases: l}}
	}
	profile := ownerCeiling{profile: "Interns", allowedMacOSBases: [][]string{{types.MacOSStandardBase}}}
	bound := ownerCeiling{profile: "Interns"}.withRevivedRestrictions([]types.Restriction{
		restriction(list("xcode-16", types.MacOSStandardBase)), restriction(list(types.MacOSStandardBase)), restriction(list()), restriction(nil)})
	if len(bound.allowedMacOSBases) != 2 {
		t.Fatalf("restriction lists folded = %v, want the two non-empty ones", bound.allowedMacOSBases)
	}
	const want = "Interns doesn't allow the macOS base xcode-16. Choose another base."
	for name, c := range map[string]ownerCeiling{"profile": profile, "restrictions": bound} {
		rerr := s.revivePlacementDenied(context.Background(), run, c, types.ActorHuman, "owner")
		if rerr == nil || rerr.status != http.StatusForbidden || rerr.reason != "placement_denied" || rerr.msg != want {
			t.Errorf("%s: revive = %+v, want 403 placement_denied %q", name, rerr, want)
		}
	}
	if rows := rec.snapshot(); len(rows) != 2 || rows[0].Action != "run.revive" || rows[0].Outcome != "denied" {
		t.Errorf("audit rows = %+v, want two denied run.revive rows", rows)
	}
	allowed := ownerCeiling{profile: "Interns", allowedMacOSBases: [][]string{{"xcode-16"}, {"xcode-16", types.MacOSStandardBase}}}
	standard, linux := run, run
	standard.VMMachine = nil // Standard
	linux.MachineKind = types.MachineLinuxVM
	for name, tc := range map[string]struct {
		run types.AgentRun
		c   ownerCeiling
	}{
		"every list allows the base": {run, allowed},
		"no list":                    {run, ownerCeiling{profile: "Interns"}},
		"Standard, listed":           {standard, ownerCeiling{allowedMacOSBases: [][]string{{types.MacOSStandardBase}}}},
		"not a macOS VM":             {linux, profile},
	} {
		if rerr := s.revivePlacementDenied(context.Background(), tc.run, tc.c, types.ActorHuman, "owner"); rerr != nil {
			t.Errorf("%s: revive refused: %+v", name, rerr)
		}
	}
}

// The pool list marks a base NotAllowed (MAC-B05) by the very rule create holds the caller to
// (macOSBaseAllowedFor), so the picker never offers a base create then refuses: for a listed base, a denied
// one, a profile narrowed by a restriction, Standard left off the list, no profile and an ungoverned
// operator, each listed base agrees with macOSBaseDeniedRefusal, and the list itself never goes out.
func TestMacOSBaseListingAgreesWithPlacement(t *testing.T) {
	s := capsServer(&fakeRunner{})
	ready := func(name string) runnerwire.MacVMDerivedBase {
		return runnerwire.MacVMDerivedBase{Name: name, State: runnerwire.MacVMDerivedReady, Items: 1, Harnesses: []string{"none"}}
	}
	mac := macRunner(t, s, macWithBases(ready("xcode-16"), ready("security-tools")), nil)
	fits := map[string]runnerFit{mac.String(): {runner: s.cfg.Store.(ownedRunners).rows[mac]}}
	pool := []poolTarget{{id: mac.String(), runnerID: &mac}}
	member := govMemberCtx([]string{"eng"}, false)
	operator := operatorCtx("sam", "sam@corp.example", oidc.RoleAdmin)

	profile := func(bases ...string) governanceCeiling {
		return governanceCeiling{Unit: &ResolvedUnit{ID: uuid.New(), Name: "Platform"}, Limits: types.GovernanceLimits{AllowedMacOSBases: bases}}
	}
	all := []string{types.MacOSStandardBase, "xcode-16", "security-tools"}
	narrowed, err := s.withRestrictions(member, profile(all...), []types.Restriction{{ID: uuid.New(), Name: "r",
		Limits: types.RestrictionLimits{AllowedMacOSBases: &[]string{types.MacOSStandardBase, "xcode-16"}}}})
	if err != nil {
		t.Fatalf("withRestrictions: %v", err)
	}
	cases := []struct {
		name      string
		ctx       context.Context
		ceiling   governanceCeiling
		wantStd   bool            // Standard NotAllowed
		wantBases map[string]bool // base -> NotAllowed
	}{
		{"an allowed base", member, profile(all...), false, map[string]bool{"xcode-16": false, "security-tools": false}},
		{"a denied base", member, profile(types.MacOSStandardBase), false, map[string]bool{"xcode-16": true, "security-tools": true}},
		// The narrowed ceiling keeps profile()'s Unit; with no Unit the helper would allow every base.
		{"narrowed by a restriction", member, narrowed, false, map[string]bool{"xcode-16": false, "security-tools": true}},
		{"Standard left off the list", member, profile("xcode-16"), true, map[string]bool{"xcode-16": false, "security-tools": true}},
		{"an empty list", member, profile(), false, map[string]bool{"xcode-16": false, "security-tools": false}},
		{"no profile", member, governanceCeiling{}, false, map[string]bool{"xcode-16": false, "security-tools": false}},
		{"an ungoverned operator", operator, profile(types.MacOSStandardBase), false, map[string]bool{"xcode-16": false, "security-tools": false}},
	}
	for _, tc := range cases {
		listing := s.poolMacOSVMOf(tc.ctx, pool, fits, nil, tc.ceiling)
		if !listing.Offered || len(listing.Bases) != len(tc.wantBases) {
			t.Fatalf("%s: listing %+v, want an offered pool with %d other bases", tc.name, listing, len(tc.wantBases))
		}
		if got := s.macOSBaseDeniedRefusal(tc.ctx, "", tc.ceiling) != nil; listing.StandardNotAllowed != got || got != tc.wantStd {
			t.Errorf("%s: Standard NotAllowed %v, create refuses %v, want %v", tc.name, listing.StandardNotAllowed, got, tc.wantStd)
		}
		for _, b := range listing.Bases {
			refused := s.macOSBaseDeniedRefusal(tc.ctx, b.Name, tc.ceiling)
			if b.NotAllowed != (refused != nil) || b.NotAllowed != tc.wantBases[b.Name] {
				t.Errorf("%s: %s NotAllowed %v, create refuses %v, want %v", tc.name, b.Name, b.NotAllowed, refused != nil, tc.wantBases[b.Name])
			}
			if refused != nil && !strings.Contains(refused.body.Error, "Platform doesn't allow the macOS base "+b.Name) {
				t.Errorf("%s: %s refusal %q is not MAC-B09 for the profile", tc.name, b.Name, refused.body.Error)
			}
		}
		// Only the caller's own answer per base goes out, never the list or another person's.
		raw, err := json.Marshal(listing)
		if err != nil || strings.Contains(string(raw), "allowed_macos_bases") {
			t.Errorf("%s: the listing carries the allow list: %s (marshal err %v)", tc.name, raw, err)
		}
	}
}
