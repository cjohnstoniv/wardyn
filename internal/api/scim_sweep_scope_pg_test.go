// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	sweepForeignIssuer = "https://other-issuer.example/v2.0"
	sweepOtherTenant   = "99999999-2222-3333-4444-555555555555"
)

// foreignLeaver is a person the sweeper must leave alone: its identity row, and what a suspension of it would
// have cut or killed.
type foreignLeaver struct {
	row       store.PrincipalIdentity
	principal string
	email     string
	token     uuid.UUID
	key       string
	run       uuid.UUID
}

// seedSuspendedAt writes a person of issuer and tenant with a token, an SSH key and a running run, suspended
// by the store (the cutoff step done and stamped, the purge scheduled, no session cut written) with pending
// ledger rows for jobs.
func (e *scimEnv) seedSuspendedAt(issuer, tenant, principal string, jobs ...store.JobKey) foreignLeaver {
	e.t.Helper()
	ctx := context.Background()
	f := foreignLeaver{principal: principal, email: principal + "@corp.example"}
	row, err := e.st.UpsertLoginIdentity(ctx, store.LoginIdentity{
		Principal: principal, Issuer: issuer, TenantID: tenant, ObjectID: "oid-" + principal, Email: f.email,
	}, time.Now().UTC())
	if err != nil {
		e.t.Fatal(err)
	}
	f.row = row
	f.token, _ = e.seedToken(principal, f.email)
	f.key = e.seedKey(principal)
	f.run = e.seedRun(principal, types.RunRunning)
	if _, err := e.st.SuspendIdentity(ctx, store.SuspendPlan{
		IdentityID: row.ID, Principal: principal, Principals: []string{principal}, PurgePrincipals: []string{principal},
		PurgeAfter: time.Hour, PendingJobs: jobs,
	}); err != nil {
		e.t.Fatal(err)
	}
	return f
}

// jobSnapshot is every ledger row of identity id, in a stable order, as text: a row the sweeper touched differs.
func (e *scimEnv) jobSnapshot(id uuid.UUID) string {
	e.t.Helper()
	rows, err := e.pool.Query(context.Background(), `SELECT kind, step, target, state, attempts, last_error, detail::text, updated_at
		FROM deprovision_jobs WHERE identity_id = $1 ORDER BY kind, step, target`, id)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out string
	for rows.Next() {
		var kind, step, target, state, lastErr, detail string
		var attempts int
		var at time.Time
		if err := rows.Scan(&kind, &step, &target, &state, &attempts, &lastErr, &detail, &at); err != nil {
			e.t.Fatal(err)
		}
		out += fmt.Sprintf("%s/%s/%s %s %d %q %s %s\n", kind, step, target, state, attempts, lastErr, detail, at.Format(time.RFC3339Nano))
	}
	if err := rows.Err(); err != nil {
		e.t.Fatal(err)
	}
	return out
}

// The sweeper works only for the issuer and tenant it serves. Another issuer's due and suspended person, one
// with a pending suspension, and a same-issuer person of another tenant are left exactly as found: not
// purged, their ledger unchanged, no session cut, no token revoked, no key deleted, no run killed. They also
// do not take the batch: more foreign due people than one batch hold the first places, and the own person
// is still purged.
func TestSCIMSweep_LeavesForeignWork(t *testing.T) {
	e := newSCIMEnv(t, func(c *Config) { c.SCIM.PurgeAfter = time.Hour })
	ctx := context.Background()

	own := e.seedPurgeSubject(purgeSub, purgeEmail, purgeOID)
	e.suspendOn(e.a, purgeEmail)
	e.ageSuspension(own.id)

	foreignDue := e.seedSuspendedAt(sweepForeignIssuer, e.tenant, "foreign-due")
	foreignPending := e.seedSuspendedAt(sweepForeignIssuer, e.tenant, "foreign-pending", store.JobKey{Step: "audit_deactivate"})
	otherTenant := e.seedSuspendedAt(e.issuer, sweepOtherTenant, "other-tenant")
	leaves := []foreignLeaver{foreignDue, foreignPending, otherTenant}

	// Foreign due people that sort ahead of the own one (proven suspensions, due a day ago): without the
	// filter before the limit they fill the whole batch.
	if _, err := e.pool.Exec(ctx, `
		INSERT INTO principal_identities (principal, issuer, tenant_id, object_id, deactivated_at, purge_after)
		SELECT 'filler-' || g, $1, $2, 'filler-oid-' || g, now() - interval '2 days', now() - interval '1 day' - g * interval '1 second'
		  FROM generate_series(1, $3::int) g`, sweepForeignIssuer, e.tenant, scimSweepBatch+10); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `
		INSERT INTO deprovision_jobs (identity_id, kind, step, target, state, attempts, detail)
		SELECT id, 'suspend', 'cutoff', '', 'done', 1,
		       jsonb_build_object('deactivated_at_us', (extract(epoch FROM deactivated_at) * 1000000)::bigint)
		  FROM principal_identities WHERE principal LIKE 'filler-%'`); err != nil {
		t.Fatal(err)
	}
	for _, l := range leaves {
		if _, err := e.pool.Exec(ctx, `UPDATE principal_identities SET purge_after = now() - interval '1 day' WHERE id = $1 AND purge_after IS NOT NULL`, l.row.ID); err != nil {
			t.Fatal(err)
		}
	}
	// Foreign suspensions the identity provider stopped retrying, more than one batch of them and older than the
	// own one, so they come first in the pending list: without the filter before the limit the own person's
	// resume is left out of the batch.
	own2 := e.seedSuspendedAt(e.issuer, e.tenant, "own-pending", store.JobKey{Step: jobStepAuditDeact})
	if _, err := e.pool.Exec(ctx, `
		INSERT INTO principal_identities (principal, issuer, tenant_id, object_id, created_at, deactivated_at)
		SELECT 'pfill-' || g, $1, $2, 'pfill-oid-' || g, now() - interval '30 days', now() - interval '2 days'
		  FROM generate_series(1, $3::int) g`, sweepForeignIssuer, e.tenant, scimSweepBatch+10); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `
		INSERT INTO deprovision_jobs (identity_id, kind, step, target)
		SELECT id, 'suspend', $1, '' FROM principal_identities WHERE principal LIKE 'pfill-%'`, jobStepAuditDeact); err != nil {
		t.Fatal(err)
	}
	// Every ledger row is idle long enough for the sweeper to resume.
	if _, err := e.pool.Exec(ctx, `UPDATE deprovision_jobs SET updated_at = now() - interval '1 hour'`); err != nil {
		t.Fatal(err)
	}
	for _, l := range leaves {
		var due bool
		if err := e.pool.QueryRow(ctx, `SELECT purge_after IS NOT NULL AND purge_after < now() FROM principal_identities WHERE id = $1`, l.row.ID).Scan(&due); err != nil || !due {
			t.Fatalf("control: %s is not due before the sweep (%v)", l.principal, err)
		}
	}
	own2Before := e.jobSnapshot(own2.row.ID)
	before := map[uuid.UUID]string{}
	for _, l := range leaves {
		before[l.row.ID] = e.jobSnapshot(l.row.ID)
	}
	if before[foreignPending.row.ID] == "" {
		t.Fatal("control: the foreign person with a pending suspension has no ledger rows")
	}
	cuts := e.cutMark(foreignDue.principal, foreignDue.email)

	if err := e.b.srv.SweepSCIMPurge(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if got := e.identity(own.id); got.PurgedAt == nil {
		t.Error("the own person past the purge delay was not purged: foreign due people took the batch")
	}
	for _, l := range leaves {
		got := e.identity(l.row.ID.String())
		if got.PurgedAt != nil {
			t.Errorf("%s: purged by a sweeper of another issuer or tenant", l.principal)
		}
		if got.DeactivatedAt == nil {
			t.Errorf("%s: reactivated or changed by the sweeper", l.principal)
		}
		if after := e.jobSnapshot(l.row.ID); after != before[l.row.ID] {
			t.Errorf("%s: ledger changed by the sweeper:\nbefore:\n%safter:\n%s", l.principal, before[l.row.ID], after)
		}
		if e.tokenRevoked(l.token) {
			t.Errorf("%s: token revoked by the sweeper", l.principal)
		}
		if e.keyCount(l.principal) != 1 {
			t.Errorf("%s: SSH key deleted by the sweeper", l.principal)
		}
		if st := e.runState(l.run); st != types.RunRunning {
			t.Errorf("%s: run is %s, want it left running", l.principal, st)
		}
		if e.cutMark(l.principal, l.email) != 0 {
			t.Errorf("%s: session cut written by the sweeper", l.principal)
		}
	}
	if got := e.cutMark(foreignDue.principal, foreignDue.email); got != cuts {
		t.Errorf("session cut mark moved from %d to %d", cuts, got)
	}
	if after := e.jobSnapshot(own2.row.ID); after == own2Before {
		t.Errorf("the own pending suspension was not resumed: foreign pending suspensions took the batch:\n%s", after)
	}
	var touched int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM deprovision_jobs j JOIN principal_identities i ON i.id = j.identity_id
		WHERE i.principal LIKE 'pfill-%' AND (j.state <> 'pending' OR j.attempts <> 0 OR j.updated_at > now() - interval '30 minutes')`).Scan(&touched); err != nil || touched != 0 {
		t.Errorf("foreign pending ledger rows touched by the sweeper = %d (%v), want none", touched, err)
	}
	var purgedFillers int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM principal_identities WHERE principal LIKE 'filler-%' AND purged_at IS NOT NULL`).Scan(&purgedFillers); err != nil || purgedFillers != 0 {
		t.Errorf("purged foreign due people = %d (%v), want none", purgedFillers, err)
	}
}

// An owed refusal row of another issuer's person is not the sweeper's to pin, write or finish: a sweep leaves
// it pending and unpinned and writes no row for it, and does not let more foreign rows than one batch keep
// the own person's owed row out of the pass. The own person's row is written and finished.
func TestSCIMSweep_LeavesForeignOwedRefusalRows(t *testing.T) {
	e := newSCIMEnv(t)
	ctx := context.Background()
	const step = store.JobStepRefusalAudit + "denied/test_reason"

	own, err := e.st.UpsertLoginIdentity(ctx, store.LoginIdentity{Principal: "own-owed", Issuer: e.issuer, TenantID: e.tenant, ObjectID: "oid-own-owed", Email: "own-owed@corp.example"}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := e.st.UpsertLoginIdentity(ctx, store.LoginIdentity{Principal: "foreign-owed", Issuer: sweepForeignIssuer, TenantID: e.tenant, ObjectID: "oid-foreign-owed", Email: "foreign-owed@corp.example"}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	otherTenant, err := e.st.UpsertLoginIdentity(ctx, store.LoginIdentity{Principal: "tenant-owed", Issuer: e.issuer, TenantID: sweepOtherTenant, ObjectID: "oid-tenant-owed", Email: "tenant-owed@corp.example"}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	// Foreign fillers whose ids sort before any random one: the owed list is ordered by identity id, so without
	// the filter before the limit they fill the batch ahead of the own person.
	if _, err := e.pool.Exec(ctx, `
		INSERT INTO principal_identities (id, principal, issuer, tenant_id, object_id)
		SELECT ('00000000-0000-4000-8000-' || lpad(g::text, 12, '0'))::uuid, 'ofill-' || g, $1, $2, 'ofill-oid-' || g
		  FROM generate_series(1, $3::int) g`, sweepForeignIssuer, e.tenant, scimSweepBatch+10); err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx, `
		INSERT INTO deprovision_jobs (identity_id, kind, step, target, state, attempts, last_error, updated_at)
		SELECT id, 'suspend', $1, '', 'pending', 1, $2, now() - interval '1 hour'
		  FROM principal_identities WHERE principal LIKE 'ofill-%' OR id = ANY($3::uuid[])`,
		step, store.RefusalAuditOwedMsg, []uuid.UUID{own.ID, foreign.ID, otherTenant.ID}); err != nil {
		t.Fatal(err)
	}
	pinned := func(id uuid.UUID) bool {
		var n int
		if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM deprovision_jobs WHERE identity_id = $1 AND audit_event_id IS NOT NULL`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n > 0
	}
	before := map[uuid.UUID]string{foreign.ID: e.jobSnapshot(foreign.ID), otherTenant.ID: e.jobSnapshot(otherTenant.ID)}
	if before[foreign.ID] == "" || before[otherTenant.ID] == "" {
		t.Fatal("control: a foreign person has no owed row")
	}

	if err := e.b.srv.SweepSCIMPurge(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	if jobs, err := e.st.ListDeprovisionJobs(ctx, own.ID, store.JobKindSuspend); err != nil || len(jobs) != 1 || !jobs[0].Done || !pinned(own.ID) {
		t.Errorf("the own person's owed row = %+v (%v), want it written and finished", jobs, err)
	}
	for _, f := range []store.PrincipalIdentity{foreign, otherTenant} {
		if after := e.jobSnapshot(f.ID); after != before[f.ID] {
			t.Errorf("%s: owed row changed by the sweeper:\nbefore:\n%safter:\n%s", f.Principal, before[f.ID], after)
		}
		if pinned(f.ID) {
			t.Errorf("%s: owed row pinned to an audit event by the sweeper", f.Principal)
		}
	}
	var touched int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM deprovision_jobs j JOIN principal_identities i ON i.id = j.identity_id
		WHERE i.principal LIKE 'ofill-%' AND (j.state <> 'pending' OR j.audit_event_id IS NOT NULL OR j.attempts <> 1)`).Scan(&touched); err != nil || touched != 0 {
		t.Errorf("foreign owed rows touched by the sweeper = %d (%v), want none", touched, err)
	}
	if rows := len(e.rows(e.a, "person.deprovision")) + len(e.rows(e.b, "person.deprovision")); rows != 1 {
		t.Errorf("person.deprovision rows = %d, want the one written for the own person", rows)
	}
}

// A person of the SCIM issuer and tenant whose deactivation is no suspension of their own (alongside another
// leaver's, with no suspension on the ledger, or after an old suspension of their own, recorded or not) is
// listed by the sweeper's read but not as proved, the mark under the row lock refuses it, and the sweeper
// leaves it deactivated and unpurged: the !Suspended guard, with an own-issuer row.
func TestSCIMSweep_LeavesAnOwnScheduleWithNoSuspensionOfItsOwn(t *testing.T) {
	for _, tc := range []struct {
		name           string
		old, unstamped bool
	}{{"no suspension of its own", false, false}, {"an old suspension recorded by this release", true, false}, {"an old suspension recorded by an earlier release", true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			e := newSCIMEnv(t, func(c *Config) { c.SCIM.PurgeAfter = time.Hour })
			ctx := t.Context()
			p := e.seedEntra("sub-alongside", "alongside@corp.example", "oid-alongside")
			if tc.old {
				if _, err := e.st.SuspendIdentity(ctx, store.SuspendPlan{IdentityID: p.ID, Principal: p.Principal, Principals: []string{"sub-alongside"}}); err != nil {
					t.Fatal(err)
				}
				if tc.unstamped {
					if _, err := e.pool.Exec(ctx, `UPDATE deprovision_jobs SET detail = detail - 'deactivated_at_us' WHERE identity_id = $1`, p.ID); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := e.st.ApplyIdentityUpdate(ctx, p.ID, store.IdentityUpdate{Reactivate: true}, time.Now().UTC()); err != nil {
					t.Fatal(err)
				}
			}
			// Deactivated and scheduled by another identity's suspension, as an earlier release did.
			if _, err := e.pool.Exec(ctx, `UPDATE principal_identities SET deactivated_at = clock_timestamp(), purge_after = now() - interval '1 minute' WHERE id = $1`, p.ID); err != nil {
				t.Fatal(err)
			}

			due, err := e.st.PurgeDueIdentities(ctx, e.issuer, e.tenant, 50)
			if err != nil || !slices.ContainsFunc(due, func(d store.DueIdentity) bool { return d.ID == p.ID && !d.Suspended }) {
				t.Fatalf("control: the candidate read = %+v (%v), want the person listed as not suspended", due, err)
			}
			if marked, err := e.st.MarkIdentityPurged(ctx, p.ID, true, e.issuer, e.tenant); err != nil || marked {
				t.Fatalf("the sweeper's mark = %v (%v), want it refused under the row lock", marked, err)
			}
			if err := e.sweepPastThePurgeDelay(); err != nil {
				t.Errorf("sweep: %v", err)
			}
			if got := e.identity(p.ID.String()); got.PurgedAt != nil || got.DeactivatedAt == nil {
				t.Errorf("identity after the sweep = %+v, want it deactivated and not purged", got)
			}
		})
	}
}
