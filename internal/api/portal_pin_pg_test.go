// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"testing"

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
		w = e.get(t, tok, "/api/v1/runs?status=all&view=mine")
		if h := w.Header().Get("X-Wardyn-Hidden-Older"); h != "0" {
			t.Errorf("hidden count = %q, want 0 (no run is counted)", h)
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

// 3 x page size runs alternate in and out of the pin: every page is full, the cursors reach every in-pin run
// exactly once, and the hidden counts count only in-pin runs, on the plain and the filtered list.
func TestPortalPin_ListFilteredToPin_PG(t *testing.T) {
	e := newAuthorityEnv(t)
	d := e.portal(t, e.bind(t), &e.plat.ID)
	person, tok := e.person(t, d, "alice")
	const page = 5
	want := map[uuid.UUID]bool{}
	for i := 0; i < 3*page*2; i++ {
		unit := &e.other.ID
		if i%2 == 0 {
			unit = &e.team.ID
		}
		r := e.run(t, person, unit, d.ID)
		if unit == &e.team.ID {
			want[r.ID] = true
		}
	}
	for _, base := range []string{"/api/v1/runs?", "/api/v1/runs?view=mine&status=all&"} {
		got := map[uuid.UUID]int{}
		for offset := 0; ; offset += page {
			w := e.get(t, tok, base+"limit="+strconv.Itoa(page)+"&offset="+strconv.Itoa(offset))
			runs := decodePortalRuns(t, w.Body.Bytes())
			for _, r := range runs {
				got[r.ID]++
			}
			if len(runs) < page {
				break
			}
			if len(runs) != page {
				t.Fatalf("%s: a page held %d runs, want %d", base, len(runs), page)
			}
		}
		if len(got) != len(want) {
			t.Errorf("%s: reached %d runs, want the %d in the pin", base, len(got), len(want))
		}
		for id, n := range got {
			if !want[id] || n != 1 {
				t.Errorf("%s: run %s seen %d times (in pin %v)", base, id, n, want[id])
			}
		}
	}
}
