// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

// AuditFilter renders twice — SQL for the Pager path, Go for the fetch-all
// fallback — and a predicate the two disagree on answers a filtered request
// with the wrong events on one of them. This pins the equivalence on a real
// server: for every filter combination, the paged SQL answer and the Go filter
// over the unfiltered feed must return the same ids in the same order.
//
// Guarded by WARDYN_TEST_PG: skipped cleanly when unset, must PASS when set.

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestPG_AuditFilterSQLAgreesWithGo(t *testing.T) {
	pool := throwawayDatabase(t)
	ctx := context.Background()
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pg := store.NewPG(pool)

	base := time.Now().UTC().Truncate(time.Second)
	runA, runB := uuid.New(), uuid.New()
	// "runA.probe" is there for ActionPrefix "run_": a LIKE rendering would
	// read the underscore as a wildcard and match it; starts_with and Go do not.
	actions := []string{"run.kill", "run.create", "runA.probe", "credential.mint", "credential.revoke", "egress.deny", "secret.write"}
	actors := []string{"alice@corp.example", "Alice@corp.example", "bob@corp.example", "wardynd"}
	actorTypes := []types.ActorType{types.ActorHuman, types.ActorAgent, types.ActorSystem}
	outcomes := []string{"success", "failure", "denied"}
	const n = 200
	for i := 0; i < n; i++ {
		var run *uuid.UUID
		switch i % 5 {
		case 0:
			run = &runA
		case 1:
			run = &runB
		}
		ev := types.AuditEvent{
			ID: uuid.New(),
			// Times deliberately out of insertion order, so an ORDER BY time
			// where seq is meant (or the reverse) changes the answer.
			Time:      base.Add(time.Duration((i*37)%n) * time.Minute),
			RunID:     run,
			ActorType: actorTypes[i%len(actorTypes)],
			Actor:     actors[(i/3)%len(actors)],
			Action:    actions[(i*7)%len(actions)],
			Outcome:   outcomes[(i/2)%len(outcomes)],
		}
		if err := store.InsertAuditEvent(ctx, pool, &ev); err != nil {
			t.Fatalf("seed event %d: %v", i, err)
		}
	}

	// Each boundary sits exactly on a seeded event's time: Since is inclusive,
	// Until exclusive, and both renderings must say so.
	since, until := base.Add(40*time.Minute), base.Add(150*time.Minute)
	var filters []store.AuditFilter
	for _, s := range []time.Time{{}, since} {
		for _, u := range []time.Time{{}, until} {
			for _, action := range []string{"", "run.kill"} {
				for _, prefix := range []string{"", "credential.", "run_"} {
					for _, actor := range []string{"", "alice@corp.example"} {
						for _, at := range []types.ActorType{"", types.ActorHuman} {
							for _, outcome := range []string{"", "failure"} {
								filters = append(filters, store.AuditFilter{
									Since: s, Until: u, Action: action, ActionPrefix: prefix,
									Actor: actor, ActorType: at, Outcome: outcome,
								})
							}
						}
					}
				}
			}
		}
	}

	recent, err := pg.QueryRecentAuditEvents(ctx, 0)
	if err != nil {
		t.Fatalf("QueryRecentAuditEvents: %v", err)
	}
	if len(recent) != n {
		t.Fatalf("the unfiltered feed returned %d events, want all %d seeded", len(recent), n)
	}
	trails := map[uuid.UUID][]types.AuditEvent{}
	for _, run := range []uuid.UUID{runA, runB} {
		if trails[run], err = pg.QueryAuditEvents(ctx, run, 0); err != nil {
			t.Fatalf("QueryAuditEvents(%s): %v", run, err)
		}
	}

	nonEmpty := 0
	for _, f := range filters {
		want := eventIDs(f.Keep(recent))
		if len(want) > 0 {
			nonEmpty++
		}
		if got := pagedIDs(t, pg, nil, f); !slices.Equal(got, want) {
			t.Errorf("global feed, filter %+v:\n SQL (paged) = %d ids %v\n Go          = %d ids %v", f, len(got), got, len(want), want)
		}
		for run, trail := range trails {
			want := eventIDs(f.Keep(trail))
			if got := pagedIDs(t, pg, &run, f); !slices.Equal(got, want) {
				t.Errorf("run %s, filter %+v:\n SQL (paged) = %v\n Go          = %v", run, f, got, want)
			}
		}
	}
	// Agreement on empty answers alone would prove nothing.
	if nonEmpty < len(filters)/4 {
		t.Fatalf("only %d of %d filters matched anything: the seed does not exercise the predicates", nonEmpty, len(filters))
	}
}

// pagedIDs walks QueryAuditEventsFilteredPage in small pages, so the answer
// crosses page boundaries the way the export does.
func pagedIDs(t *testing.T, pg store.PG, run *uuid.UUID, f store.AuditFilter) []string {
	t.Helper()
	const size = 7
	var out []string
	for offset := 0; ; offset += size {
		page, err := pg.QueryAuditEventsFilteredPage(context.Background(), run, f, store.Page{Limit: size, Offset: offset})
		if err != nil {
			t.Fatalf("QueryAuditEventsFilteredPage(%+v, offset %d): %v", f, offset, err)
		}
		out = append(out, eventIDs(page)...)
		if len(page) < size {
			return out
		}
	}
}

func eventIDs(evs []types.AuditEvent) []string {
	out := make([]string, 0, len(evs))
	for _, ev := range evs {
		out = append(out, ev.ID.String())
	}
	return out
}
