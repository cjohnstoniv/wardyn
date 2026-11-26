// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package maskmanifest

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// Watch re-reads at once only when its own run's values or the globals moved: a value registered for another
// run leaves the cached answer standing (#4069).
func TestWatchIgnoresAnotherRunsValues(t *testing.T) {
	m, reg := newManifests(t, downPool(t))
	a, b := uuid.New(), uuid.New()
	watch := m.Watch(b, time.Hour)
	if watch() {
		t.Fatal("Watch answered true with Postgres down")
	}
	// With no pool at all, a re-query panics: the panic is how a query is seen.
	m.pool = nil
	queried := func() (q bool) {
		defer func() {
			if recover() != nil {
				q = true
			}
		}()
		if watch() {
			t.Error("Watch changed its answer to true")
		}
		return false
	}

	reg.AddLocal(a, []byte("another-runs-value"))
	if queried() {
		t.Error("a value registered for another run made Watch query again")
	}
	reg.AddLocal(b, []byte("this-runs-own-value"))
	if !queried() {
		t.Error("a value registered for the watched run did not make Watch query again")
	}
	if queried() {
		t.Error("Watch queried again with nothing changed since its last read")
	}
	if err := reg.AddGlobal(0, "", "test-credential", time.Now(), []byte("a-process-global-value")); err != nil {
		t.Fatal(err)
	}
	if !queried() {
		t.Error("a global change did not make Watch query again")
	}
}
