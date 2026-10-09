// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/erasure"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

// erasureComponentStore is a store whose component erasure is recorded and can be made to fail.
type erasureComponentStore struct {
	store.ComponentStore
	calls []string
	fail  string
}

func (c *erasureComponentStore) DeleteComponentsByOwner(_ context.Context, owner string) (int, error) {
	c.calls = append(c.calls, "delete:"+owner)
	if c.fail == "delete" {
		return 0, errors.New("injected: delete failed")
	}
	return 2, nil
}

func (c *erasureComponentStore) EraseRunComponentsByOwner(_ context.Context, owner string) (int, error) {
	c.calls = append(c.calls, "erase:"+owner)
	if c.fail == "erase" {
		return 0, errors.New("injected: erase failed")
	}
	return 3, nil
}

type erasureComponentHarnessStore struct {
	store.Store
	*erasureComponentStore
}

// The components scope is registered only where the store can erase components: a store without the
// seam answers ErrNotAvailable instead of reporting the scope erased, and the SCIM purge skips it.
func TestErasureComponentsScopeNeedsAComponentStore(t *testing.T) {
	srv := newHarness(t).srv
	_, err := srv.erasureOrchestrator(nil).Orchestrate(t.Context(), "bob", []erasure.Scope{erasure.Components})
	if !errors.Is(err, erasure.ErrNotAvailable) {
		t.Fatalf("no component store: %v, want ErrNotAvailable", err)
	}
	if got, err := srv.eraseForPurge(t.Context(), "bob"); err != nil || len(got) != 0 {
		t.Errorf("purge with nothing backed = %v, %v, want a no-op", got, err)
	}
}

// The scope deletes the saved rows, then clears the snapshots, for the one person; a failure in either
// is the scope's failure, so the retry runs both again.
func TestErasureComponentsScopeRunsBothStatements(t *testing.T) {
	cs := &erasureComponentStore{}
	srv := newHarness(t).srv
	srv.cfg.Store = erasureComponentHarnessStore{Store: srv.cfg.Store, erasureComponentStore: cs}

	rep, err := srv.erasureOrchestrator(nil).Orchestrate(t.Context(), "bob", []erasure.Scope{erasure.Components})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"delete:bob", "erase:bob"}; !slices.Equal(cs.calls, want) {
		t.Errorf("calls = %v, want %v", cs.calls, want)
	}
	if got, _ := rep.Details[erasure.Components].(map[string]any); got["components"] != 2 || got["run_snapshots_cleared"] != 3 {
		t.Errorf("detail = %v, want 2 saved and 3 snapshots", rep.Details[erasure.Components])
	}

	for _, fail := range []string{"delete", "erase"} {
		cs.fail = fail
		var inc *erasure.IncompleteError
		if _, err := srv.erasureOrchestrator(nil).Orchestrate(t.Context(), "bob", []erasure.Scope{erasure.Components}); !errors.As(err, &inc) ||
			!slices.Equal(inc.Remaining, []erasure.Scope{erasure.Components}) {
			t.Errorf("%s failing: %v, want an *IncompleteError leaving the scope", fail, err)
		}
	}

	cs.fail, cs.calls = "", nil
	got, err := srv.eraseForPurge(t.Context(), "bob")
	if err != nil || got["components_erased"] != 2 {
		t.Errorf("purge = %v, %v, want components_erased 2", got, err)
	}
}
