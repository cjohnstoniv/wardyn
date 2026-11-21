// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
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
	// Every ledger row is idle long enough for the sweeper to resume.
	if _, err := e.pool.Exec(ctx, `UPDATE deprovision_jobs SET updated_at = now() - interval '1 hour'`); err != nil {
		t.Fatal(err)
	}
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
	var purgedFillers int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM principal_identities WHERE principal LIKE 'filler-%' AND purged_at IS NOT NULL`).Scan(&purgedFillers); err != nil || purgedFillers != 0 {
		t.Errorf("purged foreign due people = %d (%v), want none", purgedFillers, err)
	}
}
