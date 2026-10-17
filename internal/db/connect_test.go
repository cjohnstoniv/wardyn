// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import "testing"

// An operator's pool_max_conns is theirs: only an unset one is raised to
// DefaultPoolMaxConns, in either DSN form.
func TestDSNSetsPoolMaxConns(t *testing.T) {
	for dsn, want := range map[string]bool{
		"postgres://wardyn:pw@postgres:5432/wardyn?sslmode=disable":                  false,
		"postgres://wardyn:pw@postgres:5432/wardyn?sslmode=disable&pool_max_conns=3": true,
		"host=postgres user=wardyn dbname=wardyn":                                    false,
		"host=postgres user=wardyn dbname=wardyn pool_max_conns=2":                   true,
	} {
		if got := dsnSetsPoolMaxConns(dsn); got != want {
			t.Errorf("dsnSetsPoolMaxConns(%q) = %v, want %v", dsn, got, want)
		}
	}
}
