// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package secretstore

import "testing"

// rowStore is the fallback fake whose revision is a database row of owner.
type rowStore struct {
	*fallbackStore
	owner string
}

func (r *rowStore) RevisionRowOwner() (string, bool) { return r.owner, true }

// A store whose revision is a secrets row names its owner, directly and through the audit wrapper; a store
// without one, wrapped or not, answers ok=false.
func TestRevisionRowOf(t *testing.T) {
	cases := []struct {
		name      string
		st        Store
		wantOwner string
		wantOK    bool
	}{
		{"row store", &rowStore{fallbackStore: newBare(), owner: "op"}, "op", true},
		{"row store, audited", Audited(&rowStore{fallbackStore: newBare(), owner: "op"}, &recorded{}), "op", true},
		{"no row", newBare(), "", false},
		{"no row, audited", Audited(newBare(), &recorded{}), "", false},
		{"revisioned without a row, audited", Audited(&capableStore{fallbackStore: newBare(), rev: "r1"}, &recorded{}), "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			owner, ok := RevisionRowOf(c.st)
			if owner != c.wantOwner || ok != c.wantOK {
				t.Fatalf("RevisionRowOf = %q, %v; want %q, %v", owner, ok, c.wantOwner, c.wantOK)
			}
		})
	}
}
