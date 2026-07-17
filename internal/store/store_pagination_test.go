// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Unit tests for the shared LIMIT/OFFSET rendering that backs every *Page store
// method. This is the one piece of pagination logic that is worth pinning
// without a live DB: get the clause/arg bookkeeping wrong and every list
// endpoint pages incorrectly. The end-to-end behaviour (real LIMIT/OFFSET/order
// against Postgres) is covered by the WARDYN_TEST_PG-gated tests in
// store_pagination_pg_test.go.
package store

import (
	"reflect"
	"testing"
)

func TestPageAppendTo(t *testing.T) {
	tests := []struct {
		name     string
		page     Page
		baseArgs []any
		wantQ    string
		wantArgs []any
	}{
		{
			// Limit<=0 is the internal-caller / plain-List* contract: NO clause,
			// args untouched — the whole table, unbounded.
			name:     "unbounded limit zero",
			page:     Page{},
			baseArgs: nil,
			wantQ:    "SELECT x",
			wantArgs: nil,
		},
		{
			name:     "unbounded negative limit ignores offset",
			page:     Page{Limit: -1, Offset: 5},
			baseArgs: nil,
			wantQ:    "SELECT x",
			wantArgs: nil,
		},
		{
			// Limit only: single positional arg at $1 (no prior args).
			name:     "limit only",
			page:     Page{Limit: 50},
			baseArgs: nil,
			wantQ:    "SELECT x LIMIT $1",
			wantArgs: []any{50},
		},
		{
			// Offset>0 emits both, numbered after the limit arg.
			name:     "limit and offset",
			page:     Page{Limit: 50, Offset: 100},
			baseArgs: nil,
			wantQ:    "SELECT x LIMIT $1 OFFSET $2",
			wantArgs: []any{50, 100},
		},
		{
			// Placeholders count PRE-BOUND args: with runID at $1 (the audit /
			// state-filter queries), LIMIT lands at $2, OFFSET at $3.
			name:     "numbers after a bound arg",
			page:     Page{Limit: 10, Offset: 3},
			baseArgs: []any{"run-1"},
			wantQ:    "SELECT x WHERE run_id=$1 LIMIT $2 OFFSET $3",
			wantArgs: []any{"run-1", 10, 3},
		},
		{
			// Offset 0 is a no-op (never emitted), even with a limit.
			name:     "zero offset omitted",
			page:     Page{Limit: 10, Offset: 0},
			baseArgs: []any{"run-1"},
			wantQ:    "SELECT x WHERE run_id=$1 LIMIT $2",
			wantArgs: []any{"run-1", 10},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := "SELECT x"
			if len(tt.baseArgs) > 0 {
				base = "SELECT x WHERE run_id=$1"
			}
			gotQ, gotArgs := tt.page.appendTo(base, tt.baseArgs)
			if gotQ != tt.wantQ {
				t.Errorf("query = %q, want %q", gotQ, tt.wantQ)
			}
			if !reflect.DeepEqual(gotArgs, tt.wantArgs) {
				t.Errorf("args = %v, want %v", gotArgs, tt.wantArgs)
			}
		})
	}
}
