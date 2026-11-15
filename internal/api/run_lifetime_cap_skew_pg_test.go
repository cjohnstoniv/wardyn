// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A person launches into a pool whose Background lifetime is at most 2 hours, under an elevation that ends a
// minute before or a minute after that cap as the database counts it, from a replica whose clock is the
// database's, 2 minutes ahead of it or 2 minutes behind it. The stored lifetime_cap_at is the earlier of the two
// on the database's clock and names it; ends_at, cut from the 3 hours asked, is that cap; and the 201, its
// capped-end warning and the run.create row's end all say what Postgres stored and name the limit that set it,
// the elevation too when the ask was inside the pool's cap and only the elevation cut it. Before, the api picked the
// earlier bound on the replica's clock and the insert moved only the winner onto the database's: 2 minutes
// behind, the run outlived its elevation by a minute; 2 minutes ahead, it outlived the pool's cap by a minute.
func TestPG_CreateCap_TheEarlierBoundOnTheDatabasesClock(t *testing.T) {
	const poolMax = 2 * time.Hour
	same := func(a, b *time.Time) bool { return (a == nil) == (b == nil) && (a == nil || a.Equal(*b)) }
	for _, c := range []struct {
		name      string
		skew      time.Duration // the replica's clock less the database's
		elevation time.Duration // the elevation's end less the pool's cap, both on the database's clock
		askSec    int
		want      types.LimitSource
	}{
		{"no skew, the pool's cap first", 0, time.Minute, 3 * 3600, types.LimitSourcePool},
		{"no skew, the elevation first", 0, -time.Minute, 3 * 3600, types.LimitSourceElevation},
		{"replica ahead, the pool's cap first", 2 * time.Minute, time.Minute, 3 * 3600, types.LimitSourcePool},
		{"replica ahead, the elevation first", 2 * time.Minute, -time.Minute, 3 * 3600, types.LimitSourceElevation},
		{"replica behind, the pool's cap first", -2 * time.Minute, time.Minute, 3 * 3600, types.LimitSourcePool},
		{"replica behind, the elevation first", -2 * time.Minute, -time.Minute, 3 * 3600, types.LimitSourceElevation},
		// 30 seconds inside the pool's 2 hours and 30 seconds past the elevation: only the elevation cuts it.
		{"no skew, an ask inside the pool's cap, the elevation first", 0, -time.Minute, 2*3600 - 30, types.LimitSourceElevation},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := newPlacementEnv(t)
			ctx := context.Background()
			pool := e.lifetimePool("Two hours", `{"default_sec":3600,"max_sec":7200}`)
			replica := func() time.Time { return time.Now().Add(c.skew) }
			pg := e.pg
			pg.Now = replica
			e.srv.cfg.Now, e.srv.cfg.Store = replica, pg
			dbNow, err := pg.DatabaseNow(ctx)
			if err != nil {
				t.Fatal(err)
			}
			elevationEnd := dbNow.Add(poolMax + c.elevation).Truncate(time.Microsecond)
			unit := treeChild(t, e.govEnv, uaUnitBody("cap-skew", store.DefaultUnitID, map[string]any{}, nil))
			if err := store.InsertGovernanceMembershipQ(ctx, e.pool, types.GovernanceMembership{ID: uuid.New(), SubjectType: types.CapabilitySubjectUser,
				Subject: types.CanonicalUserSubject(e.personP), UnitID: unit.ID, CreatedAt: dbNow, CreatedBy: "seed", ExpiresAt: &elevationEnd}); err != nil {
				t.Fatal(err)
			}
			w := e.call(e.person, http.MethodPost, "/api/v1/runs", endBody("background",
				fmt.Sprintf(`"runner_pool_id":%q,"run_in":%q,"ends_after_sec":%d`, pool.ID, unit.ID, c.askSec)))
			if w.Code != http.StatusCreated {
				t.Fatalf("create = %d %s", w.Code, w.Body)
			}
			var resp struct {
				types.AgentRun
				Warnings []string `json:"warnings"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			row, err := e.pg.GetRun(ctx, resp.ID)
			if err != nil {
				t.Fatal(err)
			}

			if row.CreatedAt.Before(dbNow.Add(-30*time.Second)) || row.CreatedAt.After(dbNow.Add(30*time.Second)) {
				t.Fatalf("created_at %v, want on the database's clock near %v", row.CreatedAt, dbNow)
			}
			capOK := spanFromAPI(row.CreatedAt, row.LifetimeCapAt, poolMax) && row.LifetimeCapAt.Before(elevationEnd)
			if c.want == types.LimitSourceElevation {
				capOK = row.LifetimeCapAt != nil && row.LifetimeCapAt.Equal(elevationEnd)
			}
			if !capOK || row.LifetimeCapSource != c.want {
				t.Fatalf("stored cap %v (%q), created %v; want the %s's (the pool's: created_at + %v; the elevation's: %v)",
					row.LifetimeCapAt, row.LifetimeCapSource, row.CreatedAt, c.want, poolMax, elevationEnd)
			}
			if !same(row.EndsAt, row.LifetimeCapAt) {
				t.Fatalf("stored end %v, want the cap %v: the %d s asked are cut to it", row.EndsAt, row.LifetimeCapAt, c.askSec)
			}

			if !same(resp.EndsAt, row.EndsAt) || !same(resp.LifetimeCapAt, row.LifetimeCapAt) || resp.LifetimeCapSource != row.LifetimeCapSource {
				t.Errorf("201 says end %v, cap %v (%q); Postgres stored %v, %v (%q)",
					resp.EndsAt, resp.LifetimeCapAt, resp.LifetimeCapSource, row.EndsAt, row.LifetimeCapAt, row.LifetimeCapSource)
			}
			limit := pool.Name
			if c.want == types.LimitSourceElevation {
				limit = "your elevation"
			}
			stamp := "past what " + limit + " allows, so the run ends at " + row.EndsAt.UTC().Format(time.RFC3339) + " instead"
			if !slices.ContainsFunc(resp.Warnings, func(s string) bool { return strings.HasSuffix(s, stamp) }) {
				t.Errorf("warnings %q, want the capped end at the stored %v, naming %s", resp.Warnings, row.EndsAt, limit)
			}
			var end map[string]any
			for _, ev := range e.h.audit.snapshot() {
				if ev.Action == "run.create" && ev.RunID != nil && *ev.RunID == row.ID {
					end, _ = queueAuditData(t, ev)["end"].(map[string]any)
				}
			}
			applied, err := time.Parse(time.RFC3339Nano, fmt.Sprint(end["applied"]))
			if err != nil || !applied.Equal(*row.EndsAt) || end["asked_sec"] != float64(c.askSec) || end["capped"] != true || end["source"] != limit {
				t.Errorf("run.create end %v, want applied at the stored %v, the %d s asked, capped and bound by %s", end, row.EndsAt, c.askSec, limit)
			}
		})
	}
}
