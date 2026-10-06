// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import "testing"

func TestNestedAcquireAllowlist_MatchesCallPaths(t *testing.T) {
	const m = modulePath
	tryLock := []string{m + "internal/db.TryAdvisoryLockConn", m + "internal/db.TryAdvisoryLock"}
	for _, c := range []struct {
		name      string
		functions []string
		want      bool
	}{
		{"tick lock closure", append(tryLock, m+"cmd/wardynd.reapTickLock.func1", "testing.tRunner"), true},
		{"tick lock closure inlined into its caller", append(tryLock, m+"cmd/wardynd.serve.terminalSandboxSweepTickLock.func1"), true},
		{"TryAdvisoryLock outside a tick lock", append(tryLock, m+"cmd/wardynd.claimSingleInstance"), false},
		{"the keyed lock", []string{m + "internal/db.AdvisoryLockKeyed", m + "internal/api.(*Server).signIn"}, true},
		{"a name that only starts with an entry's", []string{m + "internal/db.AdvisoryLockKeyedProbe"}, false},
		{"an entry's name in another package", []string{m + "internal/store.AdvisoryLockKeyed"}, false},
	} {
		if got := allowlisted(c.functions); got != c.want {
			t.Errorf("%s: allowlisted(%q) = %v, want %v", c.name, c.functions, got, c.want)
		}
	}
}
