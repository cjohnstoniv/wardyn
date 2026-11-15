// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
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

// TestAuditCanaryAppendError pins who a failed canary append blames: a trigger of the operator's whose function sets
// no search_path when there is one, else the chain trigger, and the cause is wrapped either way.
func TestAuditCanaryAppendError(t *testing.T) {
	cause := &pgconn.PgError{Code: "42P01"}
	got := auditCanaryAppendError([]string{"outbox_t (ops.outbox_fn)", "meta_t (ops.meta_fn)"}, cause)
	if !errors.Is(got, cause) || !strings.Contains(got.Error(), "(outbox_t (ops.outbox_fn), meta_t (ops.meta_fn))") ||
		strings.Contains(got.Error(), "chain trigger is catalogued") {
		t.Fatalf("with pathless triggers = %v; want them named, the chain trigger not blamed, the cause wrapped", got)
	}
	got = auditCanaryAppendError(nil, cause)
	if !errors.Is(got, cause) || !strings.Contains(got.Error(), "chain trigger is catalogued and enabled but not working") {
		t.Fatalf("with none = %v; want the chain trigger named and the cause wrapped", got)
	}
}
