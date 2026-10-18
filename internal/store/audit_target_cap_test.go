// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestCapAuditTarget is the unit half — the rule, without a database.
//
// The audit `target` is `r.URL.Path` on the authz.denied / auth.fail lanes,
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

// A multi-byte rune straddling the cap, and a bare invalid byte, both left text
// Postgres refuses (22021) and the spool replays forever. The capped target must be
// valid UTF-8 and still carry the marker; a valid target is untouched.
func TestCapAuditTarget_StaysValidUTF8(t *testing.T) {
	// 511 ASCII bytes then a 2-byte rune: the cut at byte 512 falls inside it.
	straddle := strings.Repeat("a", store.MaxAuditTargetLen-1) + "é" + strings.Repeat("b", 64)
	for name, in := range map[string]string{
		"rune across the cap": straddle,
		"3-byte rune across":  strings.Repeat("a", store.MaxAuditTargetLen-1) + "€" + "tail",
		"bare invalid byte":   "/api/v1/policies/\xff",
		"invalid and long":    "\xff" + strings.Repeat("a", 2*store.MaxAuditTargetLen),
	} {
		got := store.CapAuditTarget(in)
		if !utf8.ValidString(got) {
			t.Errorf("%s: CapAuditTarget returned invalid UTF-8: %q", name, got)
		}
		if len(got) > store.MaxAuditTargetLen+len(store.AuditTargetTruncatedMarker) {
			t.Errorf("%s: capped target is %d bytes", name, len(got))
		}
		if len(in) > store.MaxAuditTargetLen && !strings.HasSuffix(got, store.AuditTargetTruncatedMarker) {
			t.Errorf("%s: a cut target must say so", name)
		}
	}
	// A valid target within the cap stays byte-identical, multi-byte or not.
	for _, ok := range []string{"/api/v1/é/€", strings.Repeat("é", store.MaxAuditTargetLen/2)} {
		if store.CapAuditTarget(ok) != ok {
			t.Errorf("CapAuditTarget(%q) rewrote a valid target within the cap", ok)
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
