// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The clean-up of a removed grant's secret across two Wardyn processes on one database: what a held or
// interrupted external delete leaves to the other process, and what is persisted.

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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

// A held external delete on one process leaves the other's refresh, heartbeat and provider save alone. The other
// process's healthy row is brought to its renewal point while the delete is held, so its pass must mint anew and
// persist a fresh attempt; an earlier OK row cannot satisfy the check.
func TestServiceGrantCleanup_HeldDeleteLeavesRefreshAndHeartbeat_PG(t *testing.T) {
	f := newSvcPG(t, true)
	clock := &svcClock{t: time.Now().UTC()}
	r := f.newReplicaClocked(clock)
	r.idp.respond = func(n int, _ svcReq) svcAnswer { return svcOK(n, serviceGrantMinLifetime) }
	uid := f.removedWithClaim()
	if _, err := r.mint("corp-a"); err != nil {
		t.Fatalf("the second process's own mint: %v", err)
	}
	replica := r.srv.svcGrant.replica()
	baseline := f.seenAt(replica)
	first, ok := f.pgStatus("corp-a", replica)
	if baseline.IsZero() || !ok || first.Outcome != "ok" {
		t.Fatalf("setup: the second process's status row = %+v, %v, seen %v; want ok", first, ok, baseline)
	}
	name := serviceGrantSecretName(uid)
	held, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	var armed atomic.Bool
	armed.Store(true)
	f.sec.mu.Lock()
	f.sec.hold = func(op, n string) {
		if op == "delete" && n == name && armed.CompareAndSwap(true, false) {
			close(held)
			if !svcWaited(release) {
				t.Error("the held delete was never released")
			}
		}
	}
	f.sec.mu.Unlock()
	done := make(chan cleanupOutcome, 1)
	go func() { done <- f.srv.serviceGrantCleanupAttempt(context.Background(), uid) }()
	svcAwait(t, held, "the external delete to be held")

	// Six minutes on, the 15-minute token is inside the reuse margin: corp-a is due on the second process.
	clock.Advance(6 * time.Minute)
	if !r.srv.serviceGrantDue(context.Background(), r.row("corp-a")) {
		t.Fatal("setup: corp-a is not due on the second process")
	}
	sent := r.idp.count()
	// The heartbeat's stamp is read after the heartbeat and before the pass may run: the pass's own status
	// write also moves seen_at, so a reading after it would not show the heartbeat did anything.
	beat, pass := make(chan struct{}), make(chan struct{})
	var passOnce sync.Once
	t.Cleanup(func() { passOnce.Do(func() { close(pass) }) })
	finished := make(chan int, 1)
	go func() {
		ctx := context.Background()
		r.srv.serviceGrantHeartbeat(ctx)
		close(beat)
		if !svcWaited(pass) {
			t.Error("the pass was never let run")
		}
		r.srv.serviceGrantPass(ctx)
		finished <- r.put(svcPGRow("corp-a", true)).Code
	}()
	select {
	case <-beat:
	case <-time.After(10 * time.Second):
		t.Fatal("the second process's heartbeat waited for a held external delete")
	}
	beatAt := f.seenAt(replica)
	if n := r.idp.count() - sent; n != 0 {
		t.Fatalf("%d token requests before the pass ran; the heartbeat must send none", n)
	}
	passOnce.Do(func() { close(pass) })
	select {
	case code := <-finished:
		if code/100 != 2 {
			t.Fatalf("the second process's provider save = %d while the delete is held, want it committed", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the second process's refresh or save waited for a held external delete")
	}
	if n := r.idp.count() - sent; n != 1 {
		t.Errorf("the second process sent %d token requests for the due row while the delete was held, want 1", n)
	}
	if !beatAt.After(baseline) {
		t.Errorf("the second process's heartbeat did not advance seen_at past its own baseline (%v then %v)", baseline, beatAt)
	}
	row, ok := f.pgStatus("corp-a", replica)
	if !ok || row.Outcome != "ok" || !row.AttemptedAt.After(first.AttemptedAt) || row.LastOKAt == nil ||
		first.LastOKAt == nil || !row.LastOKAt.After(*first.LastOKAt) {
		t.Errorf("the second process's status for corp-a = %+v, %v; want a fresh ok attempt after %v", row, ok, first.AttemptedAt)
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case out := <-done:
		if out != cleanupSettled {
			t.Fatalf("the held attempt ended %d, want settled", out)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for the released delete to settle")
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

// The earlier removal's delete runs with the revision it read; a record written since has another one, so the
// retry keeps the newer record and the claim.
func TestServiceGrant_DelayedDeleteKeepsNewerRecord_PG(t *testing.T) {
	f := newSvcPG(t, true)
	uid := f.claimFor("corp-b")
	name := serviceGrantSecretName(uid)
	f.sec.set(name, mustJSON(serviceGrantRecord{V: 1, TokenURL: svcTokenURL, ClientID: svcClientID, BaseURL: "https://new.corp.example", ClientSecret: svcSecretTwo}))
	if w := f.retry(uid); w.Code != http.StatusConflict || !f.sec.has(name) || !slices.Contains(f.claimUIDs(), uid) {
		t.Fatalf("a delayed delete with the earlier revision: %d, record kept %v, claims %v; want the newer record and the claim kept",
			w.Code, f.sec.has(name), f.claimUIDs())
	}
}

// Each answer the store's revision read can give, at the retry route and in the worker, with the claims in the
// database.
func TestServiceGrantCleanup_EachRevisionAnswer_PG(t *testing.T) {
	f := newSvcPG(t, true)
	claimed := func(uid string) bool { return slices.Contains(f.claimUIDs(), uid) }
	// Absence settles with no delete.
	uid := f.claimFor("corp-b")
	name := serviceGrantSecretName(uid)
	f.sec.mu.Lock()
	delete(f.sec.m, name)
	f.sec.rev[name]++
	f.sec.mu.Unlock()
	before := f.sec.callsOf("delete")
	if w := f.retry(uid); w.Code != http.StatusNoContent || f.sec.callsOf("delete") != before || claimed(uid) {
		t.Fatalf("absence: %d, %d deletes, claimed %v; want 204, none, settled", w.Code, f.sec.callsOf("delete")-before, claimed(uid))
	}
	// The claim's own revision deletes, guarded, and settles.
	uid = f.claimFor("corp-c")
	if w := f.retry(uid); w.Code != http.StatusNoContent || f.sec.has(serviceGrantSecretName(uid)) || claimed(uid) {
		t.Fatalf("own revision: %d, record kept %v, claimed %v", w.Code, f.sec.has(serviceGrantSecretName(uid)), claimed(uid))
	}
	// Another revision keeps the claim, deletes nothing and answers 409 revision_changed.
	uid = f.claimFor("corp-d")
	f.sec.set(serviceGrantSecretName(uid), []byte(`{"v":1,"written":"outside"}`))
	before = f.sec.callsOf("delete")
	w := f.retry(uid)
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), reasonServiceGrantCleanupRevisionChanged) ||
		f.sec.callsOf("delete") != before || !claimed(uid) {
		t.Fatalf("another revision: %d %s, %d deletes, claimed %v", w.Code, w.Body.String(), f.sec.callsOf("delete")-before, claimed(uid))
	}
	// A read error keeps the claim.
	f.sec.mu.Lock()
	f.sec.revErr = errSvcDown
	f.sec.mu.Unlock()
	if w = f.retry(uid); w.Code != http.StatusServiceUnavailable || !claimed(uid) || f.sec.callsOf("delete") != before {
		t.Fatalf("read error: %d, claimed %v, %d deletes", w.Code, claimed(uid), f.sec.callsOf("delete")-before)
	}
	// A store that keeps no revision: zero Delete calls, the claim kept, 409 cleanup_unsupported.
	f.sec.mu.Lock()
	f.sec.revErr, f.sec.noRev = nil, true
	f.sec.mu.Unlock()
	if w = f.retry(uid); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), reasonServiceGrantCleanupUnsupported) ||
		f.sec.callsOf("delete") != before || !claimed(uid) {
		t.Fatalf("no revision: %d %s, %d deletes, claimed %v; want 409, zero Delete calls, the claim kept",
			w.Code, w.Body.String(), f.sec.callsOf("delete")-before, claimed(uid))
	}
	// The worker does the same.
	f.srv.serviceGrantCleanupPass(context.Background())
	if f.sec.callsOf("delete") != before || !claimed(uid) {
		t.Fatalf("the worker deleted on a store with no revision: %d deletes", f.sec.callsOf("delete")-before)
	}
}
