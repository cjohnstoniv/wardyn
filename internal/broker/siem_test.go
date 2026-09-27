// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package broker

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// recordingSink is the audit.Sink WithSIEM fans the committed credential.mint
// event to. It keeps each event with the ctx error Emit saw, because a sink is
// allowed to skip on a done ctx (sinks.SyslogSink.Emit does) — a cancelled ctx
// reaching Emit IS a dropped SIEM event in production.
type recordingSink struct {
	mu      sync.Mutex
	events  []types.AuditEvent
	ctxErrs []error
}

func (s *recordingSink) Name() string { return "recording" }

func (s *recordingSink) Emit(ctx context.Context, ev types.AuditEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, ev)
	s.ctxErrs = append(s.ctxErrs, ctx.Err())
	return nil
}

func (s *recordingSink) snapshot() ([]types.AuditEvent, []error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.events), slices.Clone(s.ctxErrs)
}

// assertSIEMMints asserts the sink saw exactly one credential.mint SUCCESS per
// jti in wantJTIs and nothing else, each on a live ctx, and that no event's Data
// carries the minted token bytes.
func assertSIEMMints(t *testing.T, s *recordingSink, token string, wantJTIs ...string) {
	t.Helper()
	events, ctxErrs := s.snapshot()
	if len(events) != len(wantJTIs) {
		t.Fatalf("SIEM events = %d, want %d (one per winning mint)", len(events), len(wantJTIs))
	}
	var got []string
	for i, ev := range events {
		if ev.Action != "credential.mint" || ev.Outcome != "success" {
			t.Fatalf("SIEM event %d = %s/%s, want credential.mint/success", i, ev.Action, ev.Outcome)
		}
		if ctxErrs[i] != nil {
			t.Fatalf("SIEM event %d emitted on a done ctx (%v): a sink may drop it", i, ctxErrs[i])
		}
		if token != "" && strings.Contains(string(ev.Data), token) {
			t.Fatalf("SIEM event %d Data carries the minted token: %s", i, ev.Data)
		}
		got = append(got, jtiOf(t, ev))
	}
	slices.Sort(got)
	want := slices.Sorted(slices.Values(wantJTIs))
	if !slices.Equal(got, want) {
		t.Fatalf("SIEM event jtis = %v, want %v", got, want)
	}
}

func jtiOf(t *testing.T, ev types.AuditEvent) string {
	t.Helper()
	var d struct {
		JTI string `json:"jti"`
	}
	if err := json.Unmarshal(ev.Data, &d); err != nil {
		t.Fatalf("SIEM event Data: %v", err)
	}
	return d.JTI
}

// TestMint_SIEMEmitSurvivesCallerHangupAfterCommit: the mint is committed and
// the credential is already the caller's when the client hangs up. The SIEM copy
// of the committed credential.mint must still go out — a sink that honours the
// request ctx (sinks.SyslogSink) would otherwise drop a compliance record of a
// credential that exists.
func TestMint_SIEMEmitSurvivesCallerHangupAfterCommit(t *testing.T) {
	b, db, _, _ := newTestBroker(t)
	sink := &recordingSink{}
	b.WithSIEM(sink)
	runID := uuid.New()
	spec := githubGrantSpec(t, true)
	gid := seedGrant(db, runID, spec)
	seedApproval(db, runID, gid, spec.Scope, types.ApprovalApproved)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db.onCommit = cancel

	minted, err := b.MintForGrant(ctx, callerFor(runID), gid)
	if err != nil {
		t.Fatalf("MintForGrant: %v", err)
	}
	if ctx.Err() == nil {
		t.Fatal("onCommit did not cancel the request ctx; the test proves nothing")
	}
	assertSIEMMints(t, sink, minted.Token, minted.JTI)
}
