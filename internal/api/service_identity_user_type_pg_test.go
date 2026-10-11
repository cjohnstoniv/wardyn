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
	// overlap runs call while tx is open, and commits tx once call has either finished or is waiting on a
	// lock, so call's statements started before the commit.
	overlap := func(t *testing.T, tx pgx.Tx, call func() error) error {
		t.Helper()
		done := make(chan error, 1)
		go func() { done <- call() }()
		deadline := time.Now().Add(20 * time.Second)
		for {
			select {
			case err := <-done:
				if cerr := tx.Commit(ctx); cerr != nil {
					t.Fatal(cerr)
				}
				return err
			default:
			}
			var waiting bool
			if err := e.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks WHERE locktype = 'advisory' AND NOT granted)`).Scan(&waiting); err != nil {
				t.Fatal(err)
			}
			if waiting {
				if err := tx.Commit(ctx); err != nil {
					t.Fatal(err)
				}
				return <-done
			}
			if time.Now().After(deadline) {
				t.Fatal("the overlapping call neither finished nor waited")
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	t.Run("delete_during_create", func(t *testing.T) {
		const typeID, name = "ut-race-a", "svc-race-a"
		newType(t, typeID)
		tx := open(t, typeID, `INSERT INTO service_identities (name, description, sponsor, sponsor_since, user_type, created_by, created_at)
			VALUES ($1, '', $2, clock_timestamp(), $3, 'sec', clock_timestamp())`, name, svcSponsor, typeID)
		if err := overlap(t, tx, func() error { return e.st.DeleteUserType(ctx, typeID) }); !errors.Is(err, store.ErrConflict) {
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
		err := overlap(t, tx, func() error {
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
