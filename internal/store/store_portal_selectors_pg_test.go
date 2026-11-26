// Copyright 2026 The Wardyn Authors
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

func sortedIDs(ids ...uuid.UUID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	slices.Sort(out)
	return out
}

func sameIDs(t *testing.T, what string, got []uuid.UUID, want ...uuid.UUID) {
	t.Helper()
	if g, w := sortedIDs(got...), sortedIDs(want...); !slices.Equal(g, w) {
		t.Errorf("%s = %v, want %v", what, g, w)
	}
}

// The five portal selectors of the lifecycle and the two issuer-scoped listings return exactly the rows each
// names, with a workforce person and another portal's person present in the same states: portal people of any
// issuer are in the portal selectors, a workforce person is in none of them, and a scoped listing holds one
// issuer's people only, the scope applied before the limit.
func TestPortalLifecycle_SelectorsReturnExactRows_PG(t *testing.T) {
	st, pool := isolatedStore(t)
	ctx := context.Background()
	p1 := newIssuerPortal(t, st, "")
	p2 := newIssuerPortal(t, st, "")
	issuer1, issuer2 := types.PortalPersonPrefix+p1.Key, types.PortalPersonPrefix+p2.Key
	const workforceIssuer = "https://workforce.example.com"

	job := func(id uuid.UUID, step, state string, attempts int, lastError string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO deprovision_jobs (identity_id, kind, step, target, state, attempts, last_error)
			VALUES ($1, 'suspend', $2, '', $3, $4, $5)`, id, step, state, attempts, lastError); err != nil {
			t.Fatal(err)
		}
	}
	// state sets when the person was deactivated and, if given, their purge deadline, both as intervals ago.
	state := func(id uuid.UUID, deactivatedAgo, purgeAgo string) {
		t.Helper()
		if err := markedExec(t, pool, `UPDATE principal_identities SET deactivated_at = now() - $2::interval,
			purge_after = CASE WHEN $3::text = '' THEN NULL ELSE now() - $3::interval END WHERE id = $1`, id, deactivatedAgo, purgeAgo); err != nil {
			t.Fatal(err)
		}
	}

	// The first portal's people.
	issueFor(t, st, p1, "active") // an active person is in no selector
	dormant, _ := issueFor(t, st, p1, "dormant")
	state(dormant, "40 minutes", "")
	dueOwn, _ := issueFor(t, st, p1, "dueown")
	state(dueOwn, "2 hours", "1 hour")
	if _, err := pool.Exec(ctx, `INSERT INTO deprovision_jobs (identity_id, kind, step, target, state, detail)
		SELECT id, 'suspend', 'cutoff', '', 'done', jsonb_build_object('deactivated_at_us', (extract(epoch FROM deactivated_at) * 1000000)::bigint)
		  FROM principal_identities WHERE id = $1`, dueOwn); err != nil {
		t.Fatal(err)
	}
	duePlain, _ := issueFor(t, st, p1, "dueplain")
	state(duePlain, "3 hours", "30 minutes")
	failed, _ := issueFor(t, st, p1, "failed")
	state(failed, "20 minutes", "")
	job(failed, "sweep", "pending", 2, "boom")
	refused, _ := issueFor(t, st, p1, "refused")
	job(refused, store.JobStepRefusalAudit, "pending", 0, "owed")

	// Another portal's person, deactivated most recently, with a failing job.
	other, _ := issueFor(t, st, p2, "other")
	state(other, "1 minute", "")
	job(other, "sweep", "pending", 1, "other boom")

	// A workforce person in every state the portal selectors read.
	var workforce uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO principal_identities (principal, issuer, email_lower) VALUES ('wf-sub', $1, 'wf@example.com') RETURNING id`, workforceIssuer).Scan(&workforce); err != nil {
		t.Fatal(err)
	}
	state(workforce, "5 hours", "2 hours")
	job(workforce, "sweep", "pending", 0, "wf boom")
	job(workforce, store.JobStepRefusalAudit, "pending", 0, "wf owed")

	due, err := st.PurgeDuePortalIdentities(ctx, time.Now(), 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 2 || due[0] != (store.DueIdentity{ID: dueOwn, Suspended: true}) || due[1] != (store.DueIdentity{ID: duePlain, Suspended: false}) {
		t.Errorf("PurgeDuePortalIdentities = %+v, want the own-suspension row first, then the plain one, and no workforce row", due)
	}

	leavers, err := st.PendingPortalLeavers(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	var leaverIDs []uuid.UUID
	for _, l := range leavers {
		leaverIDs = append(leaverIDs, l.ID)
	}
	sameIDs(t, "PendingPortalLeavers", leaverIDs, failed, other)

	audits, err := st.PendingPortalRefusalAudits(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(audits) != 1 || audits[0].IdentityID != refused || audits[0].Kind != store.JobKindSuspend {
		t.Errorf("PendingPortalRefusalAudits = %+v, want the one portal person owed a refusal", audits)
	}

	deactivated, err := st.ListDeactivatedPortalIdentities(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	var deactivatedIDs []uuid.UUID
	for _, i := range deactivated {
		deactivatedIDs = append(deactivatedIDs, i.ID)
	}
	sameIDs(t, "ListDeactivatedPortalIdentities", deactivatedIDs, dormant, dueOwn, duePlain, failed, other)
	if len(deactivated) > 0 && deactivated[0].ID != other {
		t.Errorf("the most recently deactivated is %v first, want %v", deactivated[0].ID, other)
	}

	failures, err := st.ListPortalDeprovisionFailures(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	want := map[uuid.UUID]store.DeprovisionFailure{
		failed:  {IdentityID: failed, Kind: store.JobKindSuspend, Step: "sweep", LastError: "boom"},
		refused: {IdentityID: refused, Kind: store.JobKindSuspend, Step: store.JobStepRefusalAudit, LastError: "owed"},
		other:   {IdentityID: other, Kind: store.JobKindSuspend, Step: "sweep", LastError: "other boom"},
	}
	if len(failures) != len(want) {
		t.Errorf("ListPortalDeprovisionFailures = %+v, want %d rows", failures, len(want))
	}
	for _, f := range failures {
		if w, ok := want[f.IdentityID]; !ok || f != w {
			t.Errorf("ListPortalDeprovisionFailures row %+v, want %+v", f, want[f.IdentityID])
		}
	}

	// The issuer-scoped listings: the first portal's people only, the other portal's person, the most
	// recently deactivated, never filling a page of one.
	scoped, err := st.ListDeactivatedIdentities(ctx, issuer1, "", 50)
	if err != nil {
		t.Fatal(err)
	}
	var scopedIDs []uuid.UUID
	for _, i := range scoped {
		scopedIDs = append(scopedIDs, i.ID)
	}
	sameIDs(t, "ListDeactivatedIdentities(first portal)", scopedIDs, dormant, dueOwn, duePlain, failed)
	if one, err := st.ListDeactivatedIdentities(ctx, issuer1, "", 1); err != nil || len(one) != 1 || one[0].ID != failed {
		t.Errorf("ListDeactivatedIdentities(first portal, limit 1) = %+v, %v; want the first portal's most recent (%v), not the other portal's", one, err, failed)
	}
	if got, err := st.ListDeactivatedIdentities(ctx, issuer2, "", 50); err != nil || len(got) != 1 || got[0].ID != other {
		t.Errorf("ListDeactivatedIdentities(second portal) = %+v, %v; want only its person", got, err)
	}
	if got, err := st.ListDeactivatedIdentities(ctx, workforceIssuer, "", 50); err != nil || len(got) != 1 || got[0].ID != workforce {
		t.Errorf("ListDeactivatedIdentities(workforce) = %+v, %v; want only the workforce person", got, err)
	}
	if got, err := st.ListDeprovisionFailures(ctx, issuer1, "", 50); err != nil || len(got) != 2 {
		t.Errorf("ListDeprovisionFailures(first portal) = %+v, %v; want its two rows", got, err)
	} else {
		for _, f := range got {
			if f.IdentityID != failed && f.IdentityID != refused {
				t.Errorf("ListDeprovisionFailures(first portal) lists %+v", f)
			}
		}
	}
	if got, err := st.ListDeprovisionFailures(ctx, workforceIssuer, "", 50); err != nil || len(got) != 2 {
		t.Errorf("ListDeprovisionFailures(workforce) = %+v, %v; want its two rows", got, err)
	} else {
		for _, f := range got {
			if f.IdentityID != workforce {
				t.Errorf("ListDeprovisionFailures(workforce) lists %+v", f)
			}
		}
	}
}
