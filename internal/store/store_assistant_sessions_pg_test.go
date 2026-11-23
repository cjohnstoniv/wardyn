// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func newAssistantSession(principal string, run *uuid.UUID, purpose types.AssistantPurpose) types.AssistantSession {
	state := types.AssistantSessionStarting
	if run == nil {
		state = types.AssistantSessionFetching
	}
	return types.AssistantSession{
		ID: uuid.New(), RunID: run, Principal: principal, Email: principal + "@example.com", Role: "admin", UserType: "contractor",
		Groups: []string{"g1"}, GroupsTruncated: true, Purpose: purpose, TargetKind: types.AssistantTargetUnit, TargetKey: "u1",
		Binding: types.AssistantBinding{ProviderID: "p", ProviderUID: "uid-1", Model: "m"}, State: state,
		Draft: []byte(`{"a":1}`), PersonLetters: map[string]string{"Person A": "x@example.com"}, PreviewDigest: "d",
	}
}

func assistantStore(t *testing.T) (context.Context, store.AssistantSessionStore, func(*testing.T) uuid.UUID, *pgxpool.Pool) {
	t.Helper()
	pool := runsPGPoolIsolated(t)
	ctx := context.Background()
	run := func(t *testing.T) uuid.UUID {
		t.Helper()
		return persistRun(t, ctx, pool, newRun(types.RunRunning)).ID
	}
	return ctx, store.NewPG(pool), run, pool
}

// One live session per run, and one per person and purpose; an ended one frees both.
func TestAssistantSessionsOneLivePerRunAndPerPersonPG(t *testing.T) {
	ctx, st, newRunID, _ := assistantStore(t)
	run1, run2 := newRunID(t), newRunID(t)
	person := "ada-" + uuid.NewString()
	first, err := st.CreateAssistantSession(ctx, newAssistantSession(person, &run1, types.AssistantCompose))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := st.CreateAssistantSession(ctx, newAssistantSession("other-"+uuid.NewString(), &run1, types.AssistantCompose)); !errors.Is(err, store.ErrAssistantSessionLive) {
		t.Errorf("a second live session on one run: %v, want ErrAssistantSessionLive", err)
	}
	if _, err := st.CreateAssistantSession(ctx, newAssistantSession(person, &run2, types.AssistantCompose)); !errors.Is(err, store.ErrAssistantSessionLive) {
		t.Errorf("a second live session for one person and purpose: %v, want ErrAssistantSessionLive", err)
	}
	if _, err := st.CreateAssistantSession(ctx, newAssistantSession(person, &run2, types.AssistantReview)); err != nil {
		t.Errorf("the same person, another purpose: %v", err)
	}
	if ended, err := st.EndAssistantSession(ctx, first.ID, types.AssistantEndPerson); err != nil || !ended {
		t.Fatalf("end = %v, %v", ended, err)
	}
	if again, err := st.EndAssistantSession(ctx, first.ID, types.AssistantEndIdle); err != nil || again {
		t.Errorf("a second end = %v, %v, want false", again, err)
	}
	got, err := st.GetAssistantSession(ctx, first.ID, person)
	if err != nil || got.EndReason != types.AssistantEndPerson {
		t.Fatalf("read back = %+v, %v: the first end reason must stay", got, err)
	}
	if _, err := st.GetAssistantSession(ctx, first.ID, "someone-else"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("another principal read it: %v", err)
	}
	if _, err := st.GetAssistantSession(ctx, first.ID, ""); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("an empty principal read it: %v", err)
	}
	if _, err := st.CreateAssistantSession(ctx, newAssistantSession("third-"+uuid.NewString(), &run1, types.AssistantCompose)); err != nil {
		t.Errorf("a new session on an ended session's run: %v", err)
	}
}

// A session in fetching holds no run; attach moves it to starting; the identity values read back.
func TestAssistantSessionFetchingAndSnapshotPG(t *testing.T) {
	ctx, st, newRunID, _ := assistantStore(t)
	person := "bob-" + uuid.NewString()
	in := newAssistantSession(person, nil, types.AssistantCompose)
	created, err := st.CreateAssistantSession(ctx, in)
	if err != nil {
		t.Fatalf("create fetching: %v", err)
	}
	if created.Email != in.Email || created.UserType != "contractor" || !created.GroupsTruncated || len(created.Groups) != 1 ||
		created.Binding != in.Binding || created.PersonLetters["Person A"] != "x@example.com" {
		t.Errorf("the snapshot did not read back: %+v", created)
	}
	run := newRunID(t)
	if err := st.AttachAssistantRun(ctx, created.ID, run); err != nil {
		t.Fatalf("attach: %v", err)
	}
	if err := st.AttachAssistantRun(ctx, created.ID, run); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("a second attach: %v, want ErrNotFound", err)
	}
	live, err := st.LiveAssistantSessionForRun(ctx, run)
	if err != nil || live.ID != created.ID || live.State != types.AssistantSessionStarting {
		t.Errorf("live for run = %+v, %v", live, err)
	}
	ids, err := st.AssistantRunIDs(ctx, []uuid.UUID{run, uuid.New()})
	if err != nil || len(ids) != 1 || ids[0] != run {
		t.Errorf("AssistantRunIDs = %v, %v", ids, err)
	}
	made, err := st.AssistantRunIDsCreated(ctx, time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil || len(made) == 0 {
		t.Errorf("AssistantRunIDsCreated = %v, %v", made, err)
	}
}

// Event seq is assigned under the row lock, a reset continues the old session's count, and the relay's next
// turn is a person's message or a reset, with the first one flagged.
func TestAssistantEventsAndTurnsPG(t *testing.T) {
	ctx, st, newRunID, _ := assistantStore(t)
	run := newRunID(t)
	s1, err := st.CreateAssistantSession(ctx, newAssistantSession("cy-"+uuid.NewString(), &run, types.AssistantCompose))
	if err != nil {
		t.Fatal(err)
	}
	for i, kind := range []string{types.AssistantEventState, types.AssistantEventUserMessage, types.AssistantEventText, types.AssistantEventUserMessage} {
		ev, err := st.AppendAssistantEvent(ctx, s1.ID, kind, []byte(`{"text":"x"}`))
		if err != nil || ev.Seq != int64(i+1) {
			t.Fatalf("append %s = seq %d, %v", kind, ev.Seq, err)
		}
	}
	turn, first, err := st.NextAssistantTurn(ctx, s1.ID, 0)
	if err != nil || turn.Seq != 2 || !first {
		t.Errorf("first turn = seq %d first %v, %v, want seq 2 first", turn.Seq, first, err)
	}
	turn, first, err = st.NextAssistantTurn(ctx, s1.ID, 2)
	if err != nil || turn.Seq != 4 || first {
		t.Errorf("second turn = seq %d first %v, %v, want seq 4 not first", turn.Seq, first, err)
	}
	if _, _, err := st.NextAssistantTurn(ctx, s1.ID, 4); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("no turn left: %v", err)
	}
	if _, err := st.AppendAssistantEvent(ctx, uuid.New(), types.AssistantEventText, nil); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("an event on a missing session: %v", err)
	}
	s2, err := st.ReplaceAssistantSession(ctx, s1.ID, newAssistantSession("cy2-"+uuid.NewString(), &run, types.AssistantCompose), []byte(`{"turn":"reset"}`))
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	old, err := st.GetAssistantSession(ctx, s1.ID, s1.Principal)
	if err != nil || old.State != types.AssistantSessionEnded || old.EndReason != types.AssistantEndReplaced {
		t.Errorf("replaced session = %+v, %v", old, err)
	}
	turn, first, err = st.NextAssistantTurn(ctx, s2.ID, 4)
	if err != nil || turn.Seq != 5 || !first {
		t.Errorf("reset turn = seq %d first %v, %v, want seq 5 first: one run, one count", turn.Seq, first, err)
	}
	evs, err := st.ListAssistantEvents(ctx, s2.ID, 0, 10)
	if err != nil || len(evs) != 1 {
		t.Errorf("events of the new session = %d, %v", len(evs), err)
	}
}

// The retention sweep deletes an ended session's events past the window and clears its payload, keeping the
// audit fields; a live session and one inside the window keep theirs.
func TestAssistantSweepPG(t *testing.T) {
	ctx, st, newRunID, pool := assistantStore(t)
	run1, run2, run3 := newRunID(t), newRunID(t), newRunID(t)
	mk := func(run uuid.UUID, purpose types.AssistantPurpose) types.AssistantSession {
		s, err := st.CreateAssistantSession(ctx, newAssistantSession("dee-"+uuid.NewString(), &run, purpose))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.AppendAssistantEvent(ctx, s.ID, types.AssistantEventUserMessage, []byte(`{"text":"hello"}`)); err != nil {
			t.Fatal(err)
		}
		return s
	}
	old, recent, live := mk(run1, types.AssistantCompose), mk(run2, types.AssistantCompose), mk(run3, types.AssistantCompose)
	for _, s := range []types.AssistantSession{old, recent} {
		if _, err := st.EndAssistantSession(ctx, s.ID, types.AssistantEndPerson); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE assistant_sessions SET ended_at = $2 WHERE id = $1`, old.ID, time.Now().Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	n, err := st.SweepEndedAssistantSessions(ctx, time.Now().Add(-24*time.Hour), 10)
	if err != nil || n != 1 {
		t.Fatalf("sweep = %d, %v, want 1", n, err)
	}
	got, err := st.GetAssistantSession(ctx, old.ID, old.Principal)
	if err != nil {
		t.Fatal(err)
	}
	if got.Draft != nil || got.PersonLetters != nil || len(got.Groups) != 0 || got.Email != "" {
		t.Errorf("payload survived the sweep: %+v", got)
	}
	if got.Principal != old.Principal || got.Role != "admin" || got.UserType != "contractor" || !got.GroupsTruncated || got.Purpose != old.Purpose ||
		got.TargetKind != old.TargetKind || got.TargetKey != old.TargetKey || got.Binding != old.Binding || got.State != types.AssistantSessionEnded ||
		got.PreviewDigest != "d" || got.RunID == nil || *got.RunID != run1 || got.EndedAt == nil || got.EndReason != types.AssistantEndPerson {
		t.Errorf("an audit field changed: %+v", got)
	}
	if evs, _ := st.ListAssistantEvents(ctx, old.ID, 0, 10); len(evs) != 0 {
		t.Errorf("events survived the sweep: %d", len(evs))
	}
	for _, s := range []types.AssistantSession{recent, live} {
		if evs, _ := st.ListAssistantEvents(ctx, s.ID, 0, 10); len(evs) != 1 {
			t.Errorf("session %s lost events: %d", s.ID, len(evs))
		}
		kept, err := st.GetAssistantSession(ctx, s.ID, s.Principal)
		if err != nil || kept.Draft == nil || kept.PersonLetters == nil {
			t.Errorf("session %s lost its payload: %+v, %v", s.ID, kept, err)
		}
	}
}

// Erasing a person ends their live session as erased, returns its run, deletes their transcript text and
// blanks the principal and e-mail.
func TestAssistantEraseSessionsPG(t *testing.T) {
	ctx, st, newRunID, _ := assistantStore(t)
	run := newRunID(t)
	person := "eve-" + uuid.NewString()
	s, err := st.CreateAssistantSession(ctx, newAssistantSession(person, &run, types.AssistantCompose))
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{types.AssistantEventUserMessage, types.AssistantEventPersonEdit, types.AssistantEventText, types.AssistantEventToolRecord} {
		if _, err := st.AppendAssistantEvent(ctx, s.ID, kind, []byte(`{"text":"private"}`)); err != nil {
			t.Fatal(err)
		}
	}
	runs, n, err := st.EraseAssistantSessionPersonalFields(ctx, []string{person})
	if err != nil || n != 1 || len(runs) != 1 || runs[0] != run {
		t.Fatalf("erase = %v, %d, %v", runs, n, err)
	}
	if _, err := st.GetAssistantSession(ctx, s.ID, person); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("the erased principal still owns the session: %v", err)
	}
	got, err := st.GetAssistantSession(ctx, s.ID, types.AssistantErasedPrincipal)
	if err != nil || got.EndReason != types.AssistantEndErased || got.Email != "" || got.Draft != nil || got.PersonLetters != nil || len(got.Groups) != 0 {
		t.Errorf("erased row = %+v, %v", got, err)
	}
	evs, _ := st.ListAssistantEvents(ctx, s.ID, 0, 10)
	if len(evs) != 1 || evs[0].Kind != types.AssistantEventToolRecord {
		t.Errorf("events after erasure = %+v, want only the tool record", evs)
	}
}
