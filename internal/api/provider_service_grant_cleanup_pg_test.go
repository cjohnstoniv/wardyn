// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The clean-up of a removed grant's secret across two Wardyn processes on one database: what a held or
// interrupted external delete leaves to the other process, and what is persisted.

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// removedWithClaim has two rows with secrets, then removes corp-b while its external delete fails, so the
// claim on corp-b's uid is stored. It returns that uid.
func (f *svcPG) removedWithClaim() string {
	f.t.Helper()
	f.put(svcPGRow("corp-a", true), svcPGRow("corp-b", true))
	for _, id := range []string{"corp-a", "corp-b"} {
		if w := f.writeSecret(id, svcClientSecret); w.Code != http.StatusNoContent {
			f.t.Fatalf("secret of %s: %d %s", id, w.Code, w.Body.String())
		}
	}
	uid := f.uid("corp-b")
	f.sec.mu.Lock()
	f.sec.delErr = errSvcDown
	f.sec.mu.Unlock()
	if w := f.put(svcPGRow("corp-a", true)); w.Code/100 != 2 {
		f.t.Fatalf("removing corp-b: %d %s", w.Code, w.Body.String())
	}
	f.sec.mu.Lock()
	f.sec.delErr = nil
	f.sec.mu.Unlock()
	if got := f.claimUIDs(); len(got) != 1 || got[0] != uid {
		f.t.Fatalf("stored claims = %v, want one on %s", got, uid)
	}
	return uid
}

// seenAt is the heartbeat time stored for one replica, the zero time when it has no status row.
func (f *svcPG) seenAt(replica string) time.Time {
	f.t.Helper()
	var at time.Time
	if err := f.pool.QueryRow(context.Background(),
		`SELECT max(seen_at) FROM provider_service_grant_status WHERE replica_id = $1`, replica).Scan(&at); err != nil {
		return time.Time{}
	}
	return at
}

func (f *svcPG) secretDeleteRows() int {
	return len(f.audit.find("model_provider.service_grant.secret_delete"))
}

// A held external delete on one process leaves the other's refresh, heartbeat and provider save alone.
func TestServiceGrantCleanup_HeldDeleteLeavesRefreshAndHeartbeat_PG(t *testing.T) {
	f := newSvcPG(t, true)
	r := f.newReplica()
	uid := f.removedWithClaim()
	if _, err := r.mint("corp-a"); err != nil {
		t.Fatalf("the second process's own mint: %v", err)
	}
	replica := r.srv.svcGrant.replica()
	baseline := f.seenAt(replica)
	if baseline.IsZero() {
		t.Fatal("setup: the second process has no status row")
	}
	name := serviceGrantSecretName(uid)
	held, release := make(chan struct{}), make(chan struct{})
	f.sec.mu.Lock()
	f.sec.hold = func(op, n string) {
		if op == "delete" && n == name {
			f.sec.hold = nil
			close(held)
			<-release
		}
	}
	f.sec.mu.Unlock()
	done := make(chan cleanupOutcome, 1)
	go func() { done <- f.srv.serviceGrantCleanupAttempt(context.Background(), uid) }()
	<-held

	finished := make(chan *int, 1)
	go func() {
		ctx := context.Background()
		r.srv.serviceGrantHeartbeat(ctx)
		r.srv.serviceGrantPass(ctx)
		code := r.put(svcPGRow("corp-a", true)).Code
		finished <- &code
	}()
	select {
	case code := <-finished:
		if *code/100 != 2 {
			t.Fatalf("the second process's provider save = %d while the delete is held, want it committed", *code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the second process's heartbeat, refresh or save waited for a held external delete")
	}
	if after := f.seenAt(replica); !after.After(baseline) {
		t.Errorf("the second process's heartbeat did not advance past its own baseline (%v then %v)", baseline, after)
	}
	if row, ok := f.pgStatus("corp-a", replica); !ok || row.Outcome != "ok" {
		t.Errorf("the second process's status for corp-a = %+v, %v, want ok", row, ok)
	}
	close(release)
	if out := <-done; out != cleanupSettled {
		t.Fatalf("the held attempt ended %d, want settled", out)
	}
	if got := f.claimUIDs(); len(got) != 0 || f.secretDeleteRows() != 1 {
		t.Fatalf("after the release: claims %v, settle rows %d, want none and one", got, f.secretDeleteRows())
	}
}

// A process that stops after the external delete and before the settlement leaves a stored claim that the
// other process settles, once, with one audit row.
func TestServiceGrantCleanup_ResumesAfterDeleteBeforeSettle_PG(t *testing.T) {
	f := newSvcPG(t, true)
	r := f.newReplica()
	uid := f.removedWithClaim()
	f.srv.svcGrant.hook = func(stage string) bool { return stage == "after_delete" }
	if out := f.srv.serviceGrantCleanupAttempt(context.Background(), uid); out != cleanupKept {
		t.Fatalf("the stopped attempt ended %d, want kept", out)
	}
	f.srv.svcGrant.hook = nil
	if f.sec.has(serviceGrantSecretName(uid)) || len(f.claimUIDs()) != 1 || f.secretDeleteRows() != 0 {
		t.Fatalf("after the stop: record %v, stored claims %v, settle rows %d; want the record deleted, the claim stored, no row",
			f.sec.has(serviceGrantSecretName(uid)), f.claimUIDs(), f.secretDeleteRows())
	}
	deletes := f.sec.callsOf("delete")
	if out := r.srv.serviceGrantCleanupAttempt(context.Background(), uid); out != cleanupSettled {
		t.Fatalf("the second process ended %d, want settled", out)
	}
	if got := f.claimUIDs(); len(got) != 0 {
		t.Fatalf("stored claims after the second process settled = %v, want none (persisted in the database)", got)
	}
	if f.sec.callsOf("delete") != deletes {
		t.Errorf("the second process deleted again (%d new deletes)", f.sec.callsOf("delete")-deletes)
	}
	if n := f.secretDeleteRows() + len(r.audit.find("model_provider.service_grant.secret_delete")); n != 1 {
		t.Fatalf("settle audit rows across both processes = %d, want exactly one", n)
	}
}

// pgStatus is one replica's stored status row for a provider.
func (f *svcPG) pgStatus(id, replica string) (store.ServiceGrantStatusRow, bool) {
	f.t.Helper()
	rows, err := f.pg.ServiceGrantStatuses(context.Background(), []string{f.uid(id)})
	if err != nil {
		f.t.Fatal(err)
	}
	for _, r := range rows {
		if r.ReplicaID == replica {
			return r, true
		}
	}
	return store.ServiceGrantStatusRow{}, false
}
