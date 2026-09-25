// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestCapAuditTarget is the unit half — the rule, without a database.
//
// The audit `target` is `r.URL.Path` on the authz.denied / auth.failed lanes,
// and chi matches a path of any length up to MaxHeaderBytes+4096 (~1 MiB).
// The AUTHENTICATED lane has no limiter, so a member looping
// `PUT /policies/<1 MiB>` writes megabyte rows into an append-only table an
// operator cannot delete. The cap runs at the recorders, not at
// Server.auditEvent, because internal/approval builds AuditEvent values
// directly and would bypass an api-layer cap entirely.
func TestCapAuditTarget(t *testing.T) {
	long := strings.Repeat("a", 64*1024)
	got := store.CapAuditTarget(long)
	if len(got) > store.MaxAuditTargetLen+len(store.AuditTargetTruncatedMarker) {
		t.Fatalf("capped target is %d bytes, want <= %d",
			len(got), store.MaxAuditTargetLen+len(store.AuditTargetTruncatedMarker))
	}
	if !strings.HasSuffix(got, store.AuditTargetTruncatedMarker) {
		t.Errorf("a truncated target must SAY it was truncated, or the row reads as a real path: %q", got[len(got)-40:])
	}

	// NEGATIVE CONTROL: an ordinary target is byte-identical. Every audit query
	// an operator writes matches on this column exactly.
	for _, ok := range []string{
		"/api/v1/runs/" + uuid.NewString(),
		"", "registry.npmjs.org", strings.Repeat("b", store.MaxAuditTargetLen),
	} {
		if store.CapAuditTarget(ok) != ok {
			t.Errorf("CapAuditTarget(%q) rewrote a target that is already within the cap", ok)
		}
	}
}

// TestPG_InsertAuditEventCapsTheTarget is the durable half: the cap has to hold
// at the write, not merely in a helper nobody calls.
func TestPG_InsertAuditEventCapsTheTarget(t *testing.T) {
	pool := runsPGPool(t)
	ctx := context.Background()

	ev := types.AuditEvent{
		ID: uuid.New(), Time: time.Now().UTC(), ActorType: types.ActorHuman,
		Actor: "sub-member", Action: "authz.denied",
		Target: "/api/v1/policies/" + strings.Repeat("z", 64*1024), Outcome: "denied",
	}
	if err := (store.Recorder{Pool: pool}).Record(ctx, ev); err != nil {
		t.Fatalf("record: %v", err)
	}

	rows, err := store.NewPG(pool).QueryRecentAuditEvents(ctx, 200)
	if err != nil {
		t.Fatalf("query audit: %v", err)
	}
	var found bool
	for _, got := range rows {
		if got.ID != ev.ID {
			continue
		}
		found = true
		if len(got.Target) > store.MaxAuditTargetLen+len(store.AuditTargetTruncatedMarker) {
			t.Fatalf("persisted target is %d bytes — a member can still fill the append-only table "+
				"with rows the operator cannot prune", len(got.Target))
		}
	}
	if !found {
		t.Fatal("the event did not land")
	}
}
