// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// Governance four-eyes for key-domain assignments, against a real Postgres: with the switch on, a
// human's PUT or DELETE is held, and only a distinct security admin's approval writes it.
//
// Guarded by WARDYN_TEST_PG (throwawayPGPool): skipped cleanly when unset, must PASS when set.

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/secretstore/keydomain"
	"github.com/cjohnstoniv/wardyn/internal/testutil"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// newKeyDomainGovEnv is a four-eyes environment whose key-domain service declares "a" and "b".
func newKeyDomainGovEnv(t *testing.T) (*govEnv, *keydomain.Service) {
	t.Helper()
	t.Setenv(envGovernanceSecondHuman, "true")
	pool := throwawayPGPool(t)
	svc := keydomain.NewService(pool, []string{"a", "b"})
	return newGovEnvOn(t, pool, func(c *Config) { c.KeyDomains = svc }), svc
}

const keyDomainEngPath = "/api/v1/key-domains/assignments/group/eng"

func (e *govEnv) keyDomainOf(svc *keydomain.Service, subjectType, subject string) (string, bool) {
	e.t.Helper()
	got, found, err := svc.Get(e.t.Context(), subjectType, subject)
	if err != nil {
		e.t.Fatal(err)
	}
	return got.Domain, found
}

func TestPG_KeyDomainAssignment_HeldForSecondHuman(t *testing.T) {
	e, svc := newKeyDomainGovEnv(t)

	// A set is held: nothing is written until a second security admin approves.
	set := e.pending(e.call(e.alice, http.MethodPut, keyDomainEngPath, `{"domain":"a"}`))
	if set.TargetKind != "key_domain_assignment" || set.Op != "upsert" || set.TargetKey != "group:eng" {
		t.Errorf("held set = %s %s %s, want key_domain_assignment upsert group:eng", set.TargetKind, set.Op, set.TargetKey)
	}
	if _, found := e.keyDomainOf(svc, "group", "eng"); found {
		t.Fatal("a held set was written")
	}
	if n := len(e.audits("key_domain.assignment.set")); n != 0 {
		t.Fatalf("%d key_domain.assignment.set rows before approval, want 0", n)
	}
	if w := e.call(e.alice, http.MethodPost, approvePath(set.ID), ""); w.Code != http.StatusForbidden || wireReason(t, w) != "second_human_required" {
		t.Fatalf("self-approval = %d %s, want 403 second_human_required", w.Code, w.Body)
	}
	if _, found := e.keyDomainOf(svc, "group", "eng"); found {
		t.Fatal("a refused self-approval wrote the assignment")
	}
	e.approve(set.ID)
	if d, found := e.keyDomainOf(svc, "group", "eng"); !found || d != "a" {
		t.Fatalf("after approval group:eng = (%q, %v), want a", d, found)
	}
	if got, _, _ := svc.Get(t.Context(), "group", "eng"); got.SetBy != govSubAlice {
		t.Errorf("set_by = %q, want the proposer", got.SetBy)
	}
	if rows := e.audits("key_domain.assignment.set"); len(rows) != 1 || rows[0].Actor != govSubBob || rows[0].Target != "group:eng" {
		t.Errorf("key_domain.assignment.set rows = %+v, want one by the approver naming group:eng", rows)
	} else if d := auditData(t, rows[0]); d["proposed_by"] != govSubAlice || d["change_id"] == nil || d["domain"] != "a" || d["created"] != true {
		t.Errorf("set audit data = %v, want domain, created, change_id and proposed_by", d)
	}

	// A delete is held likewise.
	del := e.pending(e.call(e.alice, http.MethodDelete, keyDomainEngPath, ""))
	if del.Op != "delete" || del.TargetKey != "group:eng" {
		t.Errorf("held delete = %s %s, want delete group:eng", del.Op, del.TargetKey)
	}
	if _, found := e.keyDomainOf(svc, "group", "eng"); !found {
		t.Fatal("a held delete removed the assignment")
	}
	if w := e.call(e.alice, http.MethodPost, approvePath(del.ID), ""); w.Code != http.StatusForbidden {
		t.Fatalf("self-approval of the delete = %d %s, want 403", w.Code, w.Body)
	}
	e.approve(del.ID)
	if _, found := e.keyDomainOf(svc, "group", "eng"); found {
		t.Fatal("the approved delete left the assignment")
	}
	if rows := e.audits("key_domain.assignment.delete"); len(rows) != 1 || rows[0].Actor != govSubBob {
		t.Errorf("key_domain.assignment.delete rows = %+v, want one by the approver", rows)
	} else if d := auditData(t, rows[0]); d["proposed_by"] != govSubAlice || d["change_id"] == nil || d["domain"] != "a" {
		t.Errorf("delete audit data = %v, want the removed domain, change_id and proposed_by", d)
	}

	// A delete of nothing is the direct 404, never held; an undeclared domain is the direct refusal.
	if w := e.call(e.alice, http.MethodDelete, keyDomainEngPath, ""); w.Code != http.StatusNotFound {
		t.Errorf("held delete of a missing assignment = %d %s, want 404", w.Code, w.Body)
	}
	if w := e.call(e.alice, http.MethodPut, keyDomainEngPath, `{"domain":"ghost"}`); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("an undeclared domain = %d %s, want the direct 422", w.Code, w.Body)
	}
	if n := e.pendingCount(); n != 0 {
		t.Errorf("%d pending changes after refusals, want 0", n)
	}
}

// TestPG_KeyDomainAssignment_StaleAndRevalidated: an approval compares the assignment with what the
// proposal saw, and re-checks the membership against the state it finds.
func TestPG_KeyDomainAssignment_StaleAndRevalidated(t *testing.T) {
	e, svc := newKeyDomainGovEnv(t)

	// The target changed under a held set (the admin token's break-glass write): stale.
	set := e.pending(e.call(e.alice, http.MethodPut, keyDomainEngPath, `{"domain":"a"}`))
	if w := e.admin(http.MethodPut, keyDomainEngPath, `{"domain":"b"}`); w.Code != http.StatusCreated {
		t.Fatalf("admin-token PUT = %d %s, want the direct 201", w.Code, w.Body)
	}
	if rows := e.audits("governance.change.bypass"); len(rows) != 1 || rows[0].Target != "group:eng" {
		t.Errorf("governance.change.bypass rows = %+v, want one naming group:eng", rows)
	}
	w := e.call(e.bob, http.MethodPost, approvePath(set.ID), "")
	if w.Code != http.StatusConflict || wireReason(t, w) != reasonGovernanceChangeStale {
		t.Fatalf("approving a set whose target changed = %d %s, want 409 %s", w.Code, w.Body, reasonGovernanceChangeStale)
	}
	if d, _ := e.keyDomainOf(svc, "group", "eng"); d != "b" {
		t.Fatalf("a stale approval wrote %q", d)
	}

	// The same for a held delete.
	del := e.pending(e.call(e.alice, http.MethodDelete, keyDomainEngPath, ""))
	if w := e.admin(http.MethodPut, keyDomainEngPath, `{"domain":"a"}`); w.Code != http.StatusOK {
		t.Fatalf("admin-token re-PUT = %d %s, want 200", w.Code, w.Body)
	}
	if w := e.call(e.bob, http.MethodPost, approvePath(del.ID), ""); w.Code != http.StatusConflict || wireReason(t, w) != reasonGovernanceChangeStale {
		t.Fatalf("approving a delete whose target changed = %d %s, want 409 stale", w.Code, w.Body)
	}
	if _, found := e.keyDomainOf(svc, "group", "eng"); !found {
		t.Fatal("a stale delete removed the assignment")
	}

	// A held group set that another assignment has since made ambiguous is refused at approval.
	if err := svc.RecordLoginGroups(t.Context(), "gina", []string{"ops", "qa"}, false); err != nil {
		t.Fatal(err)
	}
	ops := e.pending(e.call(e.alice, http.MethodPut, "/api/v1/key-domains/assignments/group/ops", `{"domain":"a"}`))
	if _, err := svc.Set(t.Context(), keydomain.Assignment{SubjectType: "group", Subject: "qa", Domain: "b", SetBy: "seed"}); err != nil {
		t.Fatal(err)
	}
	if w := e.call(e.bob, http.MethodPost, approvePath(ops.ID), ""); w.Code != http.StatusConflict || wireReason(t, w) != "key_domain_ambiguous_membership" {
		t.Fatalf("approving a set made ambiguous = %d %s, want 409 key_domain_ambiguous_membership", w.Code, w.Body)
	}
	if _, found := e.keyDomainOf(svc, "group", "ops"); found {
		t.Fatal("a refused approval wrote the assignment")
	}
}

// TestPG_KeyDomainAssignment_ConcurrentOverlappingWritesSerialize: gina last signed in with ops and qa. ops->a is
// approved and held at its write, after its membership check, by an uncommitted row at group:ops (the
// barrier). qa->b then arrives, by a second approval or by a direct write. It must wait for the first
// and then be refused, since qa->b on top of ops->a leaves gina in two domains; it must never pass its
// own check while ops->a is uncommitted and land beside it.
func TestPG_KeyDomainAssignment_ConcurrentOverlappingWritesSerialize(t *testing.T) {
	const qaPath = "/api/v1/key-domains/assignments/group/qa"
	for _, second := range []struct {
		name string
		run  func(e *govEnv, qa types.GovernanceChange) *httptest.ResponseRecorder
	}{
		{"a second approval", func(e *govEnv, qa types.GovernanceChange) *httptest.ResponseRecorder {
			return e.call(e.carol, http.MethodPost, approvePath(qa.ID), "")
		}},
		{"a direct write", func(e *govEnv, _ types.GovernanceChange) *httptest.ResponseRecorder {
			return e.admin(http.MethodPut, qaPath, `{"domain":"b"}`)
		}},
	} {
		t.Run(second.name, func(t *testing.T) {
			e, svc := newKeyDomainGovEnv(t)
			ctx := t.Context()
			if err := svc.RecordLoginGroups(ctx, "gina", []string{"ops", "qa"}, false); err != nil {
				t.Fatal(err)
			}
			ops := e.pending(e.call(e.alice, http.MethodPut, "/api/v1/key-domains/assignments/group/ops", `{"domain":"a"}`))
			qa := e.pending(e.call(e.alice, http.MethodPut, qaPath, `{"domain":"b"}`))
			waiting := func(cond string) bool {
				var n int
				if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND `+cond).Scan(&n); err != nil {
					t.Fatal(err)
				}
				return n > 0
			}

			barrier, err := testutil.PGConn(t, e.pool).Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer barrier.Rollback(ctx) //nolint:errcheck // released below; a no-op then
			if _, err := barrier.Exec(ctx, `INSERT INTO key_domain_assignments (subject_type, subject, domain, set_by) VALUES ('group', 'ops', 'a', 'barrier')`); err != nil {
				t.Fatal(err)
			}
			firstDone := make(chan *httptest.ResponseRecorder, 1)
			go func() { firstDone <- e.call(e.bob, http.MethodPost, approvePath(ops.ID), "") }()
			deadline := time.Now().Add(15 * time.Second)
			for !waiting(`wait_event = 'transactionid' AND query LIKE '%INSERT INTO key_domain_assignments%'`) {
				if time.Now().After(deadline) {
					t.Fatal("the ops approval never reached its write")
				}
				time.Sleep(10 * time.Millisecond)
			}
			// The first approval has passed its checks and waits at its write. The second either
			// completes (nothing serializes it) or waits on the assignment lock.
			secondDone := make(chan *httptest.ResponseRecorder, 1)
			go func() { secondDone <- second.run(e, qa) }()
			var w2 *httptest.ResponseRecorder
			for w2 == nil && !waiting(`wait_event_type = 'Lock' AND wait_event = 'advisory'`) {
				if time.Now().After(deadline) {
					t.Fatal("the qa write neither finished nor waited")
				}
				select {
				case w2 = <-secondDone:
				case <-time.After(10 * time.Millisecond):
				}
			}
			if err := barrier.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			w1 := <-firstDone
			if w2 == nil {
				w2 = <-secondDone
			}

			if w1.Code != http.StatusOK {
				t.Fatalf("ops approval = %d %s, want 200", w1.Code, w1.Body)
			}
			if w2.Code != http.StatusConflict || wireReason(t, w2) != "key_domain_ambiguous_membership" {
				t.Errorf("qa write after ops = %d %s, want 409 key_domain_ambiguous_membership", w2.Code, w2.Body)
			}
			if d, found := e.keyDomainOf(svc, "group", "qa"); found {
				t.Errorf("group:qa was written (%q) beside group:ops", d)
			}
			if p, err := svc.Place(ctx, "gina"); err != nil || p.Domain != "a" {
				t.Errorf("gina's placement = %+v, %v; want domain a", p, err)
			}
		})
	}
}

// TestPG_KeyDomainAssignment_SwitchOffIsDirect: with the switch off a human's write applies at once.
func TestPG_KeyDomainAssignment_SwitchOffIsDirect(t *testing.T) {
	e, svc := newKeyDomainGovEnv(t)
	t.Setenv(envGovernanceSecondHuman, "")
	if w := e.call(e.alice, http.MethodPut, keyDomainEngPath, `{"domain":"a"}`); w.Code != http.StatusCreated {
		t.Fatalf("switch-off PUT = %d %s, want 201", w.Code, w.Body)
	}
	if d, found := e.keyDomainOf(svc, "group", "eng"); !found || d != "a" {
		t.Fatalf("switch-off PUT stored (%q, %v)", d, found)
	}
	if w := e.call(e.alice, http.MethodDelete, keyDomainEngPath, ""); w.Code != http.StatusNoContent {
		t.Fatalf("switch-off DELETE = %d %s, want 204", w.Code, w.Body)
	}
	if n := e.pendingCount(); n != 0 || len(e.audits("governance.change.bypass")) != 0 {
		t.Errorf("switch off: %d pending, %d bypass rows; want none", n, len(e.audits("governance.change.bypass")))
	}
}
