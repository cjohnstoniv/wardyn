// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// TestAuditCanaryTransient pins the one direction the boot canary must not get
// wrong: only a lock wait that timed out or a cancelled statement is let
// through as "could not run"; every other failure refuses the boot.
func TestAuditCanaryTransient(t *testing.T) {
	ctx := context.Background()
	pg := func(code string) error { return &pgconn.PgError{Code: code} }
	for _, c := range []struct {
		name      string
		err       error
		transient bool
	}{
		{"lock_timeout", pg("55P03"), true},
		{"query_canceled", pg("57014"), true},
		{"wrapped lock_timeout", fmt.Errorf("insert: %w", pg("55P03")), true},
		{"insufficient_privilege", pg("42501"), false},
		{"raise_exception from the chain trigger", pg("P0001"), false},
		{"not a Postgres error", errors.New("connection reset"), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := auditCanaryTransient(ctx, "insert", c.err)
			if c.transient && got != nil {
				t.Fatalf("= %v, want nil: a transient failure must not refuse the boot", got)
			}
			if !c.transient && (got == nil || !errors.Is(got, c.err)) {
				t.Fatalf("= %v, want an error wrapping %v: a real canary failure must refuse the boot", got, c.err)
			}
		})
	}
}
