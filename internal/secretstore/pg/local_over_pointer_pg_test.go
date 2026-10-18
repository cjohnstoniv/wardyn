// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package pg

import "testing"

// A local Put over a row that still points into the external store this wardynd reads replaces the
// row and removes the old value from that store, so a later delete or erase leaves nothing behind.
// A pointer into a store it does not reach is replaced as before.
func TestLocalPutOverAPointerRemovesTheOldValue(t *testing.T) {
	pool := rekeyDatabase(t)
	id := mustIdentity(t)
	vault, azure := newMemExt("vaultkv"), newMemExt("azurekv")
	mustPut(t, mixedStore(t, pool, id, false, vault, true), "bob", "rotated", "leaked-pat")
	mustPut(t, mixedStore(t, pool, id, false, azure, true), "zoe", "elsewhere", "zoe-azure")

	local := mixedStore(t, pool, id, false, vault, false)
	mustPut(t, local, "bob", "rotated", "fresh-pat")
	if got := rowOf(t, pool, "bob", "rotated"); got.version != 1 {
		t.Fatalf("the replaced row = %+v, want a local v1 row", got)
	}
	mustGetOwned(t, local, "bob", "rotated", "fresh-pat")
	if _, err := vault.Get(t.Context(), "bob", "rotated", vault.key("bob", "rotated")); err == nil {
		t.Error("the old value is still in the external store, named by no row")
	}

	mustPut(t, local, "zoe", "elsewhere", "zoe-local")
	if got := rowOf(t, pool, "zoe", "elsewhere"); got.version != 1 {
		t.Fatalf("a pointer into an unreached store = %+v after a local Put, want a local v1 row", got)
	}
	if azure.deletes() != 0 {
		t.Error("a store this wardynd does not read was written to")
	}
}
