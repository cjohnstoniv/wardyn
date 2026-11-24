// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import "testing"

// Only the complete labels a 0.10.2 wardynd sets, at 0.10.2 or later, pass; every near miss is suspect.
func TestWardydLabelAtLeast0102(t *testing.T) {
	cases := []struct {
		label string
		want  bool
	}{
		{"wardynd/0.10.2/replica-1", true},
		{"wardynd/0.10.2-rc.1/wardynd-7d9f6c5b4-x2k8p", true},
		{"wardynd/0.11.0/replica_a.b", true},
		{"wardynd/1.0.0/r", true},
		{"wardynd-migrate/0.10.2", true},
		{"wardynd-rekey/0.10.3", true},
		{"wardynd-migrate-only/0.11.0-rc.2", true},
		{"wardynd/0.10.2-0/r", true},
		{"wardynd/0.10.2-x-y.01a/r", true},
		{"wardynd/0.10.2-0.3.7/r", true},

		{"wardynd/0.10.1/replica-1", false},
		{"wardynd/0.9.9/replica-1", false},
		{"wardynd-migrate/0.10.1", false},
		{"wardynd/0.10.2", false},               // no replica
		{"wardynd/0.10.2/", false},              // empty replica
		{"wardynd/0.10.2/replica/extra", false}, // extra component
		{"wardynd-migrate/0.10.2/extra", false},
		{"wardynd-migrate/0.10.2/replica", false},
		{"wardynd-/0.10.2", false},
		{"wardynd-Migrate/0.10.2", false},
		{"wardynd/v0.10.2/replica", false},
		{"wardynd/0.10/replica", false},
		{"wardynd/0.10.2+build/replica", false},
		{"wardynd/00.10.2/replica", false},    // a leading zero in the major number
		{"wardynd/0.010.2/replica", false},    // a leading zero in the minor number
		{"wardynd/0.10.02/replica", false},    // a leading zero in the patch number
		{"wardynd/00.010.002/replica", false}, // a leading zero in each
		{"wardynd-migrate/0.10.02", false},
		{"wardynd/0.10.2-rc..1/replica", false}, // an empty prerelease identifier
		{"wardynd/0.10.2-/replica", false},
		{"wardynd/0.10.2-rc./replica", false},
		{"wardynd/0.10.2-.rc/replica", false},
		{"wardynd/0.10.2-01/replica", false}, // a numeric prerelease identifier with a leading zero
		{"wardynd/0.10.2-rc.01/replica", false},
		{"wardynd-migrate/0.10.2-rc..1", false},
		{"wardynd/0.10.2 /replica", false},
		{"wardynd", false},
		{"wardynd/", false},
		{"wardynd-x", false},
		{"", false},
		{"psql", false},
		{"pg_dump", false},
		{"grafana-postgres-exporter", false},
		{"unknown", false},
		{"pgbouncer/0.10.2/x", false},
	}
	for _, c := range cases {
		if got := wardydLabelAtLeast0102(c.label); got != c.want {
			t.Errorf("wardydLabelAtLeast0102(%q) = %v, want %v", c.label, got, c.want)
		}
	}
}
