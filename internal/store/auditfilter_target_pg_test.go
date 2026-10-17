// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

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

// ?target=<person> finds a person-target row in both forms it can hold: the
// name (written before WARDYN_AUDIT_SEAL=full was turned on) and the subject
// (written after). The SQL and the Go rendering must agree on both.
func TestPG_AuditFilter_TargetMatchesBothForms(t *testing.T) {
	pool := throwawayDatabase(t)
	ctx := context.Background()
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pg := store.NewPG(pool)

	const subject = "subject:5b1f0c7e-8a2d-4c55-9f0e-2d7a0b6c1e11"
	base := time.Now().UTC().Truncate(time.Second)
	seed := map[string]string{ // label -> target
		"before": "alice@corp.example", "after": subject, "other": "bob@corp.example", "otherSubject": "subject:0a0a0a0a-0000-4000-8000-000000000001", "none": "",
	}
	ids := map[string]string{}
	i := 0
	for _, label := range []string{"before", "other", "after", "otherSubject", "none"} {
		ev := types.AuditEvent{
			ID: uuid.New(), Time: base.Add(time.Duration(i) * time.Minute), ActorType: types.ActorHuman, Actor: "admin",
			Action: "person.token.create", Target: seed[label], Outcome: "success",
		}
		if err := store.InsertAuditEvent(ctx, pool, &ev); err != nil {
			t.Fatalf("seed %s: %v", label, err)
		}
		ids[label] = ev.ID.String()
		i++
	}
	recent, err := pg.QueryRecentAuditEvents(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}

	for name, tc := range map[string]struct {
		f    store.AuditFilter
		want []string
	}{
		"both forms":             {store.AuditFilter{Target: "alice@corp.example", TargetAlt: subject}, []string{"after", "before"}},
		"the name alone":         {store.AuditFilter{Target: "alice@corp.example"}, []string{"before"}},
		"a person with no alias": {store.AuditFilter{Target: "bob@corp.example"}, []string{"other"}},
		"no target filter":       {store.AuditFilter{Action: "person.token.create"}, []string{"none", "otherSubject", "after", "other", "before"}},
	} {
		var want []string
		for _, l := range tc.want {
			want = append(want, ids[l])
		}
		if got := pagedIDs(t, pg, nil, tc.f); !slices.Equal(got, want) {
			t.Errorf("%s: SQL (paged) = %v, want %v", name, got, want)
		}
		if got := eventIDs(tc.f.Keep(recent)); !slices.Equal(got, want) {
			t.Errorf("%s: Go filter = %v, want %v", name, got, want)
		}
	}
}
