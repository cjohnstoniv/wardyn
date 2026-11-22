// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func decodePortalRuns(t *testing.T, body []byte) []types.AgentRun {
	t.Helper()
	var runs []types.AgentRun
	if err := json.Unmarshal(body, &runs); err != nil {
		t.Fatalf("decode runs: %v (%s)", err, body)
	}
	return runs
}

// Two portals on one issuer serve the same person, and each reaches only the runs whose unit is under its own
// pin: the platform portal sees the platform run, the other portal the other run, and neither reads the
// other's run by id (403 portal_unit_scope).
func TestPortalPin_TwoPortalsOneIssuerSamePersonSeparateRuns_PG(t *testing.T) {
	e := newAuthorityEnv(t)
	b := e.bind(t)
	platform, other := e.portal(t, b, &e.plat.ID), e.portal(t, b, &e.other.ID)
	person, platTok := e.person(t, platform, "alice")
	_, otherTok := e.person(t, other, "alice")
	inPlat, inOther := e.run(t, person, &e.team.ID, platform.ID), e.run(t, person, &e.other.ID, other.ID)

	for _, tc := range []struct {
		name, tok     string
		visible, hide types.AgentRun
	}{{"platform", platTok, inPlat, inOther}, {"other", otherTok, inOther, inPlat}} {
		if w := e.get(t, tc.tok, "/api/v1/runs/"+tc.visible.ID.String()); w.Code != http.StatusOK {
			t.Errorf("%s portal reads its own run = %d %s", tc.name, w.Code, w.Body)
		}
		if w := e.get(t, tc.tok, "/api/v1/runs/"+tc.hide.ID.String()); !portalScopeRefused(w) {
			t.Errorf("%s portal reads the other portal's run = %d %s, want 403 portal_unit_scope", tc.name, w.Code, w.Body)
		}
		runs := decodePortalRuns(t, e.get(t, tc.tok, "/api/v1/runs").Body.Bytes())
		if len(runs) != 1 || runs[0].ID != tc.visible.ID {
			t.Errorf("%s portal lists %d runs, want only %s", tc.name, len(runs), tc.visible.ID)
		}
	}
}

// Disjoint pins: a person reached through a portal pinned to one unit can list, read, kill and ticket only the
// runs under that unit, and a second person of the same portal sees none of the first's.
func TestPortalPin_DisjointPinsIsolatedOnEveryOwnerPath_PG(t *testing.T) {
	e := newAuthorityEnv(t)
	b := e.bind(t)
	pa, pb := e.portal(t, b, &e.plat.ID), e.portal(t, b, &e.other.ID)
	person, tokA := e.person(t, pa, "alice")
	_, tokB := e.person(t, pb, "alice")
	runA := e.run(t, person, &e.team.ID, pa.ID)
	for _, route := range []struct{ method, path string }{
		{"GET", "/api/v1/runs/%s"}, {"GET", "/api/v1/runs/%s/events"}, {"GET", "/api/v1/runs/%s/output"},
		{"POST", "/api/v1/runs/%s/kill"}, {"POST", "/api/v1/runs/%s/attach-ticket"},
	} {
		path := fmt.Sprintf(route.path, runA.ID)
		if w := do(t, e.h.srv, route.method, path, tokB, ""); !portalScopeRefused(w) {
			t.Errorf("%s %s through the other portal = %d %s, want 403 portal_unit_scope", route.method, route.path, w.Code, w.Body)
		}
		if w := do(t, e.h.srv, route.method, path, tokA, ""); portalScopeRefused(w) {
			t.Errorf("%s %s through its own portal was refused by the pin: %s", route.method, route.path, w.Body)
		}
	}
}

// The run's unit is the pin unit, which is then deleted: every door refuses (the pin is read from the current
// tree, and onPath would answer true for the deleted id).
func TestPortalPin_DeletedPinUnitRefused_PG(t *testing.T) {
	e := newAuthorityEnv(t)
	// The filtered list (view=user) projects attention, which reads pending approvals from the store.
	e.h.srv.cfg.Approvals = attentionApprovals{ApprovalService: e.h.srv.cfg.Approvals, st: e.st}
	b := e.bind(t)
	pin := e.unit(t, types.GovernanceUnit{Name: "doomed", Ceiling: types.RunPolicySpec{MinConfinementClass: types.CC2}})
	d := e.portal(t, b, &pin.ID)
	person, tok := e.person(t, d, "alice")
	run := e.run(t, person, &pin.ID, d.ID)
	if w := e.get(t, tok, "/api/v1/runs/"+run.ID.String()); w.Code != http.StatusOK {
		t.Fatalf("read before the delete = %d %s", w.Code, w.Body)
	}
	if err := e.st.DeleteGovernanceUnit(context.Background(), pin.ID); err != nil {
		t.Fatalf("delete the pin unit: %v", err)
	}
	t.Run("list and counts", func(t *testing.T) {
		w := e.get(t, tok, "/api/v1/runs")
		if runs := decodePortalRuns(t, w.Body.Bytes()); len(runs) != 0 {
			t.Errorf("list after the delete = %d runs, want none", len(runs))
		}
		w = e.get(t, tok, "/api/v1/runs?view=user")
		if w.Code != http.StatusOK {
			t.Fatalf("filtered list after the delete = %d %s, want 200", w.Code, w.Body)
		}
		for _, h := range []string{"X-Wardyn-Hidden-Older", "X-Wardyn-Hidden-Killed"} {
			if got := w.Header().Get(h); got != "0" {
				t.Errorf("%s = %q, want 0 (no run is counted)", h, got)
			}
		}
		if runs := decodePortalRuns(t, w.Body.Bytes()); len(runs) != 0 {
			t.Errorf("filtered list after the delete = %d runs, want none", len(runs))
		}
	})
	t.Run("direct read", func(t *testing.T) {
		if w := e.get(t, tok, "/api/v1/runs/"+run.ID.String()); !portalScopeRefused(w) {
			t.Errorf("read after the delete = %d %s, want 403 portal_unit_scope", w.Code, w.Body)
		}
	})
	t.Run("ticket", func(t *testing.T) {
		if w := do(t, e.h.srv, "POST", "/api/v1/runs/"+run.ID.String()+"/attach-ticket", tok, ""); !portalScopeRefused(w) {
			t.Errorf("ticket after the delete = %d %s, want 403 portal_unit_scope", w.Code, w.Body)
		}
	})
}

// Live runs alternate in and out of the pin, with older and killed rows hidden by the filtered list inside and
// outside it: every page is full, the cursors reach every in-pin run exactly once, and both hidden counts count
// only the in-pin rows.
func TestPortalPin_ListFilteredToPin_PG(t *testing.T) {
	e := newAuthorityEnv(t)
	e.h.srv.cfg.Approvals = attentionApprovals{ApprovalService: e.h.srv.cfg.Approvals, st: e.st}
	d := e.portal(t, e.bind(t), &e.plat.ID)
	person, tok := e.person(t, d, "alice")
	const page = 5
	want := map[uuid.UUID]bool{}
	for i := 0; i < 3*page*2; i++ {
		unit := &e.other.ID
		if i%2 == 0 {
			unit = &e.team.ID
		}
		r := e.runAs(t, person, unit, d.ID, types.RunRunning, 0, time.Time{})
		if unit == &e.team.ID {
			want[r.ID] = true
		}
	}
	hide := func(unit *uuid.UUID, n int, state types.RunState, ago time.Duration) {
		for range n {
			e.runAs(t, person, unit, d.ID, state, ago, time.Time{})
		}
	}
	hide(&e.team.ID, 2, types.RunCompleted, 10*24*time.Hour) // older than ended_within=7d, in the pin
	hide(&e.other.ID, 3, types.RunCompleted, 10*24*time.Hour)
	hide(&e.team.ID, 3, types.RunKilled, 72*time.Hour) // killed more than a day ago, in the pin
	hide(&e.other.ID, 2, types.RunKilled, 72*time.Hour)

	count := func(w *httptest.ResponseRecorder, name string) int {
		t.Helper()
		n, err := strconv.Atoi(w.Header().Get(name))
		if err != nil {
			t.Fatalf("header %s = %q on %d %s", name, w.Header().Get(name), w.Code, w.Body)
		}
		return n
	}
	for _, c := range []struct {
		query         string
		older, killed int
	}{{"", 0, 3}, {"&ended_within=7d", 2, 3}} {
		w := e.get(t, tok, "/api/v1/runs?view=user"+c.query+"&limit=5")
		if w.Code != http.StatusOK {
			t.Fatalf("filtered list %q = %d %s, want 200", c.query, w.Code, w.Body)
		}
		if o, k := count(w, "X-Wardyn-Hidden-Older"), count(w, "X-Wardyn-Hidden-Killed"); o != c.older || k != c.killed {
			t.Errorf("filtered list %q hidden older %d killed %d, want %d and %d (in-pin rows only)", c.query, o, k, c.older, c.killed)
		}
	}
	got := map[uuid.UUID]int{}
	for offset := 0; ; offset += page {
		w := e.get(t, tok, "/api/v1/runs?view=user&ended_within=7d&limit="+strconv.Itoa(page)+"&offset="+strconv.Itoa(offset))
		if w.Code != http.StatusOK {
			t.Fatalf("filtered page at offset %d = %d %s, want 200", offset, w.Code, w.Body)
		}
		runs := decodePortalRuns(t, w.Body.Bytes())
		for _, r := range runs {
			got[r.ID]++
		}
		if len(runs) < page {
			break
		}
		if len(runs) != page {
			t.Fatalf("a page held %d runs, want %d", len(runs), page)
		}
	}
	if len(got) != len(want) {
		t.Errorf("reached %d runs, want the %d in the pin", len(got), len(want))
	}
	for id, n := range got {
		if !want[id] || n != 1 {
			t.Errorf("run %s seen %d times (in pin %v)", id, n, want[id])
		}
	}
}

// The plain list pages on an offset, so runs created at one instant need a defined order: with every run's
// creation time equal, each in-pin run is reached exactly once across the page boundaries (the creator query
// breaks the tie on id, as the filtered path does).
func TestPortalPin_PlainListTiedTimesPageExactlyOnce_PG(t *testing.T) {
	e := newAuthorityEnv(t)
	d := e.portal(t, e.bind(t), &e.plat.ID)
	person, tok := e.person(t, d, "alice")
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	want := map[uuid.UUID]bool{}
	for i := 0; i < 12; i++ {
		unit := &e.other.ID
		if i%3 != 0 {
			unit = &e.team.ID
		}
		r := e.runAs(t, person, unit, d.ID, types.RunRunning, 0, at)
		if unit == &e.team.ID {
			want[r.ID] = true
		}
	}
	const page = 3
	got := map[uuid.UUID]int{}
	for offset := 0; ; offset += page {
		w := e.get(t, tok, "/api/v1/runs?limit="+strconv.Itoa(page)+"&offset="+strconv.Itoa(offset))
		if w.Code != http.StatusOK {
			t.Fatalf("plain page at offset %d = %d %s, want 200", offset, w.Code, w.Body)
		}
		runs := decodePortalRuns(t, w.Body.Bytes())
		for _, r := range runs {
			got[r.ID]++
		}
		if len(runs) < page {
			break
		}
	}
	if len(got) != len(want) {
		t.Errorf("plain list reached %d runs, want the %d in the pin", len(got), len(want))
	}
	for id, n := range got {
		if !want[id] || n != 1 {
			t.Errorf("run %s seen %d times (in pin %v)", id, n, want[id])
		}
	}
}
