// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The subject form of a person filter (TargetAlt) names the organisation's own
// person-target rows only. A device-forwarded row keeps its claimed target
// verbatim, so one claiming the subject form must not be listed under that
// person; a filter on the claimed string itself (Target) still finds it. The
// SQL and the Go rendering must agree.
func TestPG_AuditFilter_TargetAltMatchesOrganisationRowsOnly(t *testing.T) {
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	st := store.NewPG(pool)
	d := federationDevice(t, st)
	action := "person.token.create." + uuid.NewString()
	const name = "alice@corp.example"
	subject := "subject:" + uuid.NewString()

	// The organisation's own row for the person, in the sealed (subject) form.
	org := types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond),
		ActorType: types.ActorHuman, Actor: "admin", Action: action, Target: subject, Outcome: "success"}
	if err := store.InsertAuditEvent(ctx, pool, &org); err != nil {
		t.Fatal(err)
	}
	// A device-forwarded row claiming the same subject target.
	fed := types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Microsecond),
		ActorType: types.ActorHuman, Actor: "admin", Action: action, Target: subject, Outcome: "success"}
	fed.RowHash = auditRowHash(t, pool, "", fed)
	if _, err := st.IngestDeviceAudit(ctx, d.ID, testPeer, []types.FederatedAuditEvent{{AuditEvent: fed, Seq: 1}}); err != nil {
		t.Fatalf("ingest: %v", err)
	}

	paged := func(f store.AuditFilter) []uuid.UUID {
		t.Helper()
		f.Action = action
		got, err := st.QueryAuditEventsFilteredPage(ctx, nil, f, store.Page{Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		ids := make([]uuid.UUID, len(got))
		for i, e := range got {
			ids[i] = e.ID
		}
		return ids
	}
	all, err := st.QueryRecentAuditEvents(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	var stored []types.AuditEvent // the two rows as stored (the forwarded one re-keyed)
	for _, e := range all {
		if e.Action == action {
			stored = append(stored, e)
		}
	}
	if len(stored) != 2 {
		t.Fatalf("seeded %d rows, want 2", len(stored))
	}

	for name, tc := range map[string]struct {
		f    store.AuditFilter
		want []uuid.UUID
	}{
		// defect: the device row is not listed under the person
		"person filter (name + subject)": {store.AuditFilter{Target: name, TargetAlt: subject}, []uuid.UUID{org.ID}},
		// characterization: the raw claimed string still finds the device row (and the org row, same string)
		"raw subject string":           {store.AuditFilter{Target: subject}, []uuid.UUID{org.ID, fed.ID}},
		"raw subject, origin device":   {store.AuditFilter{Target: subject, Origin: store.AuditOriginDevice}, []uuid.UUID{fed.ID}},
		"person filter, origin device": {store.AuditFilter{Target: name, TargetAlt: subject, Origin: store.AuditOriginDevice}, nil},
	} {
		got := paged(tc.f)
		var goGot []uuid.UUID
		for _, e := range tc.f.Keep(stored) {
			goGot = append(goGot, e.ID)
		}
		wantSorted := slices.Clone(tc.want)
		for _, ids := range [][]uuid.UUID{got, goGot, wantSorted} { // order is not under test
			slices.SortFunc(ids, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
		}
		if !slices.Equal(got, wantSorted) {
			t.Errorf("%s: SQL (paged) = %v, want %v", name, got, wantSorted)
		}
		if !slices.Equal(goGot, wantSorted) {
			t.Errorf("%s: Go filter = %v, want %v", name, goGot, wantSorted)
		}
	}
}
