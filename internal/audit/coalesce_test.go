// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// syncRecorder is captureRecorder with a lock, for the timer-driven test.
type syncRecorder struct {
	mu  sync.Mutex
	got []types.AuditEvent
}

func (s *syncRecorder) Record(_ context.Context, ev types.AuditEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, ev)
	return nil
}

func (s *syncRecorder) events() []types.AuditEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]types.AuditEvent(nil), s.got...)
}

func denial(actor, target, reason string) types.AuditEvent {
	data, _ := json.Marshal(map[string]any{"reason": reason, "method": "POST"})
	return types.AuditEvent{
		ID: uuid.New(), Time: time.Now().UTC(), ActorType: types.ActorHuman, Actor: actor,
		Action: DenialAction, Target: target, Outcome: DenialOutcome, Data: data,
	}
}

func dataOf(t *testing.T, ev types.AuditEvent) map[string]any {
	t.Helper()
	m := map[string]any{}
	if err := json.Unmarshal(ev.Data, &m); err != nil {
		t.Fatalf("data %s: %v", ev.Data, err)
	}
	return m
}

// chain is the serve chain's head: the stamp above the coalescer above inner.
func chain(inner Recorder, c *DenialCoalescer) Recorder {
	c.Inner = inner
	return DryRunRecorder{Inner: c}
}

// Three identical refused dry runs give one full row marked dry_run and, when
// the window closes, one summary row counting all three.
func TestDenialCoalescerSummarisesRepeatedDryRunDenials(t *testing.T) {
	inner := &syncRecorder{}
	c := &DenialCoalescer{}
	rec := chain(inner, c)
	ctx := WithDryRun(context.Background())
	for range 3 {
		if err := rec.Record(ctx, denial("sub-a", "runs.image", "byoi_user")); err != nil {
			t.Fatal(err)
		}
	}
	if got := inner.events(); len(got) != 1 {
		t.Fatalf("before the window closes: %d rows, want the first only", len(got))
	}
	c.Flush(context.Background())
	got := inner.events()
	if len(got) != 2 {
		t.Fatalf("after flush: %d rows, want 2", len(got))
	}
	if d := dataOf(t, got[0]); got[0].Action != DenialAction || d["dry_run"] != true || d["reason"] != "byoi_user" {
		t.Errorf("first row = %s %v, want a full authz.denied with dry_run true", got[0].Action, d)
	}
	sum := got[1]
	if sum.Action != CoalesceAction || sum.Outcome != DenialOutcome || sum.Actor != "sub-a" ||
		sum.ActorType != types.ActorHuman || sum.Target != "runs.image" || sum.RunID != nil {
		t.Errorf("summary = %+v, want the first row's actor, actor_type and target", sum)
	}
	d := dataOf(t, sum)
	if len(d) != 6 || d["dry_run"] != true || d["reason"] != "byoi_user" || d["count"] != float64(3) || d["suppressed"] != float64(2) {
		t.Errorf("summary data = %v, want {dry_run, reason, count 3, suppressed 2, first_at, last_at}", d)
	}
	for _, k := range []string{"first_at", "last_at"} {
		if _, ok := d[k].(string); !ok {
			t.Errorf("summary data %s = %v, want a timestamp", k, d[k])
		}
	}
	// The recorder below saw two new events, never an update of the first.
	if got[0].ID == got[1].ID {
		t.Error("the summary reused the first row's id")
	}
	// A flushed window is closed: nothing more is written for it.
	c.Flush(context.Background())
	if n := len(inner.events()); n != 2 {
		t.Errorf("second flush wrote rows: %d, want 2", n)
	}
}

// A window with no repeat writes no summary; different keys do not share one.
func TestDenialCoalescerNoSummaryWithoutRepeats(t *testing.T) {
	inner := &syncRecorder{}
	c := &DenialCoalescer{}
	rec := chain(inner, c)
	ctx := WithDryRun(context.Background())
	for _, ev := range []types.AuditEvent{
		denial("sub-a", "runs.image", "byoi_user"),
		denial("sub-b", "runs.image", "byoi_user"),
		denial("sub-a", "runs.policy", "byoi_user"),
		denial("sub-a", "runs.image", "capability_policy"),
	} {
		_ = rec.Record(ctx, ev)
	}
	c.Flush(context.Background())
	if got := inner.events(); len(got) != 4 {
		t.Fatalf("%d rows, want 4 full rows and no summary", len(got))
	}
}

// A refusal recorded outside a dry run (a launch refused before its run row
// exists carries a NULL run_id too) is never coalesced and never marked, even
// when its own detail names dry_run.
func TestDenialCoalescerLeavesLaunchRefusalsAlone(t *testing.T) {
	inner := &syncRecorder{}
	c := &DenialCoalescer{}
	rec := chain(inner, c)
	for range 3 {
		ev := denial("sub-a", "runs.image", "byoi_user")
		ev.Data = json.RawMessage(`{"reason":"byoi_user","dry_run":true}`)
		_ = rec.Record(context.Background(), ev)
	}
	c.Flush(context.Background())
	got := inner.events()
	if len(got) != 3 {
		t.Fatalf("%d launch rows, want 3", len(got))
	}
	for _, ev := range got {
		if _, ok := dataOf(t, ev)["dry_run"]; ok {
			t.Errorf("launch row carries dry_run: %s", ev.Data)
		}
	}
}

// Only authz.denied/denied is summarised: a repeated workspace.provider.admit
// on the preflight path is marked but written every time.
func TestDenialCoalescerPassesOtherMarkedEventsThrough(t *testing.T) {
	inner := &syncRecorder{}
	c := &DenialCoalescer{}
	rec := chain(inner, c)
	ctx := WithDryRun(context.Background())
	for range 3 {
		ev := denial("wardynd", "host", "legacy_host")
		ev.Action, ev.Outcome, ev.ActorType = "workspace.provider.admit", "success", types.ActorSystem
		_ = rec.Record(ctx, ev)
	}
	// Same action, a different outcome: not a denial either.
	for range 2 {
		ev := denial("sub-a", "runs.image", "byoi_user")
		ev.Outcome = "failure"
		_ = rec.Record(ctx, ev)
	}
	c.Flush(context.Background())
	got := inner.events()
	if len(got) != 5 {
		t.Fatalf("%d rows, want 5 unsummarised", len(got))
	}
	for _, ev := range got {
		if ev.Action == CoalesceAction {
			t.Errorf("a summary was written for %+v", ev)
		}
		if dataOf(t, ev)["dry_run"] != true {
			t.Errorf("a marked row lacks dry_run: %s", ev.Data)
		}
	}
}

// A full map closes its oldest window early and writes that summary: no count
// is dropped.
func TestDenialCoalescerFullMapWritesOldestSummaryEarly(t *testing.T) {
	inner := &syncRecorder{}
	c := &DenialCoalescer{MaxKeys: 2}
	rec := chain(inner, c)
	ctx := WithDryRun(context.Background())
	for range 4 {
		_ = rec.Record(ctx, denial("sub-a", "t1", "byoi_user"))
	}
	_ = rec.Record(ctx, denial("sub-a", "t2", "byoi_user"))
	if n := len(inner.events()); n != 2 {
		t.Fatalf("before the map is full: %d rows, want 2", n)
	}
	_ = rec.Record(ctx, denial("sub-a", "t3", "byoi_user"))
	var early *types.AuditEvent
	for _, ev := range inner.events() {
		if ev.Action == CoalesceAction {
			early = &ev
		}
	}
	if early == nil {
		t.Fatalf("the full map dropped the oldest window's count: %d rows, no summary", len(inner.events()))
	}
	if d := dataOf(t, *early); early.Target != "t1" || d["count"] != float64(4) || d["suppressed"] != float64(3) {
		t.Errorf("early summary = %s %v, want t1 with count 4", early.Target, d)
	}
	// t1 is closed, so its next denial opens a fresh window and is written in full.
	before := len(inner.events())
	_ = rec.Record(ctx, denial("sub-a", "t1", "byoi_user"))
	if n := len(inner.events()); n <= before {
		t.Errorf("a denial after its window was closed early was swallowed")
	}
}

// The window closes on its own: the summary arrives without a flush.
func TestDenialCoalescerWindowClosesOnTimer(t *testing.T) {
	inner := &syncRecorder{}
	c := &DenialCoalescer{Window: 20 * time.Millisecond}
	rec := chain(inner, c)
	ctx := WithDryRun(context.Background())
	_ = rec.Record(ctx, denial("sub-a", "runs.image", "byoi_user"))
	_ = rec.Record(ctx, denial("sub-a", "runs.image", "byoi_user"))
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := inner.events(); len(got) == 2 && got[1].Action == CoalesceAction {
			if d := dataOf(t, got[1]); d["count"] != float64(2) {
				t.Fatalf("summary data = %v, want count 2", d)
			}
			// The window is gone: the same denial now opens a new one in full.
			_ = rec.Record(ctx, denial("sub-a", "runs.image", "byoi_user"))
			if n := len(inner.events()); n != 3 {
				t.Fatalf("%d rows after the window closed, want a fresh full row (3)", n)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no summary after the window: %d rows", len(inner.events()))
}
