// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

var errGapCov = errors.New("gapcov: injected")

// gapCovDir is a memDir whose key-destruction lookup fails.
type gapCovDir struct{ *memDir }

func (gapCovDir) AuditKeyDestroyedAt(context.Context, string) (time.Time, bool, error) {
	return time.Time{}, false, errGapCov
}

func gapCovResolveFails(context.Context, string) (string, error) { return "", errGapCov }

// A name that cannot be resolved with no pending key to hold the row fails the
// seal, wraps the cause, and creates no key for the person.
func TestGapCovSealActorResolveFailureWithoutPendingKeyFails(t *testing.T) {
	s, mk, _ := newFullSealer(t, "alice")
	s.Resolve, s.Pending = gapCovResolveFails, nil
	ev := humanEvent("alice", "run.kill")

	got, pending, err := s.Seal(t.Context(), ev)
	if !errors.Is(err, errGapCov) || pending {
		t.Fatalf("Seal = pending %v, err %v; want a failure wrapping the resolve error", pending, err)
	}
	if !strings.Contains(err.Error(), "no pending key to hold the row") {
		t.Errorf("err = %v, want the no-pending-key clause", err)
	}
	if got.Actor != "alice" || len(mk.keys["alice"]) != 0 {
		t.Errorf("actor %q, keys %v; want the event untouched and no key created", got.Actor, mk.keys)
	}
}

// A pending key of the wrong size is no pending key: the seal fails with the key store's error
// rather than writing the actor.
func TestGapCovSealActorWithAWrongSizePendingKeyFails(t *testing.T) {
	s, mk, _ := newFullSealer(t, "alice")
	mk.down = true
	s.Pending = func() []byte { return []byte("short") }

	got, pending, err := s.Seal(t.Context(), humanEvent("alice", "run.kill"))
	if err == nil || pending || !strings.Contains(err.Error(), "no pending key to hold the row: key store down") {
		t.Fatalf("Seal = pending %v, err %v; want the unusable pending key treated as none, with the key store's error", pending, err)
	}
	if got.Actor != "alice" {
		t.Errorf("actor = %q, want it unchanged on failure", got.Actor)
	}
}

// An actor cannot wait in a row whose data is not a JSON object.
func TestGapCovSealActorRefusesDataThatIsNotAnObject(t *testing.T) {
	for name, data := range map[string]string{"an array": `[1,2]`, "null": `null`} {
		s, mk, _ := newFullSealer(t, "alice")
		mk.down = true
		ev := humanEvent("alice", "run.kill")
		ev.Data = []byte(data)
		got, pending, err := s.Seal(t.Context(), ev)
		if err == nil || pending || !strings.Contains(err.Error(), "the row's data is not an object") {
			t.Errorf("%s: Seal = pending %v, err %v; want the not-an-object refusal", name, pending, err)
		}
		if got.Actor != "alice" || string(got.Data) != data {
			t.Errorf("%s: event changed on failure: actor %q data %s", name, got.Actor, got.Data)
		}
	}
}

// pendingActorRow is a row whose actor waits under the pending key, made while
// the directory was down.
func gapCovPendingActorRow(t *testing.T) (*Sealer, *memDir, types.AuditEvent) {
	t.Helper()
	s, _, dir := newFullSealer(t, "alice")
	dir.down = true
	row, pending, err := s.Seal(t.Context(), humanEvent("alice", "run.kill"))
	if err != nil || !pending || row.Actor != PendingActor {
		t.Fatalf("setup: Seal = actor %q pending %v err %v; want a pending actor", row.Actor, pending, err)
	}
	dir.down = false
	return s, dir, row
}

func TestGapCovResealActorFailures(t *testing.T) {
	ctx := t.Context()
	t.Run("data that is not json is left alone", func(t *testing.T) {
		s, _, row := gapCovPendingActorRow(t)
		row.Data = []byte(`{broken`)
		got, err := s.resealActor(ctx, row, s.Pending())
		if err != nil || string(got.Data) != `{broken` || got.Actor != PendingActor {
			t.Fatalf("resealActor = %q %s, %v; want the row unchanged and no error", got.Actor, got.Data, err)
		}
	})
	t.Run("no subject directory", func(t *testing.T) {
		s, _, row := gapCovPendingActorRow(t)
		s.Subjects = nil
		got, err := s.resealActor(ctx, row, s.Pending())
		if err == nil || !strings.Contains(err.Error(), "subject directory is not available") || got.Actor != PendingActor {
			t.Fatalf("resealActor = %q, %v; want the directory-unavailable refusal", got.Actor, err)
		}
	})
	t.Run("malformed pending actor", func(t *testing.T) {
		s, _, row := gapCovPendingActorRow(t)
		row.Data = []byte(`{"` + PendingActorKey + `":"not-a-pending-string"}`)
		_, err := s.resealActor(ctx, row, s.Pending())
		if err == nil || !strings.Contains(err.Error(), "the pending actor is malformed") {
			t.Fatalf("resealActor err = %v, want the malformed refusal", err)
		}
	})
	t.Run("pending key that does not open it", func(t *testing.T) {
		s, _, row := gapCovPendingActorRow(t)
		_, err := s.resealActor(ctx, row, make([]byte, 32))
		if err == nil || !strings.Contains(err.Error(), "does not open under the pending key") {
			t.Fatalf("resealActor err = %v, want the does-not-open refusal", err)
		}
	})
	t.Run("resolve fails", func(t *testing.T) {
		s, _, row := gapCovPendingActorRow(t)
		s.Resolve = gapCovResolveFails
		got, err := s.resealActor(ctx, row, s.Pending())
		if !errors.Is(err, errGapCov) || got.Actor != PendingActor {
			t.Fatalf("resealActor = %q, %v; want the resolve error and the actor still pending", got.Actor, err)
		}
	})
	t.Run("erasure lookup fails", func(t *testing.T) {
		s, _, row := gapCovPendingActorRow(t)
		s.GoneSince = func(context.Context, string, time.Time) (bool, error) { return false, errGapCov }
		got, err := s.resealActor(ctx, row, s.Pending())
		if !errors.Is(err, errGapCov) || got.Actor != PendingActor {
			t.Fatalf("resealActor = %q, %v; want the lookup error and the actor still pending", got.Actor, err)
		}
	})
	t.Run("subject lookup fails for an erased person", func(t *testing.T) {
		s, dir, row := gapCovPendingActorRow(t)
		s.GoneSince = func(context.Context, string, time.Time) (bool, error) { dir.down = true; return true, nil }
		got, err := s.resealActor(ctx, row, s.Pending())
		if err == nil || !strings.Contains(err.Error(), "directory down") || got.Actor != PendingActor {
			t.Fatalf("resealActor = %q, %v; want the directory error and the actor still pending", got.Actor, err)
		}
	})
}

// A read whose key-destruction lookup fails fails, rather than showing a person
// whose key may have been destroyed.
func TestGapCovUnsealFailsWhenTheKeyDestructionLookupFails(t *testing.T) {
	s, _, dir := newFullSealer(t, "alice")
	sealed, _, err := s.Seal(t.Context(), humanEvent("alice", "run.kill"))
	if err != nil {
		t.Fatal(err)
	}
	s.Subjects = gapCovDir{dir}
	if _, err := s.Unseal(t.Context(), []types.AuditEvent{sealed}); !errors.Is(err, errGapCov) {
		t.Fatalf("Unseal err = %v, want the lookup error", err)
	}
}

// A filter name that cannot be resolved fails rather than matching the name alone.
func TestGapCovStoredActorResolveFailure(t *testing.T) {
	s, _, _ := newFullSealer(t, "alice")
	s.Resolve = gapCovResolveFails
	got, ok, err := s.StoredActor(t.Context(), "alice")
	if !errors.Is(err, errGapCov) || ok || got != "" {
		t.Fatalf("StoredActor = %q, %v, %v; want the resolve error and no translation", got, ok, err)
	}
}
