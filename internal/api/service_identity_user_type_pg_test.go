// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestServiceIdentity_UserTypeDeleteWhileNamed: a user type a service identity carries cannot be deleted
// while the identity is not retired, whether or not it holds a live token and whether or not it is
// suspended; once the identity is retired the delete goes through.
func TestServiceIdentity_UserTypeDeleteWhileNamed(t *testing.T) {
	e := newSvcEnv(t)
	ctx := context.Background()
	setup := func(t *testing.T, suffix string) (typeID, name string) {
		t.Helper()
		typeID, name = "ut-"+suffix, "svc-"+suffix
		if _, err := e.st.CreateUserType(ctx, types.UserType{ID: typeID, Name: "Type " + suffix, Priority: 3, CreatedBy: "sec"}); err != nil {
			t.Fatal(err)
		}
		if w := doSSO(t, e.h.srv, http.MethodPost, "/api/v1/service-identities", e.sec,
			`{"name":"`+name+`","sponsor":"`+svcSponsor+`","user_type":"`+typeID+`"}`); w.Code != http.StatusCreated {
			t.Fatalf("create service identity: %d %s", w.Code, w.Body.String())
		}
		return typeID, name
	}
	deleteType := func(t *testing.T, typeID string) (int, string) {
		t.Helper()
		w := doSSO(t, e.h.srv, http.MethodDelete, "/api/v1/user-types/"+typeID, e.sec, "")
		return w.Code, respReason(w)
	}
	refused := func(t *testing.T, typeID string) {
		t.Helper()
		if code, reason := deleteType(t, typeID); code != http.StatusConflict || reason != "user_type_in_use" {
			t.Errorf("delete of a user type a service identity carries: %d %s, want 409 user_type_in_use", code, reason)
		}
		if _, err := e.st.GetUserType(ctx, typeID); err != nil {
			t.Errorf("user type gone after a refused delete: %v", err)
		}
	}

	t.Run("no_live_token", func(t *testing.T) {
		typeID, _ := setup(t, "notoken")
		refused(t, typeID)
	})
	t.Run("active_with_token", func(t *testing.T) {
		typeID, name := setup(t, "active")
		e.mustMint(t, e.sponsor, name)
		refused(t, typeID)
	})
	t.Run("suspended", func(t *testing.T) {
		typeID, name := setup(t, "suspended")
		if w := doSSO(t, e.h.srv, http.MethodPatch, "/api/v1/service-identities/"+name, e.sec, `{"suspended":true}`); w.Code != http.StatusOK {
			t.Fatalf("suspend: %d %s", w.Code, w.Body.String())
		}
		refused(t, typeID)
	})
	t.Run("retired", func(t *testing.T) {
		typeID, name := setup(t, "retired")
		if w := doSSO(t, e.h.srv, http.MethodDelete, "/api/v1/service-identities/"+name, e.sec, ""); w.Code != http.StatusOK {
			t.Fatalf("retire: %d %s", w.Code, w.Body.String())
		}
		if code, reason := deleteType(t, typeID); code != http.StatusNoContent {
			t.Errorf("delete of a user type only a retired service identity carries: %d %s, want 204", code, reason)
		}
	})
	t.Run("orphaned_id_not_recreated", func(t *testing.T) {
		typeID, _ := setup(t, "orphan")
		if _, err := e.pool.Exec(ctx, `DELETE FROM user_types WHERE id = $1`, typeID); err != nil {
			t.Fatal(err)
		}
		if _, err := e.st.CreateUserType(ctx, types.UserType{ID: typeID, Name: "Again", CreatedBy: "sec"}); !errors.Is(err, store.ErrConflict) {
			t.Errorf("recreating a user type id a service identity still carries: %v, want ErrConflict", err)
		}
	})
}

// TestServiceIdentity_UserTypeCreateDeleteOrdered: a service identity create and a user type delete that
// overlap are ordered, so neither leaves a service identity carrying a type that no longer exists. Each
// side runs while the other's transaction is open and uncommitted, then that transaction commits.
func TestServiceIdentity_UserTypeCreateDeleteOrdered(t *testing.T) {
	e := newSvcEnv(t)
	ctx := context.Background()
	newType := func(t *testing.T, id string) {
		t.Helper()
		if _, err := e.st.CreateUserType(ctx, types.UserType{ID: id, Name: "Type " + id, Priority: 3, CreatedBy: "sec"}); err != nil {
			t.Fatal(err)
		}
	}
	// open begins the other side's transaction: it takes the same lock the store does, then writes.
	open := func(t *testing.T, typeID, write string, args ...any) pgx.Tx {
		t.Helper()
		tx, err := e.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('user_types:' || $1, 0))`, typeID); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, write, args...); err != nil {
			t.Fatal(err)
		}
		return tx
	}
	// overlap runs call while tx is open, and commits tx once call has either finished or is waiting on
	// typeID's lock, so call's statements started before the commit.
	overlap := func(t *testing.T, tx pgx.Tx, typeID string, call func() error) error {
		t.Helper()
		done := goCall(call)
		untilOrDone(t, "the overlapping call waits on the type lock", done, func() bool { return typeLockWaiter(t, e.pool, typeID) })
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		return done.wait(t)
	}

	t.Run("delete_during_create", func(t *testing.T) {
		const typeID, name = "ut-race-a", "svc-race-a"
		newType(t, typeID)
		tx := open(t, typeID, `INSERT INTO service_identities (name, description, sponsor, sponsor_since, user_type, created_by, created_at)
			VALUES ($1, '', $2, clock_timestamp(), $3, 'sec', clock_timestamp())`, name, svcSponsor, typeID)
		if err := overlap(t, tx, typeID, func() error { return e.st.DeleteUserType(ctx, typeID) }); !errors.Is(err, store.ErrConflict) {
			t.Errorf("user type delete overlapping a service identity create: %v, want ErrConflict", err)
		}
		if _, err := e.st.GetUserType(ctx, typeID); err != nil {
			t.Errorf("the user type a service identity now carries is gone: %v", err)
		}
	})
	t.Run("create_during_delete", func(t *testing.T) {
		const typeID, name = "ut-race-b", "svc-race-b"
		newType(t, typeID)
		tx := open(t, typeID, `DELETE FROM user_types WHERE id = $1`, typeID)
		err := overlap(t, tx, typeID, func() error {
			_, err := e.st.CreateServiceIdentity(ctx, types.ServiceIdentity{Name: name, Sponsor: svcSponsor, UserType: typeID, CreatedBy: "sec"})
			return err
		})
		if !errors.Is(err, store.ErrServiceUserTypeUnknown) {
			t.Errorf("service identity create overlapping a user type delete: %v, want ErrServiceUserTypeUnknown", err)
		}
		var n int
		if qerr := e.pool.QueryRow(ctx, `SELECT count(*) FROM service_identities WHERE name = $1`, name).Scan(&n); qerr != nil || n != 0 {
			t.Errorf("service identities named %s: %d (%v), want none", name, n, qerr)
		}
	})
}

// TestServiceIdentity_UserTypeCreateDeleteRace: a real CreateServiceIdentity and a real DeleteUserType on
// one type, released together, end in one of the two serial outcomes and never leave an identity whose
// type is gone. A transaction the test holds stops the first call inside its own transaction, after it has
// taken the type lock, until the second call is waiting; each winning order is driven once.
func TestServiceIdentity_UserTypeCreateDeleteRace(t *testing.T) {
	e := newSvcEnv(t)
	ctx := context.Background()
	observed := map[string]bool{}
	type outcome struct{ create, del error }
	race := func(t *testing.T, typeID, name string, hold func(tx pgx.Tx), firstWaits string, createFirst bool) outcome {
		t.Helper()
		if _, err := e.st.CreateUserType(ctx, types.UserType{ID: typeID, Name: "Type " + typeID, Priority: 3, CreatedBy: "sec"}); err != nil {
			t.Fatal(err)
		}
		tx, err := e.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
		var holder int32
		if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&holder); err != nil {
			t.Fatal(err)
		}
		hold(tx)
		create := func() error {
			_, err := e.st.CreateServiceIdentity(ctx, types.ServiceIdentity{Name: name, Sponsor: svcSponsor, UserType: typeID, CreatedBy: "sec"})
			return err
		}
		del := func() error { return e.st.DeleteUserType(ctx, typeID) }
		first, second := del, create
		if createFirst {
			first, second = create, del
		}
		firstDone := goCall(first)
		untilOrDone(t, "the first call waits on the held row", firstDone, func() bool { return blockedBy(t, e.pool, holder, firstWaits) })
		if firstDone.finished() {
			t.Fatal("the first call finished while the test held its row")
		}
		secondDone := goCall(second)
		untilOrDone(t, "the second call waits on the type lock", secondDone, func() bool { return typeLockWaiter(t, e.pool, typeID) })
		if err := tx.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		a, b := firstDone.wait(t), secondDone.wait(t)
		out := outcome{create: b, del: a}
		if createFirst {
			out = outcome{create: a, del: b}
		}
		var orphans int
		if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM service_identities si WHERE si.retired_at IS NULL
			AND si.name = $1
			AND NOT EXISTS (SELECT 1 FROM user_types ut WHERE ut.id = si.user_type)`, name).Scan(&orphans); err != nil {
			t.Fatal(err)
		}
		if orphans != 0 {
			t.Errorf("%d service identities carry a user type that no longer exists", orphans)
		}
		switch {
		case out.create == nil && errors.Is(out.del, store.ErrConflict):
			observed["create_then_delete"] = true
		case out.del == nil && errors.Is(out.create, store.ErrServiceUserTypeUnknown):
			observed["delete_then_create"] = true
		default:
			t.Errorf("create %v, delete %v: neither serial outcome", out.create, out.del)
		}
		return out
	}

	t.Run("create_wins", func(t *testing.T) {
		// An uncommitted row of the same name stops the create at its INSERT, after it took the type lock
		// and read the type.
		race(t, "ut-real-a", "svc-real-a", func(tx pgx.Tx) {
			if _, err := tx.Exec(ctx, `INSERT INTO service_identities (name, description, sponsor, sponsor_since, user_type, created_by, created_at)
				VALUES ('svc-real-a', '', $1, clock_timestamp(), 'standard', 'sec', clock_timestamp())`, svcSponsor); err != nil {
				t.Fatal(err)
			}
		}, "%INSERT INTO service_identities%", true)
	})
	t.Run("delete_wins", func(t *testing.T) {
		// A row lock on the type stops the delete at its DELETE, after it took the type lock.
		race(t, "ut-real-b", "svc-real-b", func(tx pgx.Tx) {
			if _, err := tx.Exec(ctx, `SELECT 1 FROM user_types WHERE id = 'ut-real-b' FOR UPDATE`); err != nil {
				t.Fatal(err)
			}
		}, "%DELETE FROM user_types%", false)
	})
	if !observed["create_then_delete"] || !observed["delete_then_create"] {
		t.Errorf("serial orders observed %v, want both", observed)
	}
}

// pendingCall is a store call running on its own goroutine.
type pendingCall struct {
	done chan struct{}
	err  error
}

func goCall(call func() error) *pendingCall {
	p := &pendingCall{done: make(chan struct{})}
	go func() { p.err = call(); close(p.done) }()
	return p
}

func (p *pendingCall) finished() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

func (p *pendingCall) wait(t *testing.T) error {
	t.Helper()
	select {
	case <-p.done:
		return p.err
	case <-time.After(20 * time.Second):
		t.Fatal("the store call did not finish within 20s")
		return nil
	}
}

// untilOrDone waits, at most 20s, until cond holds or the call has finished.
func untilOrDone(t *testing.T, what string, p *pendingCall, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); !p.finished() && !cond(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting until %s", what)
		}
	}
}

// typeLockWaiter reports whether a backend in this database waits on user type typeID's write lock.
func typeLockWaiter(t *testing.T, pool *pgxpool.Pool, typeID string) bool {
	t.Helper()
	var waiting bool
	if err := pool.QueryRow(context.Background(), `SELECT EXISTS (SELECT 1 FROM pg_locks
		WHERE locktype = 'advisory' AND NOT granted AND objsubid = 1
		AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
		AND ((classid::text::bigint << 32) | objid::text::bigint) = hashtextextended('user_types:' || $1, 0))`, typeID).Scan(&waiting); err != nil {
		t.Fatal(err)
	}
	return waiting
}

// blockedBy reports whether a backend in this database running a statement like pattern waits on holder.
func blockedBy(t *testing.T, pool *pgxpool.Pool, holder int32, pattern string) bool {
	t.Helper()
	var blocked bool
	if err := pool.QueryRow(context.Background(), `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
		WHERE datname = current_database() AND $1 = ANY (pg_blocking_pids(pid)) AND query LIKE $2)`, holder, pattern).Scan(&blocked); err != nil {
		t.Fatal(err)
	}
	return blocked
}
