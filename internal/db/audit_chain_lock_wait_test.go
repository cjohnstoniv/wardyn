// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"strings"
	"testing"
	"time"
)

// A bound of zero or less would read as lock_timeout 0, which Postgres takes to mean "wait forever".
func TestAuditChainLockWaitSQLFloorsAtOneMillisecond(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Second, 400 * time.Microsecond} {
		if _, arg := AuditChainLockWaitSQL(d); arg != "1ms" {
			t.Errorf("AuditChainLockWaitSQL(%s) argument = %q, want 1ms", d, arg)
		}
	}
}

// One statement text for every bound keeps pgx's statement cache to one entry.
func TestAuditChainLockWaitSQLIsOneTextForEveryBound(t *testing.T) {
	first, _ := AuditChainLockWaitSQL(time.Millisecond)
	for _, d := range []time.Duration{4999 * time.Millisecond, 5 * time.Second} {
		sql, arg := AuditChainLockWaitSQL(d)
		if sql != first {
			t.Errorf("AuditChainLockWaitSQL(%s) text = %q, want %q", d, sql, first)
		}
		if strings.Contains(sql, arg) {
			t.Errorf("the bound %q appears in the statement text %q", arg, sql)
		}
	}
}

func TestAuditChainLockTimeoutSQLIsTheFullBound(t *testing.T) {
	if got, want := AuditChainLockTimeoutSQL(), "SET LOCAL lock_timeout = '5000ms'"; got != want {
		t.Errorf("AuditChainLockTimeoutSQL() = %q, want %q", got, want)
	}
	if _, arg := AuditChainLockWaitSQL(AuditChainLockTimeout); arg != "5000ms" {
		t.Errorf("AuditChainLockWaitSQL(AuditChainLockTimeout) argument = %q, want 5000ms", arg)
	}
}
