// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The sponsor-reach half of the #1508 probe table: the service page's section 8 (and section 4 for the service's
// own token) against a service identity's run, for every caller of the issue that can hold a run credential.
// Admin callers have "normal admin access" (section 8), which is the tier each route already declares in
// routeMatrix; that tier is the reference, not an expectation written from the handlers.

type reachKind int

const (
	reachPast    reachKind = iota // the call got through the gate: anything but the foreign-run 404, 401 or 403 (a 501 is the handler running)
	reachForeign                  // the foreign-run 404 a member gets today
	reachScope                    // 403 service_scope
	reachRefused                  // not a 2xx, and nothing was done
	reachOK                       // the exact success status
)

type reachWant struct {
	kind reachKind
	code int
}

var reachCallers = []string{cSponsor, cSponsorPlus, cTarget, cOther, cAdmin, cSecurity, cSuper, cAdminToken, cWdn, cWdg, cService}

// reachRow is one route of section 8 with the answer for each caller.
type reachRow struct {
	ref    string
	route  string
	suffix string
	rows   map[string]reachWant
}

// adminTier is what a route's declared owner tier lets the admin callers do on a run that is not theirs.
func adminTier(route string, past, denied reachWant) map[string]reachWant {
	super, security := denied, denied
	switch routeMatrix[route].ownerTier {
	case tierSecurity:
		super, security = past, past
	case tierSuper:
		super = past
	}
	return map[string]reachWant{cAdmin: super, cSuper: super, cSecurity: security, cAdminToken: super} // the admin token is the operator, as the super admin is
}

func reachRows() []reachRow {
	foreign := reachWant{kind: reachForeign}
	scope := reachWant{kind: reachScope}
	past := reachWant{kind: reachPast}
	// readLike: the sponsor and the sponsor's own bearer pass, the service passes on its own run, a portal's
	// token does not (the F3 ruling, REPORT-C4-fix-r1), everyone else is foreign.
	build := func(route string, sponsor, service, wdn, wdg reachWant, tier map[string]reachWant) map[string]reachWant {
		rows := map[string]reachWant{
			cSponsor: sponsor, cSponsorPlus: sponsor, cTarget: foreign, cOther: foreign,
			cWdn: wdn, cWdg: wdg, cService: service,
		}
		for c, w := range tier {
			rows[c] = w
		}
		return rows
	}
	var out []reachRow
	for _, suffix := range []string{"", "/output", "/events"} {
		route := "GET /api/v1/runs/{id}" + suffix
		out = append(out, reachRow{"service 8 (read: always, while the current sponsor); 4 (its own runs)", route, suffix,
			build(route, past, past, past, foreign, adminTier(route, past, foreign))})
	}
	kill := "POST /api/v1/runs/{id}/kill"
	ok202 := reachWant{kind: reachOK, code: http.StatusAccepted}
	out = append(out, reachRow{"service 8 (kill: always, while the current sponsor); 4 (manage its own runs)", kill, "/kill",
		build(kill, ok202, ok202, ok202, foreign, adminTier(kill, ok202, foreign))})
	// Never: files, resources, grants, run tokens, policy, profile, holder, take-over. 8 names the first four
	// and "run tokens"; the rest are the same ownsRunOrAdmin seam. The service is off its allow-list for all.
	for _, r := range []struct{ route, suffix string }{
		{"GET /api/v1/runs/{id}/files", "/files"}, {"GET /api/v1/runs/{id}/resources", "/resources"},
		{"GET /api/v1/runs/{id}/grants", "/grants"}, {"GET /api/v1/runs/{id}/ado-tokens", "/ado-tokens"},
		{"GET /api/v1/runs/{id}/policy", "/policy"}, {"POST /api/v1/runs/{id}/profile", "/profile"},
		{"GET /api/v1/runs/{id}/attach-holder", "/attach-holder"}, {"POST /api/v1/runs/{id}/attach/takeover", "/attach/takeover"},
	} {
		out = append(out, reachRow{"service 8 (files, grants, profile, resources, run tokens: never)", r.route, r.suffix,
			// A portal's token is refused on these (they are off its allow-list) before the run is looked up.
			build(r.route, foreign, scope, foreign, reachWant{kind: reachRefused}, adminTier(r.route, reachWant{kind: reachPast}, reachWant{kind: reachRefused}))})
	}
	// Attach: only with the explicit grant, through the run's doors; no entry for an admin without it (8).
	ticket := "POST /api/v1/runs/{id}/attach-ticket"
	out = append(out, reachRow{"service 8 (attach: only with an explicit service_attach grant); 4 (attach absent from the service's list)", ticket, "/attach-ticket", map[string]reachWant{
		cSponsor: foreign, cSponsorPlus: {kind: reachOK, code: http.StatusOK}, cTarget: foreign, cOther: foreign,
		cAdmin: {kind: reachRefused}, cSecurity: {kind: reachRefused}, cSuper: {kind: reachRefused}, cAdminToken: {kind: reachRefused},
		cWdn: foreign, cWdg: foreign, cService: scope,
	}})
	return out
}

// TestF1508Probe_SponsorReach runs section 8 against a service identity's run, for every caller.
func TestF1508Probe_SponsorReach(t *testing.T) {
	e := newProbeEnv(t)
	ts := httptest.NewServer(panicFails(t, e.h.srv.Handler()))
	defer ts.Close()
	for _, row := range reachRows() {
		if len(row.rows) != len(reachCallers) {
			t.Fatalf("%s has %d callers, want %d", row.route, len(row.rows), len(reachCallers))
		}
		for _, c := range reachCallers {
			w := row.rows[c]
			t.Run(row.route+" / "+c, func(t *testing.T) { e.runReach(t, ts, row, c, w) })
		}
	}
	// The approval route: the sponsor never decides a hold; an admin does (section 8).
	for _, c := range []string{cSponsor, cSponsorPlus, cTarget, cOther, cWdn, cWdg, cService} {
		t.Run("POST /api/v1/approvals/{id}/approve / "+c, func(t *testing.T) {
			run := e.seedRun(t, e.ownerFor(c), nil)
			approval := seedEgressApproval(e.h, run.ID, "pypi.org")
			got := e.callAs(t, c, "sponsor", http.MethodPost, "/api/v1/approvals/"+approval.String()+"/approve", `{}`)
			if got.Code/100 == 2 {
				t.Errorf("%s decided a held request: %d %s, want a refusal", c, got.Code, got.Body.String())
			}
			if ap := e.h.approvals.byID[approval]; ap.State == types.ApprovalApproved {
				t.Errorf("%s: the hold was approved", c)
			}
		})
	}
}

// ownerFor is the run owner a caller's cell runs against: the second sponsor's own service, else ci-bot.
func (e probeEnv) ownerFor(caller string) string {
	if caller == cSponsorPlus {
		return plusPrn
	}
	return svcPrin
}

func (e probeEnv) runReach(t *testing.T, ts *httptest.Server, row reachRow, caller string, w reachWant) {
	t.Helper()
	run := e.seedRun(t, e.ownerFor(caller), nil)
	m := method(row.route)
	got := e.callAs(t, caller, "sponsor", m, "/api/v1/runs/"+run.ID.String()+row.suffix, "")
	body := strings.TrimSpace(got.Body.String())
	switch w.kind {
	case reachForeign:
		if !refusedAsForeign(got) {
			t.Errorf("%s: %d %s, want the foreign-run 404", caller, got.Code, body)
		}
	case reachScope:
		if got.Code != http.StatusForbidden || respReason(got) != "service_scope" {
			t.Errorf("%s: %d %s, want 403 service_scope", caller, got.Code, body)
		}
	case reachRefused:
		if got.Code/100 == 2 || got.Code >= 500 {
			t.Errorf("%s: %d %s, want a refusal", caller, got.Code, body)
		}
	case reachPast:
		if refusedAsForeign(got) || got.Code == http.StatusUnauthorized || got.Code == http.StatusForbidden {
			t.Errorf("%s: %d %s, want past the gate", caller, got.Code, body)
		}
	case reachOK:
		if got.Code != w.code {
			t.Errorf("%s: %d %s, want %d", caller, got.Code, body, w.code)
		}
	}
	if strings.Contains(row.route, "attach-ticket") {
		e.checkTicket(t, ts, run, caller, got)
	}
	if strings.HasSuffix(row.route, "/kill") {
		after, err := e.st.GetRun(context.Background(), run.ID)
		if err != nil {
			t.Fatal(err)
		}
		killed := after.State == types.RunKilled
		if killed != (got.Code == http.StatusAccepted) {
			t.Errorf("%s: kill answered %d and the run is %s", caller, got.Code, after.State)
		}
	}
	// A refusal that is not a success carries no run data; a success for a stranger would show here.
	if got.Code/100 == 2 && bearerShaped.MatchString(got.Body.String()) && !strings.Contains(row.route, "attach-ticket") {
		t.Errorf("%s: %d carries a bearer-shaped value: %s", caller, got.Code, got.Body.String())
	}
}

// checkTicket holds the attach rows of section 8: no ticket without the grant (and the denial names why), and a
// ticket that is granted opens a session attributed to the sponsor, as a human, naming the service and the entry.
func (e probeEnv) checkTicket(t *testing.T, ts *httptest.Server, run types.AgentRun, caller string, got *httptest.ResponseRecorder) {
	t.Helper()
	if got.Code != http.StatusOK {
		if strings.Contains(got.Body.String(), `"ticket"`) {
			t.Errorf("%s: a refused ticket mint carries a ticket: %s", caller, got.Body.String())
		}
		if caller == cSponsor || caller == cWdn {
			if reasons := e.denialReasons(run.ID); len(reasons) != 1 || reasons[0] != "capability_feature" {
				t.Errorf("%s: authz.denied reasons = %v, want [capability_feature]", caller, reasons)
			}
		}
		return
	}
	tok := ticketOf(t, got)
	dctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(dctx, "ws"+strings.TrimPrefix(ts.URL, "http")+"/api/v1/runs/"+run.ID.String()+"/attach?ticket="+tok, nil)
	if err != nil {
		t.Fatalf("%s: attach with the ticket the grant allowed: %v", caller, err)
	}
	defer func() { _ = conn.CloseNow() }()
	ev := waitForRecAudit(t, e.h.audit, run.ID, "session.attach", "success")
	if ev.ActorType != types.ActorHuman || ev.Actor != svcSponsor2 || !strings.Contains(string(ev.Data), plusBot) || !strings.Contains(string(ev.Data), `"sponsor"`) {
		t.Errorf("session.attach row = %s %s %s, want the sponsor as a human naming %s and the sponsor entry", ev.ActorType, ev.Actor, ev.Data, plusBot)
	}
}
