// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// reviewResolve is what p composes to once written over all.
func reviewResolve(t *testing.T, s *Server, all []types.GovernanceUnit, p types.GovernanceUnit) *ResolvedUnit {
	t.Helper()
	next := append(slices.DeleteFunc(slices.Clone(all), func(u types.GovernanceUnit) bool { return u.ID == p.ID }), p)
	chain, err := chainFromRows(unitsByID(next), p.ID, store.MaxUnitDepthCap)
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.composeChain(chain)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// TestUnitWriteVerdictReview: every held profile write's review names what holds it, judged against the
// current effective profile, and the exemption it decides is unitWriteExempt's.
func TestUnitWriteVerdictReview(t *testing.T) {
	other := uuid.MustParse("dddddddd-0000-0000-0000-0000000000ab")
	missing := uuid.MustParse("dddddddd-0000-0000-0000-0000000000ac")
	tie := []types.GovernanceMembership{
		{SubjectType: types.CapabilitySubjectGroup, Subject: "eng", UnitID: govCovPID, Priority: 5},
		{SubjectType: types.CapabilitySubjectGroup, Subject: "ops", UnitID: other, Priority: 5},
	}
	type fixture struct {
		all []types.GovernanceUnit
		p   types.GovernanceUnit
		res *ResolvedUnit
	}
	for _, c := range []struct {
		name        string
		assignments []types.GovernanceMembership
		build       func(t *testing.T, s *Server) fixture
		verdict     string
		held        []string
		widens      []string // field names, in order
		entries     map[string][]string
	}{
		{name: "a lock removed", verdict: verdictNarrowsHeld, held: []string{heldLockRemoved}, build: func(t *testing.T, s *Server) fixture {
			cur := govCovStandalone(govCovPID, "p", "pypi.org")
			cur.LockedKnobs = []string{"allowed_domains"}
			p := cur
			p.LockedKnobs = nil
			all := []types.GovernanceUnit{cur}
			return fixture{all, p, reviewResolve(t, s, all, p)}
		}},
		{name: "a contact change", verdict: verdictNarrowsHeld, held: []string{heldContact}, build: func(t *testing.T, s *Server) fixture {
			cur := govCovStandalone(govCovPID, "p", "pypi.org")
			cur.Contact = &policyref.Contact{Owner: "platform"}
			p := cur
			p.Contact, p.ContactSet = &policyref.Contact{Owner: "elsewhere"}, true
			all := []types.GovernanceUnit{cur}
			return fixture{all, p, reviewResolve(t, s, all, p)}
		}},
		{name: "a rename in a group tie", assignments: tie, verdict: verdictNarrowsHeld, held: []string{heldRename}, build: func(t *testing.T, s *Server) fixture {
			cur := govCovStandalone(govCovPID, "p", "pypi.org")
			p := cur
			p.Name = "renamed"
			all := []types.GovernanceUnit{cur, govCovStandalone(other, "q", "pypi.org")}
			return fixture{all, p, reviewResolve(t, s, all, p)}
		}},
		{name: "an added domain widens with its entry", verdict: verdictWidens, widens: []string{"allowed_domains"},
			entries: map[string][]string{"allowed_domains": {"api.anthropic.com"}}, build: func(t *testing.T, s *Server) fixture {
				all := []types.GovernanceUnit{govCovStandalone(govCovPID, "p", "pypi.org", "github.com")}
				p, res := govCovBuilt(t, s, all, govCovPID, govCovProfileBody("p", "pypi.org", "github.com", "api.anthropic.com"))
				return fixture{all, p, res}
			}},
		{name: "a raised member cap", verdict: verdictWidens, widens: []string{wideningMemberCap}, build: func(t *testing.T, s *Server) fixture {
			all := []types.GovernanceUnit{govCovStandalone(govCovPID, "p", "pypi.org")}
			p, res := govCovBuilt(t, s, all, govCovPID,
				`{"name":"p","ceiling":{"allowed_domains":["pypi.org"],"min_confinement_class":"CC2"},"limits":{"max_cpu_millis":8000}}`)
			return fixture{all, p, res}
		}},
		{name: "a denial dropped from the limits", verdict: verdictWidens, widens: []string{wideningLimits}, build: func(t *testing.T, s *Server) fixture {
			cur := govCovStandalone(govCovPID, "p", "pypi.org")
			cur.Limits.DenyInteractive = true
			p := cur
			p.Limits.DenyInteractive = false
			all := []types.GovernanceUnit{cur}
			return fixture{all, p, reviewResolve(t, s, all, p)}
		}},
		{name: "a create", verdict: verdictNotChecked, build: func(t *testing.T, s *Server) fixture {
			p, res := govCovBuilt(t, s, nil, govCovPID, govCovProfileBody("p", "pypi.org"))
			return fixture{nil, p, res}
		}},
		{name: "a current profile that does not compose", verdict: verdictNotChecked, build: func(t *testing.T, s *Server) fixture {
			all := []types.GovernanceUnit{govCovComposedOn(govCovPID, "p", missing, "pypi.org")}
			p := govCovStandalone(govCovPID, "p", "pypi.org")
			return fixture{all, p, reviewResolve(t, s, nil, p)}
		}},
		{name: "a write that did not resolve", verdict: verdictNotChecked, build: func(t *testing.T, s *Server) fixture {
			cur := govCovStandalone(govCovPID, "p", "pypi.org")
			return fixture{[]types.GovernanceUnit{cur}, cur, nil}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, _ := govCovServer(t, &govCovStore{assignments: c.assignments})
			f := c.build(t, s)
			exempt, review := s.unitWriteVerdict(context.Background(), f.all, f.p, f.res)
			if exempt || exempt != s.unitWriteExempt(context.Background(), f.all, f.p, f.res) {
				t.Fatalf("exempt = %v, unitWriteExempt = %v; want both false", exempt, s.unitWriteExempt(context.Background(), f.all, f.p, f.res))
			}
			if review == nil || review.Verdict != c.verdict || !reflect.DeepEqual(review.HeldBecause, c.held) {
				t.Fatalf("review = %+v, want verdict %s held %v", review, c.verdict, c.held)
			}
			var fields []string
			for _, w := range review.Widens {
				fields = append(fields, w.Field)
				if want, ok := c.entries[w.Field]; ok && !reflect.DeepEqual(w.Entries, want) {
					t.Errorf("%s entries = %v, want %v", w.Field, w.Entries, want)
				}
			}
			if !reflect.DeepEqual(fields, c.widens) {
				t.Errorf("widens = %v, want %v", fields, c.widens)
			}
			if c.widens != nil && c.widens[0] == wideningMemberCap && (len(review.Widens[0].Entries) != 1 || !strings.HasPrefix(review.Widens[0].Entries[0], "max_cpu_millis")) {
				t.Errorf("member_cap entries = %v, want the CPU raise", review.Widens[0].Entries)
			}
		})
	}
}

// TestChangeReviewStoresNotCheckedWhenNothingIsFound: a held write whose check finds neither a widening
// nor a held reason stores not_checked, never an empty claim; renderGovernanceDiff applies it to any review.
func TestChangeReviewStoresNotCheckedWhenNothingIsFound(t *testing.T) {
	if got := reviewFrom(nil, nil, true); got.Verdict != verdictNotChecked {
		t.Errorf("reviewFrom(nothing) = %+v, want not_checked", got)
	}
	for _, c := range []struct {
		in   changeReview
		want string
	}{
		{changeReview{Verdict: verdictNarrowsHeld}, verdictNotChecked},
		{changeReview{Verdict: verdictWidens}, verdictNotChecked},
		{changeReview{Verdict: verdictWidens, HeldBecause: []string{heldContact}}, verdictNarrowsHeld},
		{changeReview{Verdict: verdictNotChecked, HeldBecause: []string{heldContact}}, verdictNotChecked},
		{changeReview{Verdict: verdictNarrowsHeld, Widens: []composer.Widening{{Field: "allowed_domains"}}}, verdictWidens},
	} {
		in := c.in
		var d governanceDiffBody
		if err := json.Unmarshal(renderGovernanceDiff(nil, nil, nil, nil, heldReads{review: &in}), &d); err != nil {
			t.Fatal(err)
		}
		if d.Review == nil || d.Review.Verdict != c.want {
			t.Errorf("stored review of %+v = %+v, want %s", c.in, d.Review, c.want)
		}
		if d.Review.Verdict == verdictNotChecked && (d.Review.Widens != nil || d.Review.HeldBecause != nil) {
			t.Errorf("a not_checked review carries findings: %+v", d.Review)
		}
	}
}

// TestChangeReviewSurvivesEveryReadTier: a stored review reads the same at the member, security and full
// tiers, and never holds a mount's host path, though the diff around it does at the full tier.
func TestChangeReviewSurvivesEveryReadTier(t *testing.T) {
	const planted = "/srv/planted-host-path"
	s, _ := govCovServer(t, &govCovStore{})
	ctx := context.Background()
	cur := govCovStandalone(govCovPID, "p", "pypi.org")
	p := cur
	rw := false
	p.Ceiling.AllowedDomains = []string{"pypi.org", "api.anthropic.com"}
	p.Ceiling.WorkspaceMounts = []types.WorkspaceMount{{Source: planted, Target: "/home/agent/data", ReadOnly: &rw}}
	all := []types.GovernanceUnit{cur}
	res := reviewResolve(t, s, all, p)
	exempt, review := s.unitWriteVerdict(ctx, all, p, res)
	if exempt {
		t.Fatal("adding a writable mount applied without approval")
	}
	review = s.unitReviewRisk(ctx, all, govCovPID, review, res)
	if review.Risk == nil || review.Risk.Before == nil {
		t.Fatalf("review = %+v, want a risk before and after", review)
	}
	before, after := newUnitDiffView(cur, s.resolveForDiff(all, govCovPID), specReadFull), newUnitDiffView(p, res, specReadFull)
	ch := types.GovernanceChange{Diff: renderGovernanceDiff(before, after, []string{"ceiling"}, nil, heldReads{review: review})}
	if !strings.Contains(string(ch.Diff), planted) {
		t.Fatalf("the fixture's host path is not in the stored diff, so its absence below proves nothing: %s", ch.Diff)
	}
	canonical := func(raw json.RawMessage) string {
		var d map[string]json.RawMessage
		if err := json.Unmarshal(raw, &d); err != nil || d["review"] == nil {
			t.Fatalf("diff %s has no review (%v)", raw, err)
		}
		var v any
		if err := json.Unmarshal(d["review"], &v); err != nil {
			t.Fatal(err)
		}
		return string(mustJSON(v))
	}
	stored := canonical(ch.Diff)
	if strings.Contains(stored, planted) || !strings.Contains(stored, `"field":"workspace_mounts"`) || !strings.Contains(stored, "/home/agent/data") {
		t.Fatalf("stored review = %s, want the mounts widening and its target, never its host path", stored)
	}
	for _, role := range []string{oidc.RoleUser, oidc.RoleSecurityAdmin, oidc.RoleAdmin} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r = r.WithContext(withOIDCRole(withOIDCHuman(ctx, "sub-reader"), role))
		got := s.changeForRead(r, ch)
		if string(got.Diff) == "null" {
			t.Fatalf("%s: the diff is not served", role)
		}
		if read := canonical(got.Diff); read != stored {
			t.Errorf("%s: review = %s, want %s", role, read, stored)
		}
	}
}

// TestRestrictionReviewCollectsEveryReason: a replacement's review names each reason it is not at least
// as strict, and holds exactly when restrictionAtLeastAsStrict does.
func TestRestrictionReviewCollectsEveryReason(t *testing.T) {
	grant := types.GrantSpec{Kind: types.GrantKind("env_secret")}
	old := types.Restriction{Spec: types.RestrictionSpec{DeniedDomains: strs("a.example"), AllowedDomains: strs("b.example"),
		WithheldGrants: []types.GrantSpec{grant}}, Contact: &policyref.Contact{Owner: "platform"},
		Members: []types.RestrictionMember{userMember("sub-1"), userMember("sub-2")}}
	same := types.RestrictionWrite{Spec: old.Spec, Contact: old.Contact, Members: old.Members}
	for _, c := range []struct {
		name    string
		edit    func(w *types.RestrictionWrite)
		verdict string
		widens  []string
		held    []string
		entries []string
	}{
		{name: "unchanged applies", edit: func(*types.RestrictionWrite) {}},
		{name: "every reason at once", verdict: verdictWidens,
			edit: func(w *types.RestrictionWrite) {
				w.Spec.DeniedDomains = strs()
				w.Spec.AllowedDomains = strs("b.example", "evil.example")
				w.Spec.WithheldGrants = nil
				w.Contact = &policyref.Contact{Owner: "elsewhere"}
				w.Members = []types.RestrictionMember{userMember("sub-1")}
			},
			widens: []string{"denied_domains", wideningAllowedDomains, wideningWithheldGrants, wideningMembers}, held: []string{heldContact},
			entries: []string{"evil.example"}},
		{name: "a contact change alone", verdict: verdictNarrowsHeld, held: []string{heldContact},
			edit: func(w *types.RestrictionWrite) { w.Contact = nil }},
		{name: "everyone covers a dropped member", edit: func(w *types.RestrictionWrite) {
			w.Members = []types.RestrictionMember{{SubjectType: types.CapabilitySubjectAll}}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			next := same
			next.Spec.WithheldGrants = slices.Clone(old.Spec.WithheldGrants)
			c.edit(&next)
			got := restrictionHeldReview(false, old, next)
			if (got == nil) != restrictionAtLeastAsStrict(old, next) {
				t.Fatalf("review %+v disagrees with restrictionAtLeastAsStrict = %v", got, restrictionAtLeastAsStrict(old, next))
			}
			if c.verdict == "" {
				if got != nil {
					t.Errorf("review = %+v, want none (it applies directly)", got)
				}
				return
			}
			if got.Verdict != c.verdict || !reflect.DeepEqual(got.HeldBecause, c.held) {
				t.Errorf("review = %+v, want %s held %v", got, c.verdict, c.held)
			}
			for _, field := range c.widens {
				i := slices.IndexFunc(got.Widens, func(w composer.Widening) bool { return w.Field == field })
				if i < 0 {
					t.Errorf("widens = %+v, want %s among them", got.Widens, field)
					continue
				}
				if field == wideningAllowedDomains && !reflect.DeepEqual(got.Widens[i].Entries, c.entries) {
					t.Errorf("allowed_domains entries = %v, want %v", got.Widens[i].Entries, c.entries)
				}
			}
			if c.widens == nil && len(got.Widens) > 0 {
				t.Errorf("widens = %+v, want none", got.Widens)
			}
		})
	}
	first := types.Restriction{Spec: types.RestrictionSpec{DeniedDomains: strs("a.example")}}
	got := restrictionHeldReview(false, first, types.RestrictionWrite{Spec: types.RestrictionSpec{DeniedDomains: strs("a.example"), AllowedDomains: strs("b.example")}})
	if got == nil || got.Verdict != verdictNarrowsHeld || !reflect.DeepEqual(got.HeldBecause, []string{heldFirstAllowlist}) {
		t.Errorf("a first allowlist on update = %+v, want narrows_held first_allowlist", got)
	}
	if got := restrictionHeldReview(true, types.Restriction{}, types.RestrictionWrite{Spec: types.RestrictionSpec{AllowedDomains: strs("b.example")}}); got == nil ||
		got.Verdict != verdictNarrowsHeld || !reflect.DeepEqual(got.HeldBecause, []string{heldFirstAllowlist}) {
		t.Errorf("a create naming an allowlist = %+v, want narrows_held first_allowlist", got)
	}
	if got := restrictionHeldReview(true, types.Restriction{}, types.RestrictionWrite{}); got != nil {
		t.Errorf("a create naming no allowlist = %+v, want none (it applies directly)", got)
	}
}

// TestReviewForIsTheHoldSitesReview: reviewFor answers, with no request, the review the hold site stores
// for the same profile write, and judges every other kind as the contract says.
func TestReviewForIsTheHoldSitesReview(t *testing.T) {
	t.Setenv(envGovernanceSecondHuman, "true")
	super := govCovSession(t, "sub-super", "super@corp.example", oidc.RoleAdmin)
	superCtx := withOIDCRole(withOIDCHuman(context.Background(), "sub-super"), oidc.RoleAdmin)
	cur := govCovStandalone(govCovPID, "p", "pypi.org")
	widen := govCovProfileBody("p", "pypi.org", "github.com")

	st := &govCovStore{proposeSaved: types.GovernanceChange{ID: govCovChangeID, State: types.GovernanceChangePending}, profiles: []types.GovernanceUnit{cur}}
	s, _ := govCovServer(t, st)
	ch := govCovHoldAnswer(t, doSSO(t, s, http.MethodPut, "/api/v1/governance/units/"+govCovPID.String(), super, widen), st)
	stored := govCovDiff(t, ch).Review
	got, err := s.reviewFor(superCtx, govKindUnit, govCovPID.String(), nil, json.RawMessage(widen), reviewCaller{super: true})
	if err != nil || stored == nil || got == nil {
		t.Fatalf("reviewFor = %+v, %v; stored %+v", got, err, stored)
	}
	if string(mustJSON(got)) != string(mustJSON(stored)) {
		t.Errorf("reviewFor = %s, want the stored review %s", mustJSON(got), mustJSON(stored))
	}
	if got.Verdict != verdictWidens || got.Risk == nil || got.Risk.Before == nil {
		t.Errorf("review = %+v, want widens with a risk before and after", got)
	}

	// before stands in for the stored row: over a wider one the same write narrows and applies directly.
	wide := govCovStandalone(govCovPID, "p", "pypi.org", "github.com")
	if got, err := s.reviewFor(superCtx, govKindUnit, govCovPID.String(), mustJSON(wide), json.RawMessage(widen), reviewCaller{super: true}); err != nil || got != nil {
		t.Errorf("the same write over a wider before = %+v, %v; want none", got, err)
	}
	// A create is not_checked with no risk before; a unit admin's create applies directly; a delete is not_checked.
	fresh := uuid.MustParse("dddddddd-0000-0000-0000-0000000000ad").String()
	if got, err := s.reviewFor(superCtx, govKindUnit, fresh, nil, json.RawMessage(govCovProfileBody("fresh", "pypi.org")), reviewCaller{super: true}); err != nil ||
		got == nil || got.Verdict != verdictNotChecked || got.Risk == nil || got.Risk.Before != nil {
		t.Errorf("a create = %+v, %v; want not_checked with risk after only", got, err)
	}
	if got, err := s.reviewFor(superCtx, govKindUnit, fresh, nil, json.RawMessage(govCovProfileBody("fresh", "pypi.org")), reviewCaller{unitAdmin: true}); err != nil || got != nil {
		t.Errorf("a unit admin's create = %+v, %v; want none", got, err)
	}
	if got, err := s.reviewFor(superCtx, govKindUnit, govCovPID.String(), nil, nil, reviewCaller{super: true}); err != nil || got == nil || got.Verdict != verdictNotChecked {
		t.Errorf("a delete = %+v, %v; want not_checked", got, err)
	}
	for _, bad := range []struct{ key, body string }{{"not-an-id", widen}, {govCovPID.String(), `{"name":"p","nope":1}`}} {
		if _, err := s.reviewFor(superCtx, govKindUnit, bad.key, nil, json.RawMessage(bad.body), reviewCaller{super: true}); err == nil {
			t.Errorf("reviewFor(%q, %s) answered no error", bad.key, bad.body)
		}
	}

	// A restriction, its before given; the other kinds.
	oldR := types.Restriction{Spec: types.RestrictionSpec{DeniedDomains: strs("a.example")}, Members: []types.RestrictionMember{userMember("sub-1")}}
	looser := types.RestrictionWrite{Name: "r", Members: oldR.Members}
	if got, err := s.reviewFor(superCtx, govKindRestriction, uuid.NewString(), mustJSON(oldR), mustJSON(looser), reviewCaller{super: true}); err != nil ||
		got == nil || got.Verdict != verdictWidens {
		t.Errorf("a looser restriction = %+v, %v; want widens", got, err)
	}
	for kind, want := range map[string]string{
		govKindWorkspaceOwningUnit: verdictWidens, govKindComponentOwningUnit: verdictWidens,
		govKindGrant: verdictNotChecked, govKindMembership: verdictNotChecked,
	} {
		if got, err := s.reviewFor(superCtx, kind, "k", nil, json.RawMessage(`{}`), reviewCaller{super: true}); err != nil || got == nil || got.Verdict != want {
			t.Errorf("%s = %+v, %v; want %s", kind, got, err, want)
		}
	}
	for _, kind := range []string{types.GovernanceTargetElevation, types.GovernanceTargetImageRequest} {
		if got, err := s.reviewFor(superCtx, kind, "k", nil, json.RawMessage(`{}`), reviewCaller{super: true}); err != nil || got != nil {
			t.Errorf("%s = %+v, %v; want none (a request stores no review)", kind, got, err)
		}
	}
}

// TestImpactForCounts: impactFor answers counts for a profile write and none/kind for a kind with no
// dry run; a request kind's held change is never replayed.
func TestImpactForCounts(t *testing.T) {
	s, _ := govCovServer(t, &govCovStore{})
	ctx := withOIDCRole(withOIDCHuman(context.Background(), "sub-sec"), oidc.RoleSecurityAdmin)
	got, err := s.impactFor(ctx, govKindGrant, "k", json.RawMessage(`{}`))
	if err != nil || got != (impactCounts{Computed: impactNone, NotComputed: impactKindNotComputed}) {
		t.Errorf("a grant = %+v, %v; want none/kind", got, err)
	}
	if _, err := s.impactFor(ctx, govKindUnit, "not-an-id", json.RawMessage(govCovProfileBody("p", "pypi.org"))); err == nil {
		t.Error("a target key that is no profile id answered no error")
	}
	// The held change it stands for carries the graph's base hash, the deployment hash and the body.
	cur := govCovStandalone(govCovPID, "p", "pypi.org")
	s2, _ := govCovServer(t, &govCovStore{profiles: []types.GovernanceUnit{cur}})
	body := json.RawMessage(govCovProfileBody("p", "pypi.org", "github.com"))
	ch, err := s2.unitWriteAsHeld(ctx, govCovPID.String(), body)
	if err != nil {
		t.Fatal(err)
	}
	if ch.Op != "update" || ch.BaseHash != computeETag(unitBaseState([]types.GovernanceUnit{cur}, govCovPID, nil)) ||
		ch.DeploymentHash != computeETag(s2.cfg.DefaultPolicy) || string(ch.Payload) != string(body) || ch.ProposedBy != "sub-sec" {
		t.Errorf("held stand-in = %+v", ch)
	}
	if heldShiftStale(ch, legacyShift{}) != nil {
		t.Error("the stand-in's diff does not decode as a held shift")
	}
	// A deployment default that moved is stale before any replay; counts carry it.
	ch.DeploymentHash = "moved"
	out, err := s2.changeImpactOf(httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx), ch)
	if err != nil || out.counts() != (impactCounts{Computed: impactNone, NotComputed: impactStaleNotComputed}) {
		t.Errorf("a moved deployment default = %+v, %v; want none/stale", out.counts(), err)
	}
	// A membership add, removal or promotion, and a unit deletion, are not modelled by the tree dry run.
	for _, op := range []struct {
		kind, op string
		tree     bool
	}{{govKindUnit, "create", true}, {govKindUnit, "update", true}, {govKindUnit, "delete", false},
		{govKindMembership, "move", true}, {govKindMembership, "upsert", false}, {govKindMembership, "delete", false},
		{govKindMembership, "promote", false}} {
		if got := treeModels(types.GovernanceChange{TargetKind: op.kind, Op: op.op}); got != op.tree {
			t.Errorf("treeModels(%s %s) = %v, want %v", op.kind, op.op, got, op.tree)
		}
	}
	ch.DeploymentHash, ch.TargetKind, ch.Op = computeETag(s2.cfg.DefaultPolicy), govKindMembership, "upsert"
	out, err = s2.changeImpactOf(httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx), ch)
	if err != nil || out.counts() != (impactCounts{Computed: impactNone, NotComputed: impactKindNotComputed}) || out.People != nil {
		t.Errorf("a membership add = %+v, %v; want none/kind", out.counts(), err)
	}
	people := []treeDryRunPerson{{}, {}}
	total := 7
	full := changeImpact{Computed: impactTree, People: &people, LiveRunsTotal: &total, Activates: []unitActivation{{}}}
	if c := full.counts(); c != (impactCounts{Computed: impactTree, People: 2, LiveRunsTotal: 7, Activates: 1}) {
		t.Errorf("counts = %+v", c)
	}
}
