// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
)

// sponsorReachRoutes are the six routes a service identity's sponsor reaches (F3): the handler that asks
// ownsRunOrSponsorOrAdmin, and the route line that mounts it. GET /runs has no per-run check: its handler
// lists through the store's sponsor-aware pager, which the second test pins.
var sponsorReachRoutes = map[string]string{
	"handleGetRun":    `r.Get("/runs/{id}", s.handleGetRun)`,
	"handleRunEvents": `r.Get("/runs/{id}/events", s.handleRunEvents)`,
	"handleRunOutput": `r.Get("/runs/{id}/output", s.handleRunOutput)`,
	"handleKillRun":   `r.Post("/runs/{id}/kill", s.handleKillRun)`,
	// The containment hold is part of what run detail shows; its pod and node identities stay with security operators.
	"handleGetRunContainment": `r.Get("/runs/{id}/containment", s.handleGetRunContainment)`,
}

// sponsorReachHelpers are the helpers a handler above asks ownsRunOrSponsorOrAdmin through: the handler, its one
// caller, and how many times the helper asks it. runEventsOpeningRun is handleRunEvents' read gate, split out so an
// issuer portal's stream reads its run under its opening's bound; it asks once on each of its two paths, that
// stream's and every other's (getRunAuthorizedBy).
var sponsorReachHelpers = map[string]struct {
	handler string
	asks    int
}{
	"runEventsOpeningRun": {"handleRunEvents", 2},
}

// TestSponsorReach_RouteCensus: ownsRunOrSponsorOrAdmin is asked by exactly the five per-run handlers above, once
// each (directly or through their helper in sponsorReachHelpers), and by nothing else; those handlers are mounted where the design says. A new caller is a new route a sponsor
// reaches, and has to be classified here before it merges.
func TestSponsorReach_RouteCensus(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	found, direct, redeem, entry := map[string]int{}, map[string]int{}, map[string]int{}, map[string]int{}
	callers := map[string]map[string]int{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok {
					switch sel.Sel.Name {
					case "ownsRunOrSponsorOrAdmin":
						found[fn.Name.Name]++
					case "sponsorsRun":
						direct[fn.Name.Name]++
					case "mayEnterRunOrSponsor":
						redeem[fn.Name.Name]++
					case "sponsorEntry":
						entry[fn.Name.Name]++
					default:
						if _, ok := sponsorReachHelpers[sel.Sel.Name]; ok {
							if callers[sel.Sel.Name] == nil {
								callers[sel.Sel.Name] = map[string]int{}
							}
							callers[sel.Sel.Name][fn.Name.Name]++
						}
					}
				}
				return true
			})
		}
	}
	for helper, h := range sponsorReachHelpers {
		if found[helper] != h.asks {
			t.Errorf("%s asks ownsRunOrSponsorOrAdmin %d times, want %d", helper, found[helper], h.asks)
		}
		if want := map[string]int{h.handler: 1}; !maps.Equal(callers[helper], want) {
			t.Errorf("%s is called by %v, want %v: a helper is one handler's read gate, not a route of its own", helper, callers[helper], want)
		}
		found[h.handler]++ // the handler asks it through the helper
		delete(found, helper)
	}
	for fn, route := range sponsorReachRoutes {
		if found[fn] != 1 {
			t.Errorf("%s asks ownsRunOrSponsorOrAdmin %d times, want once", fn, found[fn])
		}
		delete(found, fn)
		if src, err := os.ReadFile("routes.go"); err != nil || !strings.Contains(string(src), route) {
			t.Errorf("routes.go does not mount %s (%v)", route, err)
		}
	}
	// sponsorsRun itself is asked only by the predicate above and by the interactive entry rule (admitRunEntry,
	// the ticket mint's and a takeover's), whose load admits a sponsor so that its entry check can refuse by
	// capability. loadTakeover asks it only to send a sponsor on to that rule rather than refuse them as a
	// stranger; it admits nothing itself. Any other caller is a new sponsor door.
	if want := map[string]int{"ownsRunOrSponsorOrAdmin": 1, "admitRunEntry": 1, "loadTakeover": 1, "sponsorMayEnter": 1}; !maps.Equal(direct, want) {
		t.Errorf("sponsorsRun is asked by %v, want %v", direct, want)
	}
	// mayEnterRunOrSponsor admits a sponsor on a redeemed ticket or an open UI session without a delegation
	// check of its own: the portal refusal holds because the one ticket mint refuses. A new redemption site
	// is a new way in, and has to be classified here. uiTicketMayEnter is the gateway enter's redemption
	// (uiEnterCommon's, moved there beside the inbound caller's ticket, which never takes this arm).
	// inboundCaller redeems an inbound token, with no role (superAdmin false); a portal's delegated token
	// cannot mint one (mintGuards' delegation refusal, on both token mints), so the portal refusal holds there too.
	wantRedeem := map[string]int{"refuseAttachEntry": 1, "uiTicketMayEnter": 1, "inboundCaller": 1}
	if !maps.Equal(redeem, wantRedeem) {
		t.Errorf("mayEnterRunOrSponsor is asked by %v, want %v", redeem, wantRedeem)
	}
	// sponsorEntry is mayEnterRunOrSponsor for a re-check: it returns the sponsor read's error, so an open session
	// rides the session budget on a read that failed. Its callers are the two re-checks and the predicate itself.
	// uiSessionArms is the open session's re-check, after its portal's arm (uiSessionAuthority, its one caller).
	wantEntry := map[string]int{"mayEnterRunOrSponsor": 1, "attachAuthority": 1, "uiSessionArms": 1}
	if !maps.Equal(entry, wantEntry) {
		t.Errorf("sponsorEntry is asked by %v, want %v", entry, wantEntry)
	}
	if len(found) != 0 {
		t.Errorf("ownsRunOrSponsorOrAdmin is also asked by %v: a route a sponsor now reaches", slices.Sorted(func(yield func(string) bool) {
			for k := range found {
				if !yield(k) {
					return
				}
			}
		}))
	}
}

// TestSponsorReach_ListIsSponsorAware: GET /runs lists through the sponsor-aware pager on both its paths, and the
// run routes that must stay the owner's or an admin's still ask the unchanged predicate.
func TestSponsorReach_ListIsSponsorAware(t *testing.T) {
	for file, want := range map[string]string{
		"runs_policy.go":         "RunsByCreatorOrSponsorPager",
		"runs_list_filter.go":    "WithSponsored",
		"run_files.go":           "getRunAuthorized(",
		"approvals_decidable.go": "ownsRunOrAdmin(r, run)",
	} {
		src, err := os.ReadFile(file)
		if err != nil || !strings.Contains(string(src), want) {
			t.Errorf("%s no longer contains %q (%v)", file, want, err)
		}
	}
}
